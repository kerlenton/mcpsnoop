// Command refserver is the MCP server the client matrix is captured against.
//
// It is built on the official Go SDK, so a client is measured against the
// protocol's own implementation rather than against anything mcpsnoop wrote,
// and the SDK speaks every revision from 2024-11-05 to 2026-07-28, so old and
// new clients connect to the same server. Each tool exists to make a client
// show one behaviour. echo is the baseline call, confirm_action asks the user
// a question back, and slow_task reports progress and can be cancelled. The
// list results carry a one-minute ttlMs, so a client that re-lists sooner is
// ignoring the hint.
package main

import (
	"context"
	"fmt"
	"log"
	"time"

	"github.com/google/jsonschema-go/jsonschema"
	"github.com/modelcontextprotocol/go-sdk/mcp"
)

type echoIn struct {
	Text string `json:"text" jsonschema:"the text to send back"`
}

type confirmIn struct {
	Action string `json:"action" jsonschema:"the action to ask the user to confirm"`
}

type slowIn struct {
	Seconds int `json:"seconds,omitempty" jsonschema:"how long to work, from 1 to 30 seconds, 5 when omitted"`
}

func main() {
	server := mcp.NewServer(&mcp.Implementation{Name: "mcpsnoop-reference", Version: "1"}, &mcp.ServerOptions{
		Instructions: "A reference server for observing how MCP clients behave. Every tool is safe to call.",
		SetCacheable: func(_ context.Context, _ mcp.Request, c *mcp.Cacheable) {
			c.TTLMs = 60_000
			c.CacheScope = "private"
		},
	})
	mcp.AddTool(server, &mcp.Tool{Name: "echo", Description: "Return the text it is given."}, echo)
	mcp.AddTool(server, &mcp.Tool{Name: "confirm_action", Description: "Ask the user to confirm an action, then report what they answered."}, confirm)
	mcp.AddTool(server, &mcp.Tool{Name: "slow_task", Description: "Work for a few seconds while reporting progress, then finish."}, slow)
	// The same two tools again, marked read-only. A client may treat a call it
	// knows has no side effects differently, and the pair makes the difference
	// observable, for example whether two calls asked for at once are sent at once.
	readOnly := &mcp.ToolAnnotations{ReadOnlyHint: true}
	mcp.AddTool(server, &mcp.Tool{Name: "echo_read", Description: "Return the text it is given. Read-only.", Annotations: readOnly}, echo)
	mcp.AddTool(server, &mcp.Tool{Name: "slow_read", Description: "Work for a few seconds while reporting progress, then finish. Read-only.", Annotations: readOnly}, slow)
	mcp.AddTool(server, &mcp.Tool{Name: "list_roots", Description: "Ask the client for its roots and report them."}, listRoots)
	mcp.AddTool(server, &mcp.Tool{Name: "unlock_tool", Description: "Make a new tool, bonus_tool, available on this server."}, unlock(server))
	server.AddPrompt(&mcp.Prompt{
		Name:        "greeting",
		Description: "Greet someone by name.",
		Arguments:   []*mcp.PromptArgument{{Name: "name", Description: "who to greet", Required: true}},
	}, greeting)
	server.AddResource(&mcp.Resource{URI: "ref://about", Name: "about", Description: "What this server is for.", MIMEType: "text/plain"}, about)

	if err := server.Run(context.Background(), &mcp.StdioTransport{}); err != nil {
		log.Fatal(err)
	}
}

func text(s string) *mcp.CallToolResult {
	return &mcp.CallToolResult{Content: []mcp.Content{&mcp.TextContent{Text: s}}}
}

func echo(_ context.Context, _ *mcp.CallToolRequest, in echoIn) (*mcp.CallToolResult, any, error) {
	return text(in.Text), nil, nil
}

// confirm asks through multi round-trip requests. The SDK turns the same
// answer into an elicitation/create request of its own for a client on an older
// revision, so one handler exercises both channels.
func confirm(_ context.Context, req *mcp.CallToolRequest, in confirmIn) (*mcp.CallToolResult, any, error) {
	// The specification forbids sending an input request the client has not
	// declared it can answer, so a client without elicitation gets told instead.
	if caps := req.ClientCapabilities(); caps == nil || caps.Elicitation == nil {
		return text("this client did not declare elicitation, so nothing was asked"), nil, nil
	}
	resp, answered := req.Params.InputResponses["confirm"]
	if !answered {
		return &mcp.CallToolResult{
			InputRequests: mcp.InputRequestMap{"confirm": &mcp.ElicitParams{
				Message: "Confirm this action: " + in.Action,
				RequestedSchema: &jsonschema.Schema{
					Type:       "object",
					Properties: map[string]*jsonschema.Schema{"confirm": {Type: "boolean", Description: "yes to go ahead"}},
					Required:   []string{"confirm"},
				},
			}},
			RequestState: "confirm-v1",
		}, nil, nil
	}
	result, ok := resp.(*mcp.ElicitResult)
	if !ok || result == nil {
		return text("the answer was not an elicitation result"), nil, nil
	}
	return text("the user answered " + result.Action), nil, nil
}

