package tui

import (
	"fmt"
	"os"
	"testing"

	tea "github.com/charmbracelet/bubbletea"

	"github.com/kerlenton/mcpsnoop/internal/proxy"
	"github.com/kerlenton/mcpsnoop/internal/store"
)

// TestDumpView prints rendered frames to stdout when MCPSNOOP_DUMP is set, for
// manual visual inspection. No-op otherwise.
//
//	MCPSNOOP_DUMP=1         sessions table
//	MCPSNOOP_DUMP=stream    stream table (drilled in)
//	MCPSNOOP_DUMP=caps      capabilities overlay
//	MCPSNOOP_DUMP=help      help screen
func TestDumpView(t *testing.T) {
	mode := os.Getenv("MCPSNOOP_DUMP")
	if mode == "" {
		t.Skip("set MCPSNOOP_DUMP=1 to dump the view")
	}
	st := store.New(0)
	seed(st)
	st.Ingest(env(5, proxy.ClientToServer, `{"jsonrpc":"2.0","id":3,"method":"tools/call","params":{"name":"slow_search","arguments":{}}}`))
	st.Ingest(env(6, proxy.ServerToClient, `{"jsonrpc":"2.0","id":3,"error":{"code":-32000,"message":"boom"}}`))
	st.Ingest(sessionEnv("s2", "search-api"))
	st.Ingest(sessionEnv("s3", "github"))

	m := New(st)
	m = drive(t, m, tea.WindowSizeMsg{Width: 100, Height: 24})
	m = drive(t, m, frameMsg{})

	switch mode {
	case "stream":
		m = drive(t, m, tea.KeyMsg{Type: tea.KeyEnter})
	case "caps":
		m = drive(t, m, tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("c")})
	case "help":
		m = drive(t, m, tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("?")})
	}
	fmt.Println(m.View())
}
