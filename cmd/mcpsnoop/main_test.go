package main

import (
	"encoding/json"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/kerlenton/mcpsnoop/internal/paths"
	"github.com/kerlenton/mcpsnoop/internal/proxy"
)

func TestLabelFor(t *testing.T) {
	cases := []struct {
		cmd  []string
		want string
	}{
		{[]string{"npx", "-y", "@modelcontextprotocol/server-everything"}, "server-everything"},
		{[]string{"npx", "-y", "@modelcontextprotocol/server-filesystem", "/Users/me/code"}, "server-filesystem"},
		{[]string{"node", "build/index.js"}, "index.js"},
		{[]string{"python3", "-m", "my_mcp_server"}, "my_mcp_server"},
		{[]string{"uvx", "some-mcp"}, "some-mcp"},
		{[]string{"./bin/myserver"}, "myserver"},
		{[]string{"deno", "run", "-A", "server.ts"}, "server.ts"},
	}
	for _, c := range cases {
		if got := labelFor(c.cmd); got != c.want {
			t.Errorf("labelFor(%v) = %q, want %q", c.cmd, got, c.want)
		}
	}
}

func TestRedactKeysFlagParsesCommaSeparatedAndRepeatedValues(t *testing.T) {
	var flag redactKeysFlag
	if err := flag.Set("token, api_key"); err != nil {
		t.Fatal(err)
	}
	if err := flag.Set("password"); err != nil {
		t.Fatal(err)
	}

	cfg := flag.config(false)
	if cfg.CommonSecrets {
		t.Fatal("CommonSecrets = true, want false")
	}
	if got, want := cfg.Keys, []string{"token", "api_key", "password"}; !slices.Equal(got, want) {
		t.Fatalf("keys = %v, want %v", got, want)
	}
	if got := flag.String(); got != "token,api_key,password" {
		t.Fatalf("String() = %q, want token,api_key,password", got)
	}
}

func TestRedactKeysFlagConfigEnablesCommonSecretsPreset(t *testing.T) {
	var flag redactKeysFlag
	if err := flag.Set("custom_secret"); err != nil {
		t.Fatal(err)
	}

	cfg := flag.config(true)
	if !cfg.CommonSecrets {
		t.Fatal("CommonSecrets = false, want true")
	}
	if got, want := cfg.Keys, []string{"custom_secret"}; !slices.Equal(got, want) {
		t.Fatalf("keys = %v, want %v", got, want)
	}
}

func TestTraceSinkRedactsFileAndLiveSocket(t *testing.T) {
	stateDir := filepath.Join(os.TempDir(), fmt.Sprintf("mcpsnoop-test-%d", os.Getpid()))
	if err := os.RemoveAll(stateDir); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(stateDir) })
	t.Setenv("MCPSNOOP_HOME", stateDir)

	ln, err := net.Listen("unix", paths.SocketPath())
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()

	live := make(chan proxy.Envelope, 1)
	acceptErr := make(chan error, 1)
	go func() {
		conn, err := ln.Accept()
		if err != nil {
			acceptErr <- err
			return
		}
		defer conn.Close()

		var env proxy.Envelope
		if err := json.NewDecoder(conn).Decode(&env); err != nil {
			acceptErr <- err
			return
		}
		live <- env
	}()

	traceFile := filepath.Join(t.TempDir(), "session.jsonl")
	sink := traceSink("s1", traceFile, false, proxy.RedactConfig{Keys: []string{"token"}})
	closed := false
	t.Cleanup(func() {
		if !closed {
			_ = sink.Close()
		}
	})

	sink.Emit(proxy.Envelope{
		SessionID: "s1",
		Direction: proxy.ClientToServer,
		Raw:       json.RawMessage(`{"params":{"token":"secret","keep":"visible"}}`),
	})

	select {
	case got := <-live:
		assertRawTokenRedacted(t, got.Raw)
	case err := <-acceptErr:
		t.Fatal(err)
	case <-time.After(3 * time.Second):
		t.Fatal("timed out waiting for redacted live socket envelope")
	}

	if err := sink.Close(); err != nil {
		t.Fatal(err)
	}
	closed = true

	data, err := os.ReadFile(traceFile)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(data), "secret") {
		t.Fatalf("trace contains unredacted secret: %s", data)
	}
	var got proxy.Envelope
	if err := json.Unmarshal(data, &got); err != nil {
		t.Fatalf("trace is invalid JSONL envelope: %v", err)
	}
	assertRawTokenRedacted(t, got.Raw)
}

func assertRawTokenRedacted(t *testing.T, raw json.RawMessage) {
	t.Helper()
	if strings.Contains(string(raw), "secret") {
		t.Fatalf("raw payload contains unredacted secret: %s", raw)
	}
	var payload map[string]any
	if err := json.Unmarshal(raw, &payload); err != nil {
		t.Fatalf("raw payload is invalid JSON: %v", err)
	}
	params := payload["params"].(map[string]any)
	if params["token"] != "[REDACTED]" {
		t.Fatalf("token = %v, want redacted", params["token"])
	}
	if params["keep"] != "visible" {
		t.Fatalf("keep = %v, want visible", params["keep"])
	}
}
