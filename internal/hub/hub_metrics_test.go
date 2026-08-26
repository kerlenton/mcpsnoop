package hub_test

import (
	"context"
	"encoding/json"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/kerlenton/mcpsnoop/internal/hub"
	"github.com/kerlenton/mcpsnoop/internal/metrics"
	"github.com/kerlenton/mcpsnoop/internal/proxy"
)

func writeMetricMetaLog(t *testing.T, dir, session string, command []string) {
	t.Helper()
	meta, err := json.Marshal(proxy.SessionMeta{Command: command, CWD: "/srv"})
	if err != nil {
		t.Fatal(err)
	}
	file, err := os.Create(filepath.Join(dir, session+".jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	if err := json.NewEncoder(file).Encode(proxy.Envelope{
		SessionID: session, ServerLabel: "same", Seq: 1, TS: time.Unix(1_700_000_000, 0),
		Direction: proxy.DirectionMeta, Transport: proxy.TransportStdio, Raw: meta,
	}); err != nil {
		file.Close()
		t.Fatal(err)
	}
	if err := file.Close(); err != nil {
		t.Fatal(err)
	}
}

func TestBackfillPrimesMetricsIdentityForLiveReconnect(t *testing.T) {
	sessionsDir := t.TempDir()
	writeMetricMetaLog(t, sessionsDir, "s0", []string{"node", "alpha.js"})
	writeMetricMetaLog(t, sessionsDir, "s1", []string{"node", "beta.js"})

	socket := filepath.Join(os.TempDir(), fmt.Sprintf("mcpsnoop-metrics-%d.sock", os.Getpid()))
	defer os.Remove(socket)
	probe, err := net.Listen("unix", socket)
	if err != nil {
		t.Skipf("unix sockets unavailable: %v", err)
	}
	probe.Close()
	os.Remove(socket)
	collector := metrics.New()
	h := hub.NewWithOptions(socket, sessionsDir, func(proxy.Envelope) {}, hub.Options{
		BackfillLimit:      0,
		OnBackfillEnvelope: collector.Prime,
		OnLive:             collector.Observe,
	})

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	hubErr := make(chan error, 1)
	go func() { hubErr <- h.Run(ctx) }()
	for range 50 {
		if _, err := os.Stat(socket); err == nil {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	if _, err := os.Stat(socket); err != nil {
		cancel()
		t.Fatalf("hub socket did not start: %v", err)
	}

	sink := proxy.NewSocketSink(socket, 0)
	defer sink.Close()
	for i := range 2 {
		session := fmt.Sprintf("s%d", i)
		at := time.Unix(1_700_000_000+int64(i), 0)
		sink.Emit(proxy.Envelope{
			SessionID: session, ServerLabel: "same", Seq: 2, TS: at,
			Direction: proxy.ClientToServer, Transport: proxy.TransportStdio,
			Raw: json.RawMessage(`{"jsonrpc":"2.0","id":"1","method":"tools/call","params":{"name":"echo"}}`),
		})
		sink.Emit(proxy.Envelope{
			SessionID: session, ServerLabel: "same", Seq: 3, TS: at.Add(time.Millisecond),
			Direction: proxy.ServerToClient, Transport: proxy.TransportStdio,
			Raw: json.RawMessage(`{"jsonrpc":"2.0","id":"1","result":{"content":[]}}`),
		})
	}

	var out strings.Builder
	for range 100 {
		out.Reset()
		if err := collector.Write(&out); err != nil {
			t.Fatal(err)
		}
		if strings.Contains(out.String(), `mcpsnoop_tool_calls_total{server="same"`) {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	if err := sink.Close(); err != nil {
		t.Fatal(err)
	}
	cancel()
	if err := <-hubErr; err != nil {
		t.Fatal(err)
	}
	if got := strings.Count(out.String(), `mcpsnoop_tool_calls_total{server="same"`); got != 2 {
		t.Fatalf("backfill did not prime distinct reconnect identities, got %d series:\n%s", got, out.String())
	}
}
