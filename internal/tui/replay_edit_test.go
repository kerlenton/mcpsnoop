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
		case "resaved":
			// vim's :wq on an unmodified buffer, which rewrites the same bytes.
			seeded, readErr := os.ReadFile(path)
			if readErr != nil {
				os.Exit(9)
			}
			err = os.WriteFile(path, seeded, 0o600)
		case "companions":
			// What vim leaves with "set backup" or "set undofile", and what emacs
			// leaves as an autosave. Same captured bytes, a name mcpsnoop never chose.
			dir, base := filepath.Dir(path), filepath.Base(path)
			seeded, readErr := os.ReadFile(path)
			if readErr != nil {
				os.Exit(9)
			}
			for _, name := range []string{base + "~", "." + base + ".swp", "#" + base + "#"} {
				if err = os.WriteFile(filepath.Join(dir, name), seeded, 0o600); err != nil {
					break
				}
			}
			if err == nil {
				err = os.WriteFile(path, []byte("{\n  \"name\": \"echo\",\n  \"arguments\": {\"text\": \"new\"}\n}\n"), 0o600)
			}
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
		{name: "unchanged", behavior: "unchanged", wantErr: "saved no change"},
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
		t.Setenv("MCPSNOOP_EDITOR_BEHAVIOR", "changed")
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
		t.Setenv("MCPSNOOP_EDITOR_BEHAVIOR", "changed")
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

// TestEditorBufferKeepsTheBytesTheServerSent. The buffer is what the user edits,
// so it has to be what the server actually sent. Decoding into a map and
// re-encoding does neither: encoding/json escapes &, < and > on the way out, so
// a URL argument arrived as "https://host?a=1&b=2", and a map has no order,
// so every object came back alphabetised. internal/jsonwire exists in this repo
// because of the first half of that.
func TestEditorBufferKeepsTheBytesTheServerSent(t *testing.T) {
	const captured = `{"zeta":1,"url":"https://h/p?a=1&b=2","alpha":"<b>","n":{"y":2,"x":1}}`
	call := store.CallView{Method: "tools/call", Params: json.RawMessage(captured)}

	t.Setenv("TMPDIR", t.TempDir())
	t.Setenv("VISUAL", "")
	t.Setenv("EDITOR", os.Args[0])
	t.Setenv("MCPSNOOP_EDITOR_HELPER", "1")
	t.Setenv("MCPSNOOP_EDITOR_BEHAVIOR", "unchanged")

	cmd, _, err := prepareReplayEditor(call, call.Params)
	if err != nil {
		t.Fatal(err)
	}
	seeded, err := os.ReadFile(cmd.Args[len(cmd.Args)-1])
	if err != nil {
		t.Fatal(err)
	}
	got := string(seeded)
	for _, want := range []string{`"https://h/p?a=1&b=2"`, `"<b>"`} {
		if !strings.Contains(got, want) {
			t.Errorf("buffer escaped what the server sent, %s is missing:\n%s", want, got)
		}
	}
	if strings.Contains(got, `\u00`) {
		t.Errorf("buffer carries an escape the capture did not:\n%s", got)
	}
	// Key order is the server's, not the alphabet's.
	if z, a := strings.Index(got, `"zeta"`), strings.Index(got, `"alpha"`); z < 0 || a < 0 || z > a {
		t.Errorf("buffer reordered the keys:\n%s", got)
	}
	if x, y := strings.Index(got, `"y"`), strings.Index(got, `"x"`); x < 0 || y < 0 || x > y {
		t.Errorf("buffer reordered a nested object:\n%s", got)
	}
}

// TestEditorThatSavesNothingSendsNothing. An editor that hands the file to an
// already-running instance and exits, which is what EDITOR=code without --wait
// and emacsclient -n do, returns before the user has typed. Treating that as a
// finished edit fired a live request at the server carrying the untouched
// captured params, while the user was still looking at the window, and the
// result panel called it REPLAY rather than EDITED so nothing said so.
func TestEditorThatSavesNothingSendsNothing(t *testing.T) {
	call := store.CallView{Method: "tools/call", Params: json.RawMessage(`{"name":"echo"}`)}
	for _, behavior := range []string{"unchanged", "resaved"} {
		t.Run(behavior, func(t *testing.T) {
			t.Setenv("TMPDIR", t.TempDir())
			t.Setenv("VISUAL", "")
			t.Setenv("EDITOR", os.Args[0])
			t.Setenv("MCPSNOOP_EDITOR_HELPER", "1")
			t.Setenv("MCPSNOOP_EDITOR_BEHAVIOR", behavior)

			cmd, done, err := prepareReplayEditor(call, call.Params)
			if err != nil {
				t.Fatal(err)
			}
			msg := done(cmd.Run()).(replayEditDoneMsg)
			if msg.err == nil {
				t.Fatalf("an unedited buffer was sent as an edit: %s", msg.call.Params)
			}
			// The message has to name the key that does resend it, or a user who
			// meant to replay unchanged is left with no way forward.
			if !strings.Contains(msg.err.Error(), "press r") {
				t.Fatalf("error does not say what to do instead: %v", msg.err)
			}
		})
	}
}

