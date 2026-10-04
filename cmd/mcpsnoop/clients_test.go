package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/kerlenton/mcpsnoop/internal/clientprofile"
	"github.com/kerlenton/mcpsnoop/internal/proxy"
)

func executeClients(t *testing.T, args []string) (int, string, string) {
	t.Helper()
	cmd := newClientsCmd()
	cmd.SetArgs(args)
	var stdout, stderr bytes.Buffer
	cmd.SetOut(&stdout)
	cmd.SetErr(&stderr)
	cmd.SilenceErrors = true
	cmd.SilenceUsage = true
	err := cmd.Execute()
	if err == nil {
		return 0, stdout.String(), stderr.String()
	}
	var code exitCode
	if !errors.As(err, &code) {
		t.Fatalf("unexpected command error: %v", err)
	}
	return int(code), stdout.String(), stderr.String()
}

// clientSession builds one session's envelopes from direction and frame pairs.
func clientSession(sessionID string, frames ...[2]string) []proxy.Envelope {
	t0 := time.Date(2026, 9, 1, 12, 0, 0, 0, time.UTC)
	out := make([]proxy.Envelope, 0, len(frames))
	for i, f := range frames {
		out = append(out, proxy.Envelope{
			SessionID: sessionID, ServerLabel: "ref", Seq: uint64(i + 1),
			TS: t0.Add(time.Duration(i) * time.Second), Direction: proxy.Direction(f[0]),
			Transport: proxy.TransportStdio, Raw: json.RawMessage(f[1]),
		})
	}
	return out
}

const (
	c2s = string(proxy.ClientToServer)
	s2c = string(proxy.ServerToClient)
)

func modernClientFrames(name string) [][2]string {
	meta := `"_meta":{"io.modelcontextprotocol/protocolVersion":"2026-07-28",` +
		`"io.modelcontextprotocol/clientInfo":{"name":` + quoteJSON(name) + `,"version":"1.4.0"},` +
		`"io.modelcontextprotocol/clientCapabilities":{"elicitation":{"form":{}}},` +
		`"traceparent":"00-4bf92f3577b34da6a3ce929d0e0e4736-00f067aa0ba902b7-01"}`
	return [][2]string{
		{c2s, `{"jsonrpc":"2.0","id":1,"method":"server/discover","params":{` + meta + `}}`},
		{s2c, `{"jsonrpc":"2.0","id":1,"result":{"resultType":"complete","supportedVersions":["2026-07-28"],"capabilities":{"tools":{}},"serverInfo":{"name":"ref","version":"1"}}}`},
		{c2s, `{"jsonrpc":"2.0","id":2,"method":"tools/list","params":{` + meta + `}}`},
		{s2c, `{"jsonrpc":"2.0","id":2,"result":{"resultType":"complete","tools":[],"ttlMs":60000,"cacheScope":"private"}}`},
	}
}

func legacyClientFrames() [][2]string {
	return [][2]string{
		{c2s, `{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"2025-11-25","clientInfo":{"name":"oldie","version":"0.9"},"capabilities":{"roots":{}}}}`},
		{s2c, `{"jsonrpc":"2.0","id":1,"result":{"protocolVersion":"2025-11-25","capabilities":{},"serverInfo":{"name":"ref","version":"1"}}}`},
		{c2s, `{"jsonrpc":"2.0","id":2,"method":"logging/setLevel","params":{"level":"info"}}`},
	}
}

func quoteJSON(s string) string {
	b, _ := json.Marshal(s)
	return string(b)
}

func TestClientsWalksTheSessionsDirectory(t *testing.T) {
	t.Setenv("MCPSNOOP_HOME", t.TempDir())
	writeSessionLog(t, "modern.jsonl", clientSession("m", modernClientFrames("probe")...)...)
	writeSessionLog(t, "legacy.jsonl", clientSession("l", legacyClientFrames()...)...)

	code, stdout, stderr := executeClients(t, nil)
	if code != 0 || stderr != "" {
		t.Fatalf("code %d stderr %q", code, stderr)
	}
	for _, want := range []string{
		"read 2 logs of 2 in ", "2 sessions with client traffic",
		"\noldie 0.9\n", "\nprobe 1.4.0\n",
		"opens with               initialize\n",
		"opens with               server/discover, then stateless\n",
		"deprecated in use        logging ×1\n",
		"trace context            2 of 2 requests\n",
	} {
		if !strings.Contains(stdout, want) {
			t.Errorf("output lacks %q:\n%s", want, stdout)
		}
	}
}

