package tools

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
)

// serveSearchStub points the DuckDuckGo endpoint at a local server
// returning the given status and body, and restores it on cleanup.
func serveSearchStub(t *testing.T, status int, body string) {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(status)
		_, _ = w.Write([]byte(body))
	}))
	t.Cleanup(srv.Close)
	orig := ddgLiteEndpoint
	ddgLiteEndpoint = srv.URL + "/lite/?q="
	t.Cleanup(func() { ddgLiteEndpoint = orig })
}

// loadAnomalyPage reads the real bot-check payload captured from
// lite.duckduckgo.com on 2026-07-29 (HTTP 202, captcha modal).
func loadAnomalyPage(t *testing.T) string {
	t.Helper()
	data, err := os.ReadFile("testdata/ddg_anomaly_202.html")
	if err != nil {
		t.Fatalf("read testdata: %v", err)
	}
	return string(data)
}

// TestSearchRateLimitedOn202 verifies the 202 anomaly-challenge
// interstitial is reported as throttling, not parsed into an empty
// result set.
func TestSearchRateLimitedOn202(t *testing.T) {
	serveSearchStub(t, http.StatusAccepted, loadAnomalyPage(t))
	_, err := searchDuckDuckGo(context.Background(), http.DefaultClient, "anything", 10)
	if !errors.Is(err, errSearchRateLimited) {
		t.Fatalf("expected errSearchRateLimited, got %v", err)
	}
}

// TestSearchRateLimitedOnAnomalyPage verifies the captcha modal is
// detected by content as well: DuckDuckGo also serves the same page
// with HTTP 200 once a client is flagged.
func TestSearchRateLimitedOnAnomalyPage(t *testing.T) {
	serveSearchStub(t, http.StatusOK, loadAnomalyPage(t))
	_, err := searchDuckDuckGo(context.Background(), http.DefaultClient, "anything", 10)
	if !errors.Is(err, errSearchRateLimited) {
		t.Fatalf("expected errSearchRateLimited, got %v", err)
	}
}

// TestSearchParsesNormalResults guards against false positives: a
// genuine results page still parses.
func TestSearchParsesNormalResults(t *testing.T) {
	page := `<html><body><table>
<tr><td><a class="result-link" href="//duckduckgo.com/l/?uddg=https%3A%2F%2Fexample.com%2Fpost">Example Post</a></td></tr>
<tr><td class="result-snippet">A snippet about the example post.</td></tr>
</table></body></html>`
	serveSearchStub(t, http.StatusOK, page)
	results, err := searchDuckDuckGo(context.Background(), http.DefaultClient, "example", 10)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(results) != 1 || results[0].Link != "https://example.com/post" {
		t.Fatalf("unexpected results: %+v", results)
	}
}

// TestSearchSearXNGParsesResults verifies the SearXNG JSON API path
// queries the /search endpoint with format=json and parses results.
func TestSearchSearXNGParsesResults(t *testing.T) {
	t.Parallel()

	var gotQuery, gotFormat string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotQuery = r.URL.Query().Get("q")
		gotFormat = r.URL.Query().Get("format")
		payload := searxngResponse{Results: []searxngResult{
			{URL: "https://example.com/a", Title: "A", Content: "snippet a"},
			{URL: "https://example.com/b", Title: "B", Content: "snippet b"},
			{URL: "", Title: "skipped", Content: "no url"},
		}}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(payload)
	}))
	defer srv.Close()

	results, err := searchSearXNG(context.Background(), srv.Client(), srv.URL, "example", 10)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if gotFormat != "json" || !strings.Contains(gotQuery, "example") {
		t.Fatalf("unexpected request: q=%q format=%q", gotQuery, gotFormat)
	}
	if len(results) != 2 || results[0].Link != "https://example.com/a" || results[1].Position != 2 {
		t.Fatalf("unexpected results: %+v", results)
	}
}

// TestSearchSearXNGMaxResults verifies the result cap is respected.
func TestSearchSearXNGMaxResults(t *testing.T) {
	t.Parallel()

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		payload := searxngResponse{}
		for i := range 10 {
			payload.Results = append(payload.Results, searxngResult{
				URL:   fmt.Sprintf("https://example.com/%d", i),
				Title: fmt.Sprintf("R%d", i),
			})
		}
		_ = json.NewEncoder(w).Encode(payload)
	}))
	defer srv.Close()

	results, err := searchSearXNG(context.Background(), srv.Client(), srv.URL, "example", 3)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(results) != 3 {
		t.Fatalf("expected 3 results, got %d", len(results))
	}
}

// TestSearchSearXNGErrorStatus verifies non-200 responses surface as
// errors so callers can fall back to DuckDuckGo.
func TestSearchSearXNGErrorStatus(t *testing.T) {
	t.Parallel()

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusForbidden)
	}))
	defer srv.Close()

	_, err := searchSearXNG(context.Background(), srv.Client(), srv.URL, "example", 10)
	if err == nil {
		t.Fatal("expected an error for non-200 response")
	}
}