// slow reports progress once a second when the client offered a token, and
// stops as soon as the client cancels, which is what makes cancellation
// observable from the server's side too.
func slow(ctx context.Context, req *mcp.CallToolRequest, in slowIn) (*mcp.CallToolResult, any, error) {
	seconds := in.Seconds
	if seconds <= 0 {
		seconds = 5
	}
	seconds = min(seconds, 30)
	token := req.Params.GetProgressToken()
	for i := 1; i <= seconds; i++ {
		select {
		case <-ctx.Done():
			return nil, nil, ctx.Err()
		case <-time.After(time.Second):
		}
		if token != nil {
			_ = req.Session.NotifyProgress(ctx, &mcp.ProgressNotificationParams{
				ProgressToken: token,
				Progress:      float64(i),
				Total:         float64(seconds),
				Message:       fmt.Sprintf("%d of %d seconds", i, seconds),
			})
		}
	}
	return text(fmt.Sprintf("worked for %d seconds", seconds)), nil, nil
}

// listRoots asks for the client's roots the same way confirm asks a question.
// Roots is deprecated in 2026-07-28 and still fully part of it, so what a client
// answers here, and whether it answers at all, is worth measuring while it lasts.
func listRoots(_ context.Context, req *mcp.CallToolRequest, _ struct{}) (*mcp.CallToolResult, any, error) {
	if caps := req.ClientCapabilities(); caps == nil || caps.RootsV2 == nil {
		return text("this client did not declare roots, so nothing was asked"), nil, nil
	}
	resp, answered := req.Params.InputResponses["roots"]
	if !answered {
		return &mcp.CallToolResult{
			InputRequests: mcp.InputRequestMap{"roots": &mcp.ListRootsParams{}},
			RequestState:  "roots-v1",
		}, nil, nil
	}
	result, ok := resp.(*mcp.ListRootsResult)
	if !ok || result == nil {
		return text("the answer was not a roots result"), nil, nil
	}
	if len(result.Roots) == 0 {
		return text("the client has no roots"), nil, nil
	}
	out := fmt.Sprintf("the client sent %d root(s):", len(result.Roots))
	for _, r := range result.Roots {
		out += " " + r.URI
	}
	return text(out), nil, nil
}

// unlock adds a tool while the conversation is running. The SDK announces it
// with notifications/tools/list_changed, which a 2026-07-28 client receives on
// the subscriptions/listen stream it opted into, so whether the new tool is
// usable afterwards shows whether the client acts on that stream.
func unlock(server *mcp.Server) mcp.ToolHandlerFor[struct{}, any] {
	return func(context.Context, *mcp.CallToolRequest, struct{}) (*mcp.CallToolResult, any, error) {
		mcp.AddTool(server, &mcp.Tool{Name: "bonus_tool", Description: "A tool that only exists after unlock_tool was called."},
			func(context.Context, *mcp.CallToolRequest, struct{}) (*mcp.CallToolResult, any, error) {
				return text("bonus_tool works"), nil, nil
			})
		return text("bonus_tool is now available"), nil, nil
	}
}

func greeting(_ context.Context, req *mcp.GetPromptRequest) (*mcp.GetPromptResult, error) {
	name := req.Params.Arguments["name"]
	return &mcp.GetPromptResult{
		Messages: []*mcp.PromptMessage{{Role: "user", Content: &mcp.TextContent{Text: "Say hello to " + name + "."}}},
	}, nil
}

func about(_ context.Context, req *mcp.ReadResourceRequest) (*mcp.ReadResourceResult, error) {
	return &mcp.ReadResourceResult{Contents: []*mcp.ResourceContents{{
		URI:      req.Params.URI,
		MIMEType: "text/plain",
		Text:     "mcpsnoop's reference server. It exists so that what an MCP client does can be captured and compared.",
	}}}, nil
}
