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
