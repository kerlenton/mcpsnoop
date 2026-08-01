// Package paths resolves the well-known locations mcpsnoop uses so the shim and
// the hub agree without any flags or manual socket wiring. This is deliberate.
// The whole UX win over prior art is "wrap your server, then just run mcpsnoop",
// no --socket, no --name, no ordering dance.
//
// Resolution order for the base directory, highest priority first.
//
//	$MCPSNOOP_HOME            explicit override (tests, power users)
//	$XDG_STATE_HOME/mcpsnoop  XDG, when set
//	~/.local/state/mcpsnoop   default (macOS, Linux, and Windows alike, where
//	                          ~ is the OS home from os.UserHomeDir)
package paths

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"unicode"
)

// maxSocketPathLen is a conservative unix-domain socket path limit. sun_path is
// 104 bytes on darwin and 108 on Linux including the null terminator, so 103
// usable bytes is safe on both. A longer path makes bind and dial fail with a
// bare "invalid argument".
const maxSocketPathLen = 103

// CheckSocketPath returns an actionable error when path is too long to use as a
// unix domain socket, instead of the opaque syscall error bind and dial produce.
func CheckSocketPath(path string) error {
	if len(path) > maxSocketPathLen {
		return fmt.Errorf("socket path %q is %d bytes, over the %d-byte unix socket limit; set a shorter MCPSNOOP_HOME",
			path, len(path), maxSocketPathLen)
	}
	return nil
}

// maxLabelLen bounds a label so the file name it ends up in stays within the
// 255-byte limit every mainstream filesystem enforces. The label is only part of
// that name: newSessionID appends a pid and a nonce, and SessionLogPath appends
// ".jsonl", so the budget has to leave room for both. 200 is comfortably clear
// of the boundary, which matters because the pid width varies, so an unbounded
// label can be accepted on one run and rejected on the next.
//
// Without this the failure is silent rather than loud. OpenFile returns
// ENAMETOOLONG, the trace sink warns once and carries on, the process exits 0,
// and the whole session goes unrecorded.
const maxLabelLen = 200