// TestEditorBufferDirectoryGoesWholeIncludingCompanions. Editors write beside
// the file they are given, a vim swap or undo file, an emacs autosave, a
// rename-style backup, each holding the same captured params and named by the
// editor rather than by mcpsnoop. Unlinking the one path mcpsnoop knows about
// left those on disk with the captured request in them.
func TestEditorBufferDirectoryGoesWholeIncludingCompanions(t *testing.T) {
	tmp := t.TempDir()
	t.Setenv("TMPDIR", tmp)
	t.Setenv("VISUAL", "")
	t.Setenv("EDITOR", os.Args[0])
	t.Setenv("MCPSNOOP_EDITOR_HELPER", "1")
	t.Setenv("MCPSNOOP_EDITOR_BEHAVIOR", "companions")

	call := store.CallView{Method: "tools/call", Params: json.RawMessage(`{"name":"echo","arguments":{"token":"sk-live-secret"}}`)}
	cmd, done, err := prepareReplayEditor(call, call.Params)
	if err != nil {
		t.Fatal(err)
	}
	dir := filepath.Dir(cmd.Args[len(cmd.Args)-1])
	if msg := done(cmd.Run()).(replayEditDoneMsg); msg.err != nil {
		t.Fatalf("editor callback: %v", msg.err)
	}
	if _, err := os.Stat(dir); !os.IsNotExist(err) {
		left, _ := os.ReadDir(dir)
		names := make([]string, 0, len(left))
		for _, e := range left {
			names = append(names, e.Name())
		}
		t.Fatalf("the buffer directory survived holding %v", names)
	}
	// Nothing of it is anywhere else under the temp root either.
	entries, err := os.ReadDir(tmp)
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range entries {
		if strings.Contains(e.Name(), "mcpsnoop-replay") {
			t.Fatalf("%s was left behind", e.Name())
		}
	}
}

// TestEditorValueThatNeedsAShellIsRefusedByName. The whole value is tried as one
// executable so a path with a space works, and anything else is split on
// whitespace so "code --wait" works. A quoted path carrying flags needs a shell
// to unquote and gets neither, and letting it through produced
// "fork/exec /Applications/Sublime: no such file or directory", which names a
// path the user never wrote.
func TestEditorValueThatNeedsAShellIsRefusedByName(t *testing.T) {
	call := store.CallView{Method: "tools/call", Params: json.RawMessage(`{"a":1}`)}

	// A real executable whose path contains a space, since the whole point is that
	// this one resolves as a single value and the others cannot.
	spaced := filepath.Join(t.TempDir(), "My Editor")
	if err := os.WriteFile(spaced, []byte("#!/bin/sh\nexit 0\n"), 0o700); err != nil {
		t.Fatal(err)
	}

	for name, tc := range map[string]struct {
		editor  string
		wantErr bool
	}{
		"a path with a space, alone":       {spaced, false},
		"a bare name with flags":           {"vim -n", false},
		"a path with a space, plus a flag": {spaced + " --wait", true},
		"a quoted path with a flag":        {`"` + spaced + `" --wait`, true},
	} {
		t.Run(name, func(t *testing.T) {
			t.Setenv("TMPDIR", t.TempDir())
			t.Setenv("VISUAL", tc.editor)
			t.Setenv("EDITOR", "")
			_, _, err := prepareReplayEditor(call, call.Params)
			if tc.wantErr {
				if err == nil {
					t.Fatal("a value needing a shell was accepted, so exec fails on a mangled path instead")
				}
				if !strings.Contains(err.Error(), "runs no shell") {
					t.Fatalf("the error must say why, got %v", err)
				}
				return
			}
			if err != nil {
				t.Fatalf("a value mcpsnoop can run was refused: %v", err)
			}
		})
	}
}
