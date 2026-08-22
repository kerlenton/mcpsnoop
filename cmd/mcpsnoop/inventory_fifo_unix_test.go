//go:build !windows

package main

import (
	"path/filepath"
	"syscall"
	"testing"
	"time"

	"github.com/kerlenton/mcpsnoop/internal/paths"
)

// TestInventoryDoesNotHangOnAFifo covers a directory anything can write to. Open
// on a fifo blocks until a writer appears, and the command prints nothing until
// the whole walk is done, so one of them stopped the answer entirely.
func TestInventoryDoesNotHangOnAFifo(t *testing.T) {
	t.Setenv("MCPSNOOP_HOME", t.TempDir())
	t0 := time.Date(2026, 8, 1, 12, 0, 0, 0, time.UTC)
	writeSessionLog(t, "good.jsonl", stdioSession(t, "s1", "srv", t0, "/proj", []string{"node", "i.js"}, false)...)
	fifo := filepath.Join(paths.SessionsDir(), "pipe.jsonl")
	if err := syscall.Mkfifo(fifo, 0o600); err != nil {
		t.Skipf("cannot make a fifo here: %v", err)
	}

	done := make(chan inventory, 1)
	go func() {
		inv, err := takeInventory(paths.SessionsDir(), false)
		if err == nil {
			done <- inv
		}
		close(done)
	}()
	select {
	case inv, ok := <-done:
		if !ok {
			t.Fatal("takeInventory failed on a directory holding a fifo")
		}
		if len(inv.Servers) != 1 {
			t.Fatalf("servers = %d, want the one real log still reported", len(inv.Servers))
		}
		if inv.Skipped != 1 {
			t.Fatalf("skipped = %d, want the fifo counted", inv.Skipped)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("a fifo in the sessions directory hung the whole command")
	}
}