// CheckLabel returns an actionable error when a label cannot safely name files
// under the state directory. An explicit --label flows verbatim into the
// session id and from there into SessionLogPath's file name, so a path
// separator would write the trace outside SessionsDir, and the other cases here
// produce a session that open and export cannot address again. labelFor already
// strips separators when it derives a label from the wrapped command; this is
// the same guarantee for the label the user supplies. Rejecting rather than
// sanitising is deliberate: --label is documented as needing to be stable and
// unique for baselines, and a silently rewritten label is neither.
//
// ".." is deliberately NOT rejected. Once separators are banned it cannot
// traverse anything, and newSessionID always appends "-<pid>-<nonce>", so the
// id is never the bare ".." that a Join would resolve. Rejecting it would turn
// away ordinary values like "v1..v2" and "foo..bar" for no gain, and refusing
// correct input is the failure mode this project keeps having to undo.
func CheckLabel(label string) error {
	switch {
	case strings.ContainsAny(label, `/\`):
		return fmt.Errorf("label %q contains a path separator; a label names files under the mcpsnoop state dir", label)
	case strings.ContainsRune(label, 0):
		return fmt.Errorf("label contains a NUL byte; a label names files under the mcpsnoop state dir")
	case containsControl(label):
		// A control character reaches the file name and the startup banner raw. A
		// carriage return in particular rewrites the line the user is reading, so
		// the banner can name a label the session does not have. The OTLP header
		// flag already refuses CR and LF for the same reason.
		return fmt.Errorf("label %q contains a control character; a label names files under the mcpsnoop state dir", label)
	case strings.HasPrefix(label, "-"):
		// The label becomes the session id, and the id is a positional argument to
		// open, export and diff. One starting with a dash is parsed as a flag
		// there, so the session can be recorded and then never addressed again,
		// which is the harm this check exists to prevent.
		return fmt.Errorf("label %q starts with a dash; the session id it forms would parse as a flag in mcpsnoop open and export", label)
	case len(label) > maxLabelLen:
		return fmt.Errorf("label is %d bytes, over the %d-byte limit; the session id it forms must still fit a file name", len(label), maxLabelLen)
	}
	return nil
}

// containsControl reports whether s holds a C0 or C1 control character. unicode
// .IsControl covers both, which matters because a lone C1 byte is as damaging in
// a terminal as an ESC.
func containsControl(s string) bool {
	return strings.ContainsFunc(s, unicode.IsControl)
}

// Base returns the mcpsnoop state directory, creating it if needed.
func Base() string {
	var base string
	switch {
	case os.Getenv("MCPSNOOP_HOME") != "":
		base = os.Getenv("MCPSNOOP_HOME")
	case os.Getenv("XDG_STATE_HOME") != "":
		base = filepath.Join(os.Getenv("XDG_STATE_HOME"), "mcpsnoop")
	default:
		home, err := os.UserHomeDir()
		if err != nil || home == "" {
			home = os.TempDir()
		}
		base = filepath.Join(home, ".local", "state", "mcpsnoop")
	}
	_ = os.MkdirAll(base, 0o700)
	return base
}

// SocketPath is the unix socket the hub listens on and shims connect to.
func SocketPath() string {
	return filepath.Join(Base(), "hub.sock")
}

// SessionsDir holds per-session JSONL trace logs. Created if needed.
func SessionsDir() string {
	d := filepath.Join(Base(), "sessions")
	_ = os.MkdirAll(d, 0o700)
	return d
}

// ExportsDir holds files written from the TUI export action.
func ExportsDir() string {
	d := filepath.Join(Base(), "exports")
	_ = os.MkdirAll(d, 0o700)
	return d
}

// ToolBaselinesDir holds trust-on-first-use tool definitions per server label.
func ToolBaselinesDir() string {
	d := filepath.Join(Base(), "tool-baselines")
	_ = os.MkdirAll(d, 0o700)
	return d
}

// SessionLogPath returns the JSONL trace path for a given session id. Use it
// only for an id mcpsnoop minted itself, where CheckLabel has already run on the
// label the id is built from.
func SessionLogPath(sessionID string) string {
	return filepath.Join(SessionsDir(), sessionID+".jsonl")
}

// SessionLogPathFrom is SessionLogPath for an id that came out of a log rather
// than out of newSessionID, and it refuses one that cannot name a file under the
// sessions directory.
//
// The difference matters because the two ids have different provenance. A label
// is typed by the user and CheckLabel guards it up front, but a session id is
// read straight out of a log's own session_id field, and a log is a file people
// hand around, export and `open -` exist for exactly that, and the hub backfills
// any .jsonl dropped into the sessions directory. An id spelling "../../x"
// therefore made filepath.Join resolve anywhere on disk, which the TUI's delete
// key then passed to os.Remove.
func SessionLogPathFrom(sessionID string) (string, error) {
	if err := CheckSessionID(sessionID); err != nil {
		return "", err
	}
	return SessionLogPath(sessionID), nil
}

// CheckSessionID rejects a session id that cannot safely name a file under the
// sessions directory. The rules are CheckLabel's, since an id is a label with a
// pid and a nonce appended, plus a refusal of "." and ".." which a label may
// legitimately contain in the middle but which are the whole of a traversal.
func CheckSessionID(id string) error {
	switch {
	case id == "":
		return errors.New("session id is empty; it names a file under the mcpsnoop state dir")
	case id == "." || id == "..":
		return fmt.Errorf("session id %q names a directory rather than a session log", id)
	}
	if err := CheckLabel(id); err != nil {
		return fmt.Errorf("session id %q cannot name a file under the mcpsnoop state dir", id)
	}
	return nil
}
