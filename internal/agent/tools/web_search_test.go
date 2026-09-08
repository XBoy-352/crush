package tools

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"charm.land/fantasy"
)

// TestWebSearchFallbackToDuckDuckGo verifies that when the configured
// SearXNG instance is unreachable, the tool falls back to DuckDuckGo
// instead of erroring.
func TestWebSearchFallbackToDuckDuckGo(t *testing.T) {
	serveSearchStub(t, http.StatusOK, `<html><body><table>
<tr><td><a class="result-link" href="//duckduckgo.com/l/?uddg=https%3A%2F%2Fexample.com%2Fpost">Example Post</a></td></tr>
<tr><td class="result-snippet">A snippet.</td></tr>
</table></body></html>`)

	tool := NewWebSearchTool(nil, "http://127.0.0.1:1", "searxng")
	resp, err := tool.Run(context.Background(), fantasy.ToolCall{
		ID:    "t1",
		Name:  WebSearchToolName,
		Input: `{"query":"example"}`,
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if resp.IsError {
		t.Fatalf("unexpected tool error: %s", resp.Content)
	}
	out := resp.Content
	if !strings.Contains(out, "https://example.com/post") {
		t.Fatalf("expected fallback results in output, got: %s", out)
	}
}

// TestWebSearchDuckDuckGoDefault verifies engine values other than
// "searxng" skip SearXNG entirely, even when a URL is configured.
func TestWebSearchDuckDuckGoDefault(t *testing.T) {
	serveSearchStub(t, http.StatusOK, `<html><body><table>
<tr><td><a class="result-link" href="//duckduckgo.com/l/?uddg=https%3A%2F%2Fexample.com%2Fddg">DDG Result</a></td></tr>
<tr><td class="result-snippet">snippet</td></tr>
</table></body></html>`)

	tool := NewWebSearchTool(nil, "http://127.0.0.1:1", "")
	resp, err := tool.Run(context.Background(), fantasy.ToolCall{
		ID:    "t2",
		Name:  WebSearchToolName,
		Input: `{"query":"example"}`,
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if resp.IsError {
		t.Fatalf("unexpected tool error: %s", resp.Content)
	}
	out := resp.Content
	if !strings.Contains(out, "https://example.com/ddg") {
		t.Fatalf("expected DuckDuckGo results in output, got: %s", out)
	}
}

// TestWebSearchSearXNGPrimary verifies a working SearXNG instance is
// used when selected and its JSON results are formatted for the agent.
func TestWebSearchSearXNGPrimary(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_ = json.NewEncoder(w).Encode(searxngResponse{Results: []searxngResult{
			{URL: "https://example.com/searx", Title: "Searx Result", Content: "snip"},
		}})
	}))
	defer srv.Close()

	tool := NewWebSearchTool(srv.Client(), srv.URL, "searxng")
	resp, err := tool.Run(context.Background(), fantasy.ToolCall{
		ID:    "t3",
		Name:  WebSearchToolName,
		Input: `{"query":"example"}`,
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if resp.IsError {
		t.Fatalf("unexpected tool error: %s", resp.Content)
	}
	out := resp.Content
	if !strings.Contains(out, "https://example.com/searx") {
		t.Fatalf("expected SearXNG results in output, got: %s", out)
	}
}
