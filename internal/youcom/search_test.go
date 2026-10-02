package youcom

import (
	"testing"
)

func TestClientCreation(t *testing.T) {
	// Test creating client without API key
	client := NewClient("")
	if client != nil {
		t.Error("Expected nil client when no API key provided")
	}

	// Test creating client with API key
	client = NewClient("test-key")
	if client == nil {
		t.Error("Expected non-nil client when API key provided")
	}
}

func TestSearchFormatting(t *testing.T) {
	resp := &SearchResponse{
		Query: "test query",
		Results: []SearchResult{
			{Title: "Test Result 1", URL: "https://example.com/1", Snippet: "Test snippet 1"},
			{Title: "Test Result 2", URL: "https://example.com/2", Snippet: "Test snippet 2"},
		},
	}

	formatted := resp.FormatResults()

	if !contains(formatted, "test query") {
		t.Error("Formatted results should contain the query")
	}

	if !contains(formatted, "Test Result 1") {
		t.Error("Formatted results should contain result titles")
	}

	if !contains(formatted, "https://example.com/1") {
		t.Error("Formatted results should contain URLs")
	}
}

func TestSearchEmptyResults(t *testing.T) {
	resp := &SearchResponse{
		Query:   "empty query",
		Results: []SearchResult{},
	}

	formatted := resp.FormatResults()

	if !contains(formatted, "No web search results") {
		t.Error("Should indicate no results found")
	}
}

func contains(s, substr string) bool {
	return len(s) >= len(substr) && (s == substr || (len(s) > len(substr) &&
		(s[:len(substr)] == substr || s[len(s)-len(substr):] == substr ||
			func() bool {
				for i := 0; i <= len(s)-len(substr); i++ {
					if s[i:i+len(substr)] == substr {
						return true
					}
				}
				return false
			}())))
}
