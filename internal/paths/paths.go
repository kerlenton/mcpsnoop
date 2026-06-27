// Package paths resolves the well-known locations mcpsnoop uses so the shim and
// the hub agree without any flags or manual socket wiring. This is deliberate:
// the whole UX win over prior art is "wrap your server, then just run mcpsnoop" —
// no --socket, no --name, no ordering dance.
//
// Resolution order for the base directory:
//
//	$MCPSNOOP_HOME            explicit override (tests, power users)
//	$XDG_STATE_HOME/mcpsnoop  XDG, when set
//	~/.local/state/mcpsnoop   default (works on macOS and Linux alike)
package paths

import (
	"os"
	"path/filepath"
)

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

// SessionLogPath returns the JSONL trace path for a given session id.
func SessionLogPath(sessionID string) string {
	return filepath.Join(SessionsDir(), sessionID+".jsonl")
}
