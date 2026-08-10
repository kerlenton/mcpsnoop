package tui

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/kerlenton/mcpsnoop/internal/store"
)

// TestMain doubles the current test binary as a portable stub editor. The
// parent gives it only the temporary JSON path, so no shell or platform-specific
// script is needed to exercise the real exec.Cmd boundary.
func TestMain(m *testing.M) {
	if os.Getenv("MCPSNOOP_EDITOR_HELPER") == "1" {
		path := os.Args[len(os.Args)-1]
		var err error
		switch os.Getenv("MCPSNOOP_EDITOR_BEHAVIOR") {
		case "unchanged":
		case "changed":
			err = os.WriteFile(path, []byte("{\n  \"name\": \"echo\",\n  \"arguments\": {\"text\": \"new\"}\n}\n"), 0o600)
		case "invalid":
			err = os.WriteFile(path, []byte("{not json\n"), 0o600)
		case "non-object":
			err = os.WriteFile(path, []byte("[1, 2, 3]\n"), 0o600)
		case "multiple":
			err = os.WriteFile(path, []byte("{} {}\n"), 0o600)
		case "empty":
			err = os.WriteFile(path, nil, 0o600)
		case "exit":
			os.Exit(7)
		default:
			os.Exit(8)
		}
		if err != nil {
			os.Exit(9)
		}
		os.Exit(0)
	}
	os.Exit(m.Run())
}

func TestReplayEditorOutcomes(t *testing.T) {
	original := json.RawMessage(`{"name":"echo","arguments":{"text":"old"}}`)
	call := store.CallView{Method: "tools/call", Params: original}

	for _, tc := range []struct {
		name     string
		behavior string
		wantErr  string
		wantText string
	}{
		{name: "changed", behavior: "changed", wantText: `"new"`},
		{name: "unchanged", behavior: "unchanged", wantText: `"old"`},
		{name: "invalid", behavior: "invalid", wantErr: "not valid JSON"},
		{name: "non object", behavior: "non-object", wantErr: "must be a JSON object"},
		{name: "multiple values", behavior: "multiple", wantErr: "more than one JSON value"},
		{name: "empty", behavior: "empty", wantErr: "empty"},
		{name: "nonzero exit", behavior: "exit", wantErr: "editor failed"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv("TMPDIR", t.TempDir())
			t.Setenv("VISUAL", "")
			t.Setenv("EDITOR", os.Args[0])
			t.Setenv("MCPSNOOP_EDITOR_HELPER", "1")
			t.Setenv("MCPSNOOP_EDITOR_BEHAVIOR", tc.behavior)

			cmd, done, err := prepareReplayEditor(call, original)
			if err != nil {
				t.Fatalf("prepareReplayEditor: %v", err)
			}
			path := cmd.Args[len(cmd.Args)-1]
			info, err := os.Stat(path)
			if err != nil {
				t.Fatalf("editor buffer: %v", err)
			}
			if info.Mode().Perm() != 0o600 {
				t.Fatalf("editor buffer mode = %o, want 600", info.Mode().Perm())
			}
			seeded, err := os.ReadFile(path)
			if err != nil {
				t.Fatal(err)
			}
			if !strings.Contains(string(seeded), "\n  \"arguments\"") {
				t.Fatalf("editor buffer is not pretty JSON:\n%s", seeded)
			}

			msg, ok := done(cmd.Run()).(replayEditDoneMsg)
			if !ok {
				t.Fatal("editor callback returned the wrong message type")
			}
			if _, err := os.Stat(path); !os.IsNotExist(err) {
				t.Fatalf("editor buffer was not removed, stat err=%v", err)
			}
			if tc.wantErr != "" {
				if msg.err == nil || !strings.Contains(msg.err.Error(), tc.wantErr) {
					t.Fatalf("error = %v, want substring %q", msg.err, tc.wantErr)
				}
				return
			}
			if msg.err != nil {
				t.Fatalf("editor callback: %v", msg.err)
			}
			if !strings.Contains(string(msg.call.Params), tc.wantText) {
				t.Fatalf("saved params = %s, want %s", msg.call.Params, tc.wantText)
			}
			if string(msg.captured) != string(original) {
				t.Fatalf("captured params changed: %s", msg.captured)
			}
		})
	}
}

func TestReplayEditorSelectionAndGuards(t *testing.T) {
	call := store.CallView{Method: "tools/call", Params: json.RawMessage(`{"name":"echo"}`)}

	t.Run("VISUAL takes precedence", func(t *testing.T) {
		t.Setenv("TMPDIR", t.TempDir())
		t.Setenv("VISUAL", os.Args[0])
		t.Setenv("EDITOR", filepath.Join(t.TempDir(), "must-not-run"))
		t.Setenv("MCPSNOOP_EDITOR_HELPER", "1")
		t.Setenv("MCPSNOOP_EDITOR_BEHAVIOR", "unchanged")
		cmd, done, err := prepareReplayEditor(call, call.Params)
		if err != nil {
			t.Fatal(err)
		}
		msg := done(cmd.Run()).(replayEditDoneMsg)
		if msg.err != nil {
			t.Fatalf("VISUAL was not used: %v", msg.err)
		}
	})

	t.Run("EDITOR fallback", func(t *testing.T) {
		t.Setenv("TMPDIR", t.TempDir())
		t.Setenv("VISUAL", "")
		t.Setenv("EDITOR", os.Args[0])
		t.Setenv("MCPSNOOP_EDITOR_HELPER", "1")
		t.Setenv("MCPSNOOP_EDITOR_BEHAVIOR", "unchanged")
		cmd, done, err := prepareReplayEditor(call, call.Params)
		if err != nil {
			t.Fatal(err)
		}
		if msg := done(cmd.Run()).(replayEditDoneMsg); msg.err != nil {
			t.Fatalf("EDITOR fallback failed: %v", msg.err)
		}
	})

	t.Run("missing editor", func(t *testing.T) {
		t.Setenv("VISUAL", "")
		t.Setenv("EDITOR", "")
		if _, _, err := prepareReplayEditor(call, call.Params); err == nil || !strings.Contains(err.Error(), "$VISUAL or $EDITOR") {
			t.Fatalf("missing editor error = %v", err)
		}
	})

	t.Run("captured non-object", func(t *testing.T) {
		t.Setenv("EDITOR", os.Args[0])
		bad := call
		bad.Params = json.RawMessage(`[1]`)
		if _, _, err := prepareReplayEditor(bad, bad.Params); err == nil || !strings.Contains(err.Error(), "JSON object") {
			t.Fatalf("non-object captured params error = %v", err)
		}
	})
}

func TestReplayParamsEquivalentIgnoresFormattingAndObjectOrder(t *testing.T) {
	a := json.RawMessage(`{"b":[2,3],"a":1}`)
	b := json.RawMessage("{\n  \"a\": 1,\n  \"b\": [2, 3]\n}")
	if !replayParamsEquivalent(a, b) {
		t.Fatal("formatting and object key order should not mark a replay as edited")
	}
	if replayParamsEquivalent(a, json.RawMessage(`{"a":1,"b":[2,4]}`)) {
		t.Fatal("a changed nested value should mark a replay as edited")
	}
}