func TestClientsJSONCarriesEveryProfile(t *testing.T) {
	t.Setenv("MCPSNOOP_HOME", t.TempDir())
	writeSessionLog(t, "modern.jsonl", clientSession("m", modernClientFrames("probe")...)...)
	writeSessionLog(t, "legacy.jsonl", clientSession("l", legacyClientFrames()...)...)

	code, stdout, _ := executeClients(t, []string{"--format", "json"})
	if code != 0 {
		t.Fatalf("code %d", code)
	}
	var rep struct {
		Sessions int                     `json:"sessions"`
		Clients  []clientprofile.Profile `json:"clients"`
	}
	if err := json.Unmarshal([]byte(stdout), &rep); err != nil {
		t.Fatalf("not json: %v\n%s", err, stdout)
	}
	if rep.Sessions != 2 || len(rep.Clients) != 2 || rep.Clients[0].Client != "oldie" || rep.Clients[1].Client != "probe" {
		t.Fatalf("report = %+v", rep)
	}
}

func TestClientsReadsNamedLogsAsMarkdown(t *testing.T) {
	t.Setenv("MCPSNOOP_HOME", t.TempDir())
	dir := t.TempDir()
	a := writeLogAt(t, filepath.Join(dir, "a.jsonl"), clientSession("m", modernClientFrames("probe")...))
	b := writeLogAt(t, filepath.Join(dir, "b.jsonl"), clientSession("l", legacyClientFrames()...))

	code, stdout, stderr := executeClients(t, []string{"--format", "markdown", a, b})
	if code != 0 || stderr != "" {
		t.Fatalf("code %d stderr %q", code, stderr)
	}
	for _, want := range []string{
		"|  | oldie 0.9 | probe 1.4.0 |\n|---|---|---|\n",
		"| opens with | initialize | server/discover, then stateless |",
		"Read by `mcpsnoop clients` from 2 sessions.",
	} {
		if !strings.Contains(stdout, want) {
			t.Errorf("markdown lacks %q:\n%s", want, stdout)
		}
	}
}

func TestClientsRefusesWhatItCannotHonour(t *testing.T) {
	t.Setenv("MCPSNOOP_HOME", t.TempDir())
	dir := t.TempDir()
	a := writeLogAt(t, filepath.Join(dir, "a.jsonl"), clientSession("m", modernClientFrames("probe")...))
	for _, c := range []struct {
		name string
		args []string
		code int
	}{
		{"unknown format", []string{"--format", "yaml"}, 2},
		{"since with named logs", []string{"--since", "7d", a}, 2},
		{"limit with named logs", []string{"--limit", "3", a}, 2},
		{"negative limit", []string{"--limit", "-1"}, 2},
		{"blank label", []string{"--label", ""}, 2},
		{"missing named log", []string{filepath.Join(dir, "nope.jsonl")}, 1},
	} {
		t.Run(c.name, func(t *testing.T) {
			code, _, stderr := executeClients(t, c.args)
			if code != c.code || !strings.HasPrefix(stderr, "mcpsnoop clients:") {
				t.Fatalf("code %d, stderr %q, want code %d", code, stderr, c.code)
			}
		})
	}
}

