// Command goclient drives the reference server with the official Go SDK client.
//
// It is the one column of the client matrix that needs no account, and it runs
// a fixed scenario that touches every behaviour the matrix reports on. It
// lists everything, reads a resource and a prompt, calls each tool, accepts the
// confirmation, asks for progress, lists the tools again inside the cache
// window, and gives up on a slow call so the cancellation shows.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"log"
	"os"
	"os/exec"
	"runtime/debug"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

func main() {
	revision := flag.String("revision", "", "protocol revision to speak, the SDK's newest when empty")
	name := flag.String("name", "go-sdk", "clientInfo name to send")
	flag.Usage = func() {
		fmt.Fprintln(os.Stderr, "usage: goclient [-revision 2025-11-25] [-name n] -- <server command> [args...]")
		flag.PrintDefaults()
	}
	flag.Parse()
	if flag.NArg() == 0 {
		flag.Usage()
		os.Exit(2)
	}
	log.SetFlags(0)
	log.SetPrefix("goclient: ")

	ctx := context.Background()
	client := mcp.NewClient(&mcp.Implementation{Name: *name, Version: sdkVersion()}, &mcp.ClientOptions{
		ElicitationHandler: func(context.Context, *mcp.ElicitRequest) (*mcp.ElicitResult, error) {
			return &mcp.ElicitResult{Action: "accept", Content: map[string]any{"confirm": true}}, nil
		},
	})
	client.AddRoots(&mcp.Root{URI: "file:///tmp/goclient-root", Name: "goclient"})
	cmd := exec.Command(flag.Arg(0), flag.Args()[1:]...)
	cmd.Stderr = os.Stderr
	session, err := client.Connect(ctx, &mcp.CommandTransport{Command: cmd}, &mcp.ClientSessionOptions{ProtocolVersion: *revision})
	if err != nil {
		log.Fatal("connect: ", err)
	}
	defer session.Close()

	failed := false
	step := func(what string, err error) {
		if err != nil {
			failed = true
			log.Printf("%s: %v", what, err)
			return
		}
		log.Printf("%s: ok", what)
	}

	_, err = session.ListTools(ctx, nil)
	step("tools/list", err)
	_, err = session.ListPrompts(ctx, nil)
	step("prompts/list", err)
	_, err = session.ListResources(ctx, nil)
	step("resources/list", err)
	_, err = session.ReadResource(ctx, &mcp.ReadResourceParams{URI: "ref://about"})
	step("resources/read", err)
	_, err = session.GetPrompt(ctx, &mcp.GetPromptParams{Name: "greeting", Arguments: map[string]string{"name": "matrix"}})
	step("prompts/get", err)
	_, err = session.CallTool(ctx, &mcp.CallToolParams{Name: "echo", Arguments: map[string]any{"text": "hello"}})
	step("echo", err)

	res, err := session.CallTool(ctx, &mcp.CallToolParams{Name: "confirm_action", Arguments: map[string]any{"action": "delete the demo file"}})
	step("confirm_action", err)
	if err == nil {
		for _, c := range res.Content {
			if t, ok := c.(*mcp.TextContent); ok {
				log.Printf("confirm_action answered: %s", t.Text)
			}
		}
	}

	slow := &mcp.CallToolParams{Name: "slow_task", Arguments: map[string]any{"seconds": 2}}
	slow.SetProgressToken("progress-1")
	_, err = session.CallTool(ctx, slow)
	step("slow_task with a progress token", err)

	_, err = session.ListTools(ctx, nil)
	step("tools/list again, inside the ttl", err)

	res, err = session.CallTool(ctx, &mcp.CallToolParams{Name: "list_roots"})
	step("list_roots", err)
	if err == nil {
		for _, c := range res.Content {
			if t, ok := c.(*mcp.TextContent); ok {
				log.Printf("list_roots answered: %s", t.Text)
			}
		}
	}
	_, err = session.CallTool(ctx, &mcp.CallToolParams{Name: "unlock_tool"})
	step("unlock_tool", err)
	_, err = session.CallTool(ctx, &mcp.CallToolParams{Name: "bonus_tool"})
	step("bonus_tool, added mid-session", err)

	short, cancel := context.WithTimeout(ctx, 1500*time.Millisecond)
	_, err = session.CallTool(short, &mcp.CallToolParams{Name: "slow_task", Arguments: map[string]any{"seconds": 10}})
	cancel()
	if errors.Is(err, context.DeadlineExceeded) {
		log.Print("slow_task given up after 1.5s: ok")
	} else {
		step("slow_task given up after 1.5s", err)
	}
	// The session carries on after giving up, the way a real client does. Closing
	// at once would leave no room for a cancellation the SDK sends after the call
	// returns, and the capture would blame it for one it never had time to send.
	_, err = session.CallTool(ctx, &mcp.CallToolParams{Name: "echo", Arguments: map[string]any{"text": "still here"}})
	step("echo after giving up", err)

	// A retry after a timeout, the case spec issue #3394 reproduces. add_note adds
	// its note before it answers, so giving up on it and sending it again with the
	// same arguments adds the note twice, which count_notes then shows.
	_, err = session.CallTool(ctx, &mcp.CallToolParams{Name: "clear_notes"})
	step("clear_notes", err)
	note := map[string]any{"text": "retried", "delay_ms": 1000}
	short, cancel = context.WithTimeout(ctx, 300*time.Millisecond)
	_, err = session.CallTool(short, &mcp.CallToolParams{Name: "add_note", Arguments: note})
	cancel()
	if errors.Is(err, context.DeadlineExceeded) {
		log.Print("add_note given up after 0.3s: ok")
	} else {
		step("add_note given up after 0.3s", err)
	}
	_, err = session.CallTool(ctx, &mcp.CallToolParams{Name: "add_note", Arguments: note})
	step("add_note sent again", err)
	res, err = session.CallTool(ctx, &mcp.CallToolParams{Name: "count_notes"})
	step("count_notes", err)
	if err == nil {
		for _, c := range res.Content {
			if t, ok := c.(*mcp.TextContent); ok {
				log.Printf("count_notes answered: %s", t.Text)
			}
		}
	}
	time.Sleep(500 * time.Millisecond)

	if failed {
		os.Exit(1)
	}
}

// sdkVersion is the SDK version this binary was built with, which is the honest
// version for a column that measures the SDK.
func sdkVersion() string {
	if info, ok := debug.ReadBuildInfo(); ok {
		for _, dep := range info.Deps {
			if dep.Path == "github.com/modelcontextprotocol/go-sdk" {
				return dep.Version
			}
		}
	}
	return "unknown"
}
