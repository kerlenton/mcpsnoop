package store

import (
	"encoding/json"
	"reflect"
	"strings"
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

// TestUnnegotiatedTasksIsReportedOnlyWhenTheCaptureCanShowIt. SEP-2133 moved
// Tasks out of the core protocol, and 2026-07-28 says that if one party supports
// an extension and the other does not, the supporting party MUST fall back to
// core behaviour or reject the request. mcpsnoop correlated the whole
// unnegotiated lifecycle instead and said nothing, so a feature that silently
// does nothing looked exactly like one that works.
func TestUnnegotiatedTasksIsReportedOnlyWhenTheCaptureCanShowIt(t *testing.T) {
	const tasksCaps = `{"extensions":{"io.modelcontextprotocol/tasks":{}}}`

	for name, tc := range map[string]struct {
		version              string
		client, server       string
		redactedHandshake    bool
		wantRequestWarning   bool
		wantTaskHandleWarned bool
	}{
		"neither side advertised": {
			version: "2026-07-28", client: `{}`, server: `{}`,
			wantRequestWarning: true, wantTaskHandleWarned: true,
		},
		"both advertised": {
			version: "2026-07-28", client: tasksCaps, server: tasksCaps,
		},
		// The side that has to have advertised is the one being asked, so a server
		// that supports Tasks answers tasks/get legitimately while the handle it
		// hands a client that does not is still the violation.
		"only the server advertised": {
			version: "2026-07-28", client: `{}`, server: tasksCaps,
			wantTaskHandleWarned: true,
		},
		"only the client advertised": {
			version: "2026-07-28", client: tasksCaps, server: `{}`,
			wantRequestWarning: true,
		},
		// In 2025-11-25 tasks/get and its siblings are in the core schema and there
		// is no extensions field to negotiate, so driving them is correct there.
		"an earlier revision where tasks are core": {
			version: "2025-11-25", client: `{}`, server: `{}`,
		},
		// A capture that starts after the handshake, and one whose capabilities
		// mcpsnoop's own redaction scrubbed, both cannot show what was negotiated.
		"capabilities never observed": {
			version: "", client: "", server: "",
		},
		// Both sides, since each half of the check reads the other side's
		// declaration and a guard on only one of them would be half a guard.
		"capabilities scrubbed by redaction": {
			version: "2026-07-28", client: `"[REDACTED]"`, server: `"[REDACTED]"`, redactedHandshake: true,
		},
	} {
		t.Run(name, func(t *testing.T) {
			s := New()
			t0 := time.Now()
			seq := uint64(0)
			next := func() uint64 { seq++; return seq }

			if tc.version != "" {
				s.Ingest(req(next(), t0, proxy.ClientToServer, "1", "initialize",
					`{"protocolVersion":"`+tc.version+`","capabilities":`+tc.client+`,"clientInfo":{"name":"c","version":"1"}}`))
				e := resp(next(), t0, proxy.ServerToClient, "1",
					`"result":{"protocolVersion":"`+tc.version+`","capabilities":`+tc.server+`,"serverInfo":{"name":"s","version":"1"}}`)
				e.Redacted = tc.redactedHandshake
				s.Ingest(e)
			}

			s.Ingest(req(next(), t0, proxy.ClientToServer, "2", "tools/call", `{"name":"slow"}`))
			handle := s.Ingest(resp(next(), t0, proxy.ServerToClient, "2",
				`"result":{"resultType":"task","taskId":"t-1","status":"working"}`))
			request := s.Ingest(req(next(), t0, proxy.ClientToServer, "3", "tasks/get", `{"taskId":"t-1"}`))

			if got := strings.Contains(request.Warning, "never advertised"); got != tc.wantRequestWarning {
				t.Errorf("tasks/get warned = %v, want %v (warning %q)", got, tc.wantRequestWarning, request.Warning)
			}
			if got := strings.Contains(handle.Warning, "never advertised"); got != tc.wantTaskHandleWarned {
				t.Errorf("task handle warned = %v, want %v (warning %q)", got, tc.wantTaskHandleWarned, handle.Warning)
			}
			// Whichever way it goes, the message names the side that was short, or a
			// reader has to guess which end to go and look at.
			if tc.wantRequestWarning && !strings.Contains(request.Warning, "the server never advertised") {
				t.Errorf("the request warning does not name the server: %q", request.Warning)
			}
			if tc.wantTaskHandleWarned && !strings.Contains(handle.Warning, "the client never advertised") {
				t.Errorf("the handle warning does not name the client: %q", handle.Warning)
			}
		})
	}
}

// TestAdvertisesSeparatesAbsentFromUnknown. Reading an unparsable declaration as
// "not advertised" is what turns a capture that starts mid-session, or one the
// user asked mcpsnoop to redact, into an accusation against traffic that was
// correct.
func TestAdvertisesSeparatesAbsentFromUnknown(t *testing.T) {
	for name, tc := range map[string]struct {
		raw  string
		want extensionState
	}{
		"never observed":            {``, extensionUnknown},
		"scrubbed whole":            {`"[REDACTED]"`, extensionUnknown},
		"extensions map scrubbed":   {`{"extensions":"[REDACTED]"}`, extensionUnknown},
		"not JSON at all":           {`{oops`, extensionUnknown},
		"declared none":             {`{}`, extensionAbsent},
		"declared other extensions": {`{"extensions":{"io.modelcontextprotocol/ui":{}}}`, extensionAbsent},
		"declared it":               {`{"extensions":{"io.modelcontextprotocol/tasks":{}}}`, extensionAdvertised},
		// The settings object is opaque, so a scrubbed one still declares the id.
		"declared it, settings scrubbed": {`{"extensions":{"io.modelcontextprotocol/tasks":"[REDACTED]"}}`, extensionAdvertised},
	} {
		t.Run(name, func(t *testing.T) {
			if got := advertises(json.RawMessage(tc.raw), tasksExtension); got != tc.want {
				t.Fatalf("advertises(%s) = %v, want %v", tc.raw, got, tc.want)
			}
		})
	}
}

// TestUnnegotiatedTasksCoversTheNotification. A server pushing
// notifications/tasks is reaching for the extension as much as one handing out a
// task handle, and it is the frame a client that never advertised Tasks has no
// idea what to do with.
func TestUnnegotiatedTasksCoversTheNotification(t *testing.T) {
	notify := func(clientCaps string) EventView {
		s := New()
		t0 := time.Now()
		s.Ingest(req(1, t0, proxy.ClientToServer, "1", "initialize",
			`{"protocolVersion":"2026-07-28","capabilities":`+clientCaps+`,"clientInfo":{"name":"c","version":"1"}}`))
		s.Ingest(resp(2, t0, proxy.ServerToClient, "1",
			`"result":{"protocolVersion":"2026-07-28","capabilities":{},"serverInfo":{"name":"s","version":"1"}}`))
		return s.Ingest(proxy.Envelope{
			SessionID: "s1", ServerLabel: "srv", Seq: 3, TS: t0, Direction: proxy.ServerToClient,
			Raw: json.RawMessage(`{"jsonrpc":"2.0","method":"notifications/tasks","params":{"taskId":"t-1","status":"failed"}}`),
		})
	}

	silent := notify(`{"extensions":{"io.modelcontextprotocol/tasks":{}}}`)
	if strings.Contains(silent.Warning, "never advertised") {
		t.Fatalf("a client that advertised Tasks was warned about: %q", silent.Warning)
	}
	warned := notify(`{}`)
	if !strings.Contains(warned.Warning, "the client never advertised") {
		t.Fatalf("notifications/tasks to a client that never advertised Tasks: %q", warned.Warning)
	}
	if !strings.Contains(warned.Warning, "notifications/tasks") {
		t.Fatalf("the warning does not name the frame: %q", warned.Warning)
	}
}