// TestClientsCannotBeDrivenFromTheWire covers the one value every block starts
// with. A client chooses its own name, and the name reaches a terminal and a
// rendered page, so an escape sequence must not run and a pipe must not open a
// cell that is not there.
func TestClientsCannotBeDrivenFromTheWire(t *testing.T) {
	t.Setenv("MCPSNOOP_HOME", t.TempDir())
	writeSessionLog(t, "evil.jsonl", clientSession("e", modernClientFrames("evil\x1b]52;c;cHduZWQ=\x07 | forged")...)...)

	_, text, _ := executeClients(t, nil)
	if strings.ContainsRune(text, 0x1b) || strings.ContainsRune(text, 0x07) {
		t.Fatalf("a control sequence reached the terminal output:\n%q", text)
	}
	if !strings.Contains(text, `\x1b]52`) {
		t.Fatalf("the name was dropped rather than quoted:\n%s", text)
	}
	_, md, _ := executeClients(t, []string{"--format", "markdown"})
	if strings.ContainsRune(md, 0x1b) {
		t.Fatalf("a control sequence reached the markdown:\n%q", md)
	}
	header := strings.SplitN(md, "\n", 2)[0]
	if strings.Count(header, "|")-strings.Count(header, `\|`) != 3 {
		t.Fatalf("a pipe in the name opened a column that is not there: %q", header)
	}
}

func TestClientsSaysSoWhenThereIsNoClientTraffic(t *testing.T) {
	t.Setenv("MCPSNOOP_HOME", t.TempDir())
	writeSessionLog(t, "quiet.jsonl", clientSession("q", [2]string{s2c, `{"jsonrpc":"2.0","method":"notifications/message","params":{"level":"info","data":"hi"}}`})...)

	code, stdout, _ := executeClients(t, nil)
	if code != 0 || !strings.HasPrefix(stdout, "no client traffic found: read 1 log of 1 in ") {
		t.Fatalf("code %d, output %q", code, stdout)
	}
}

func writeLogAt(t *testing.T, path string, envs []proxy.Envelope) string {
	t.Helper()
	var buf bytes.Buffer
	for _, env := range envs {
		b, err := json.Marshal(env)
		if err != nil {
			t.Fatal(err)
		}
		buf.Write(append(b, '\n'))
	}
	if err := os.WriteFile(path, buf.Bytes(), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

// TestClientsQuotesWireValuesInsideCells is the same hazard one level down. A
// revision and an extension id are as much the client's choice as its name, and
// both are printed as cells.
func TestClientsQuotesWireValuesInsideCells(t *testing.T) {
	t.Setenv("MCPSNOOP_HOME", t.TempDir())
	meta := `"_meta":{"io.modelcontextprotocol/protocolVersion":"2026-07-28\u001b[2J",` +
		`"io.modelcontextprotocol/clientInfo":{"name":"probe","version":"1"},` +
		`"io.modelcontextprotocol/clientCapabilities":{"extensions":{"x\u001b]0;pwned\u0007":{}}}}`
	writeSessionLog(t, "cells.jsonl", clientSession("c",
		[2]string{c2s, `{"jsonrpc":"2.0","id":1,"method":"tools/list","params":{` + meta + `}}`})...)

	_, text, _ := executeClients(t, nil)
	if strings.ContainsRune(text, 0x1b) || strings.ContainsRune(text, 0x07) {
		t.Fatalf("a control sequence in a cell reached the terminal output:\n%q", text)
	}
	if !strings.Contains(text, `2026-07-28\x1b[2J`) {
		t.Fatalf("the revision was dropped rather than quoted:\n%s", text)
	}
}

func TestClientsLabelNarrowsNamedLogs(t *testing.T) {
	t.Setenv("MCPSNOOP_HOME", t.TempDir())
	dir := t.TempDir()
	keep := clientSession("m", modernClientFrames("probe")...)
	drop := clientSession("l", legacyClientFrames()...)
	for i := range drop {
		drop[i].ServerLabel = "other"
	}
	a := writeLogAt(t, filepath.Join(dir, "a.jsonl"), keep)
	b := writeLogAt(t, filepath.Join(dir, "b.jsonl"), drop)

	code, stdout, _ := executeClients(t, []string{"--label", "ref", a, b})
	if code != 0 || !strings.Contains(stdout, "probe 1.4.0") || strings.Contains(stdout, "oldie") {
		t.Fatalf("code %d, --label ref must keep probe and drop oldie:\n%s", code, stdout)
	}
}
