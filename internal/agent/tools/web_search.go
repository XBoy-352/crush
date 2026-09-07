package tools

import (
	"context"
	_ "embed"
	"html/template"
	"log/slog"
	"net/http"
	"strings"
	"time"

	"charm.land/fantasy"
)

//go:embed web_search.md.tpl
var webSearchDescriptionTmpl []byte

var webSearchDescriptionTpl = template.Must(
	template.New("webSearchDescription").
		Parse(string(webSearchDescriptionTmpl)),
)

// NewWebSearchTool creates a web search tool for sub-agents (no permissions needed).
// When engine is "searxng", queries are sent to the SearXNG instance at
// searxngURL, falling back to DuckDuckGo if the instance is unreachable.
// Any other engine value uses DuckDuckGo directly.
func NewWebSearchTool(client *http.Client, searxngURL, engine string) fantasy.AgentTool {
	if client == nil {
		transport := http.DefaultTransport.(*http.Transport).Clone()
		transport.MaxIdleConns = 100
		transport.MaxIdleConnsPerHost = 10
		transport.IdleConnTimeout = 90 * time.Second

		client = &http.Client{
			Timeout:   30 * time.Second,
			Transport: transport,
		}
	}

	useSearxng := strings.EqualFold(engine, "searxng") && strings.TrimSpace(searxngURL) != ""

	return fantasy.NewParallelAgentTool(
		WebSearchToolName,
		renderToolDescription(webSearchDescriptionTpl),
		func(ctx context.Context, params WebSearchParams, call fantasy.ToolCall) (fantasy.ToolResponse, error) {
			if params.Query == "" {
				return fantasy.NewTextErrorResponse("query is required"), nil
			}

			maxResults := params.MaxResults
			if maxResults <= 0 {
				maxResults = 10
			}
			if maxResults > 20 {
				maxResults = 20
			}

			var (
				results []SearchResult
				err     error
			)
			switch {
			case useSearxng:
				results, err = searchSearXNG(ctx, client, searxngURL, params.Query, maxResults)
				if err != nil {
					// The configured instance is unavailable; fall
					// back to DuckDuckGo instead of failing the call.
					slog.Warn("SearXNG search failed, falling back to DuckDuckGo", "url", searxngURL, "error", err)
					results, err = searchDuckDuckGo(ctx, client, params.Query, maxResults)
				}
			default:
				maybeDelaySearch()
				results, err = searchDuckDuckGo(ctx, client, params.Query, maxResults)
			}
			slog.Debug("Web search completed", "query", params.Query, "results", len(results), "err", err)
			if err != nil {
				return fantasy.NewTextErrorResponse("Failed to search: " + err.Error()), nil
			}

			return fantasy.NewTextResponse(formatSearchResults(results)), nil
		},
	)
}
