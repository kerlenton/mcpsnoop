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
	"fmt"
	"os"
	"path/filepath"
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

// SessionLogPath returns the JSONL trace path for a given session id.
func SessionLogPath(sessionID string) string {
	return filepath.Join(SessionsDir(), sessionID+".jsonl")
}
