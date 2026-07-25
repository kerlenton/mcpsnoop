package store

import (
	"encoding/json"
	"reflect"
	"testing"
	"time"

	"github.com/kerlenton/mcpsnoop/internal/proxy"
)

const (
	extTasks = "io.modelcontextprotocol/tasks"
	extApps  = "io.modelcontextprotocol/apps"
)

// handshake feeds one legacy initialize exchange carrying the given raw
// capability objects, the shortest path to a populated CapsView.
func handshake(s *Store, sessionID, clientCaps, serverCaps string) {
	now := time.Now()
	s.Ingest(proxy.Envelope{
		SessionID: sessionID, ServerLabel: "srv", Seq: 1, TS: now, Direction: proxy.ClientToServer,
		Raw: json.RawMessage(`{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"2026-07-28","clientInfo":{"name":"cli"},"capabilities":` + clientCaps + `}}`),
	})
	s.Ingest(proxy.Envelope{
		SessionID: sessionID, ServerLabel: "srv", Seq: 2, TS: now, Direction: proxy.ServerToClient,
		Raw: json.RawMessage(`{"jsonrpc":"2.0","id":1,"result":{"protocolVersion":"2026-07-28","capabilities":` + serverCaps + `,"serverInfo":{"name":"srv"}}}`),
	})
}

// TestCapabilitiesExtensionsAgreement locks the whole point of the section:
// agreement, not presence. An extension only one side advertised must survive
// into the view marked one-sided, because reporting it as supported is the
// error that leaves someone puzzled about a feature doing nothing.
func TestCapabilitiesExtensionsAgreement(t *testing.T) {
	cases := []struct {
		name       string
		clientCaps string
		serverCaps string
		want       []ExtensionView
	}{
		{
			name:       "neither side advertises any",
			clientCaps: `{"elicitation":{}}`,
			serverCaps: `{"tools":{}}`,
			want:       nil,
		},
		{
			name:       "both sides advertise the same extension",
			clientCaps: `{"extensions":{"` + extTasks + `":{}}}`,
			serverCaps: `{"extensions":{"` + extTasks + `":{}}}`,
			want:       []ExtensionView{{ID: extTasks, Client: true, Server: true}},
		},
		{
			name:       "client only",
			clientCaps: `{"extensions":{"` + extTasks + `":{}}}`,
			serverCaps: `{"tools":{}}`,
			want:       []ExtensionView{{ID: extTasks, Client: true}},
		},
		{
			name:       "server only",
			clientCaps: `{"elicitation":{}}`,
			serverCaps: `{"extensions":{"` + extTasks + `":{}}}`,
			want:       []ExtensionView{{ID: extTasks, Server: true}},
		},
		{
			name:       "overlapping sets merge and sort by id",
			clientCaps: `{"extensions":{"` + extTasks + `":{},"` + extApps + `":{}}}`,
			serverCaps: `{"extensions":{"` + extTasks + `":{}}}`,
			want: []ExtensionView{
				{ID: extApps, Client: true},
				{ID: extTasks, Client: true, Server: true},
			},
		},
		{
			name:       "an empty extensions map is not an extension",
			clientCaps: `{"extensions":{}}`,
			serverCaps: `{"extensions":{}}`,
			want:       nil,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			s := New()
			handshake(s, "s1", tc.clientCaps, tc.serverCaps)
			caps, ok := s.Capabilities("s1")
			if !ok {
				t.Fatal("capabilities should be set after a handshake")
			}
			if !reflect.DeepEqual(caps.Extensions, tc.want) {
				t.Fatalf("extensions = %+v, want %+v", caps.Extensions, tc.want)
			}
		})
	}
}

// TestCapabilitiesExtensionsFromStatelessSources checks the parse is source
// agnostic. 2026-07-28 removed the initialize handshake, so the only paths that
// matter in practice are the client's _meta and a server/discover response.
func TestCapabilitiesExtensionsFromStatelessSources(t *testing.T) {
	s := New()
	now := time.Now()
	s.Ingest(proxy.Envelope{
		SessionID: "s1", ServerLabel: "srv", Seq: 1, TS: now, Direction: proxy.ClientToServer,
		Raw: json.RawMessage(`{"jsonrpc":"2.0","id":1,"method":"server/discover","params":{"_meta":{"io.modelcontextprotocol/protocolVersion":"2026-07-28","io.modelcontextprotocol/clientInfo":{"name":"cli"},"io.modelcontextprotocol/clientCapabilities":{"extensions":{"` + extTasks + `":{}}}}}}`),
	})
	s.Ingest(proxy.Envelope{
		SessionID: "s1", ServerLabel: "srv", Seq: 2, TS: now, Direction: proxy.ServerToClient,
		Raw: json.RawMessage(`{"jsonrpc":"2.0","id":1,"result":{"supportedVersions":["2026-07-28"],"capabilities":{"tools":{},"extensions":{"` + extTasks + `":{}}}}}`),
	})

	caps, ok := s.Capabilities("s1")
	if !ok {
		t.Fatal("capabilities should be set from the stateless paths")
	}
	want := []ExtensionView{{ID: extTasks, Client: true, Server: true}}
	if !reflect.DeepEqual(caps.Extensions, want) {
		t.Fatalf("extensions = %+v, want %+v", caps.Extensions, want)
	}
}

// TestExtensionIDsIgnoresUnreadableDeclarations checks the parse stays quiet on
// input it cannot interpret. Reporting an extension we did not really see would
// be worse than reporting none, since the whole screen is an evidence claim.
func TestExtensionIDsIgnoresUnreadableDeclarations(t *testing.T) {
	for _, raw := range []string{``, `null`, `{}`, `{"extensions":null}`, `{"extensions":[]}`, `{"extensions":"tasks"}`, `not json`} {
		if got := extensionIDs(json.RawMessage(raw)); got != nil {
			t.Fatalf("extensionIDs(%q) = %v, want nil", raw, got)
		}
	}
}
