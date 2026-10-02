package youcom

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"time"
)

// Client provides web search capabilities via You.com API.
type Client struct {
	apiKey string
	client *http.Client
}

// NewClient creates a You.com search client. Returns nil if apiKey is empty.
func NewClient(apiKey string) *Client {
	if apiKey == "" {
		return nil
	}
	return &Client{
		apiKey: apiKey,
		client: &http.Client{Timeout: 10 * time.Second},
	}
}

// SearchResult represents a single search result from You.com.
type SearchResult struct {
	Title   string `json:"title"`
	URL     string `json:"url"`
	Snippet string `json:"snippet"`
}

// SearchResponse represents the response from You.com search API.
type SearchResponse struct {
	Results []SearchResult `json:"results"`
	Query   string         `json:"query"`
}

// Search performs a web search using You.com API.
func (c *Client) Search(ctx context.Context, query string) (*SearchResponse, error) {
	if c == nil {
		return nil, fmt.Errorf("You.com client not configured")
	}

	// Use the You.com search API endpoint
	reqURL := "https://api.you.com/search"
	params := url.Values{
		"query": {query},
		"count": {"5"}, // Limit results for TUI display
	}

	req, err := http.NewRequestWithContext(ctx, "GET", reqURL+"?"+params.Encode(), nil)
	if err != nil {
		return nil, fmt.Errorf("creating request: %w", err)
	}

	req.Header.Set("X-API-Key", c.apiKey)
	req.Header.Set("Content-Type", "application/json")

	resp, err := c.client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("search request failed: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		return nil, fmt.Errorf("search API error %d: %s", resp.StatusCode, string(body))
	}

	var result SearchResponse
	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		return nil, fmt.Errorf("decoding response: %w", err)
	}

	result.Query = query
	return &result, nil
}

// FormatResults formats search results for TUI display.
func (sr *SearchResponse) FormatResults() string {
	if len(sr.Results) == 0 {
		return fmt.Sprintf("No web search results for: %s", sr.Query)
	}

	var buf bytes.Buffer
	fmt.Fprintf(&buf, "Web Search: %s\n\n", sr.Query)

	for i, result := range sr.Results {
		fmt.Fprintf(&buf, "%d. %s\n", i+1, result.Title)
		fmt.Fprintf(&buf, "   %s\n", result.URL)
		if result.Snippet != "" {
			fmt.Fprintf(&buf, "   %s\n", result.Snippet)
		}
		fmt.Fprint(&buf, "\n")
	}

	return buf.String()
}
