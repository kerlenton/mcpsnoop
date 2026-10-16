package replay

import "testing"

func mustParse(t *testing.T, raw string) *node {
	t.Helper()
	n, err := parseNode([]byte(raw))
	if err != nil {
		t.Fatal(err)
	}
	return n
}

// carry learns from a recorded and a replayed result, then applies what it
// learned to a client frame and returns it.
func carry(t *testing.T, recorded, replayed, frame string) string {
	t.Helper()
	c := newCarried()
	c.learn(mustParse(t, recorded), mustParse(t, replayed))
	n := mustParse(t, frame)
	c.apply(n)
	return string(n.encode())
}

// TestCarryNeverRenamesATool is the failure a real Claude Code recording found
// against a server that had gained tools since. The new ones listed first, the
// lists were compared position by position, and every later call went to
// whichever tool now sat where the recorded one had. A name is what the client
// asks for, never something the server issued for it to echo, and list items
// pair by name.
func TestCarryNeverRenamesATool(t *testing.T) {
	recorded := `{"tools":[{"name":"confirm_action","description":"Ask first"},{"name":"slow_task"}]}`
	replayed := `{"tools":[{"name":"add_note"},{"name":"clear_notes"},{"name":"confirm_action","description":"Ask first"},{"name":"slow_task"}]}`
	call := `{"jsonrpc":"2.0","id":5,"method":"tools/call","params":{"name":"confirm_action","arguments":{"action":"delete"}}}`
	if got := carry(t, recorded, replayed, call); got != call {
		t.Fatalf("the call changed:\n got %s\nwant %s", got, call)
	}

	// The same holds when a tool really was renamed. The replay asks for the
	// recorded name and lets the server say it is gone, which is the regression
	// a replay exists to show.
	renamed := `{"tools":[{"name":"confirm"},{"name":"slow_task"}]}`
	if got := carry(t, recorded, renamed, call); got != call {
		t.Fatalf("a renamed tool was followed:\n got %s", got)
	}
}

// TestCarryLeavesDefinitionsAlone. Descriptions, titles, schemas, enum-like
// statuses and metadata change between versions of a server without being
// anything a client sends back, so nothing in them is learned.
func TestCarryLeavesDefinitionsAlone(t *testing.T) {
	recorded := `{"tools":[{"name":"search","title":"Search v2024-01","description":"Index built 2024-01-02","inputSchema":{"type":"object","properties":{"q":{"default":"2024-01-02"}}}}],"_meta":{"build":"2024-01-02"}}`
	replayed := `{"tools":[{"name":"search","title":"Search v2026-10","description":"Index built 2026-10-16","inputSchema":{"type":"object","properties":{"q":{"default":"2026-10-16"}}}}],"_meta":{"build":"2026-10-16"}}`
	call := `{"jsonrpc":"2.0","id":2,"method":"tools/call","params":{"name":"search","arguments":{"q":"2024-01-02"}}}`
	if got := carry(t, recorded, replayed, call); got != call {
		t.Fatalf("a value from a definition was carried:\n got %s", got)
	}
}

// TestCarryFollowsWhatTheServerIssued is the other half. Ids, cursors, states
// and identifiers in text move with the server, by name or by position in a
// list of equal length.
func TestCarryFollowsWhatTheServerIssued(t *testing.T) {
	for _, tc := range []struct{ name, recorded, replayed, frame, want string }{
		{"a cursor",
			`{"tools":[],"nextCursor":"c1"}`, `{"tools":[],"nextCursor":"c2"}`,
			`{"jsonrpc":"2.0","id":3,"method":"tools/list","params":{"cursor":"c1"}}`,
			`{"jsonrpc":"2.0","id":3,"method":"tools/list","params":{"cursor":"c2"}}`},
		{"a numeric id",
			`{"structuredContent":{"issueId":41}}`, `{"structuredContent":{"issueId":97}}`,
			`{"jsonrpc":"2.0","id":4,"method":"tools/call","params":{"name":"close","arguments":{"issueId":41,"limit":41}}}`,
			`{"jsonrpc":"2.0","id":4,"method":"tools/call","params":{"name":"close","arguments":{"issueId":97,"limit":41}}}`},
		{"an id in text",
			`{"content":[{"type":"text","text":"Created note n-12."}]}`, `{"content":[{"type":"text","text":"Created note n-88."}]}`,
			`{"jsonrpc":"2.0","id":5,"method":"tools/call","params":{"name":"get","arguments":{"id":"n-12","note":"see n-12 please"}}}`,
			`{"jsonrpc":"2.0","id":5,"method":"tools/call","params":{"name":"get","arguments":{"id":"n-88","note":"see n-88 please"}}}`},
		{"ids in a list of equal length",
			`{"structuredContent":{"hits":[{"id":"r-1"},{"id":"r-2"}]}}`, `{"structuredContent":{"hits":[{"id":"r-7"},{"id":"r-8"}]}}`,
			`{"jsonrpc":"2.0","id":6,"method":"tools/call","params":{"name":"open","arguments":{"id":"r-2"}}}`,
			`{"jsonrpc":"2.0","id":6,"method":"tools/call","params":{"name":"open","arguments":{"id":"r-8"}}}`},
		{"a resource uri, found by its name",
			`{"resources":[{"name":"report","uri":"mem://r/1a2b"}]}`, `{"resources":[{"name":"other","uri":"mem://o"},{"name":"report","uri":"mem://r/9f8e"}]}`,
			`{"jsonrpc":"2.0","id":7,"method":"resources/read","params":{"uri":"mem://r/1a2b"}}`,
			`{"jsonrpc":"2.0","id":7,"method":"resources/read","params":{"uri":"mem://r/9f8e"}}`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := carry(t, tc.recorded, tc.replayed, tc.frame); got != tc.want {
				t.Fatalf("\n got %s\nwant %s", got, tc.want)
			}
		})
	}
}

// TestCarryKeepsTheFramesOwnAddress. The id, method and jsonrpc of the frame
// being sent are the replay's to set, never something to carry into.
func TestCarryKeepsTheFramesOwnAddress(t *testing.T) {
	got := carry(t, `{"structuredContent":{"id":"tools/call"}}`, `{"structuredContent":{"id":"tools/list"}}`,
		`{"jsonrpc":"2.0","id":9,"method":"tools/call","params":{"name":"x"}}`)
	if got != `{"jsonrpc":"2.0","id":9,"method":"tools/call","params":{"name":"x"}}` {
		t.Fatalf("got %s", got)
	}
}
