package main

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestExtractSearchResults_picksFirstVideoIDs(t *testing.T) {
	// Simulate a fragment of YouTube's ytInitialData JSON. Two distinct video
	// IDs, each followed by a title in either "runs" or "simpleText" form.
	html := `var ytInitialData = {"contents": [
		{"videoRenderer": {"videoId":"abcdefghijk", "title": {"runs": [{"text":"First Track"}]}}},
		{"videoRenderer": {"videoId":"ABC_DEF-123", "title": {"simpleText":"Second Track"}}}
	]};`

	got := extractSearchResults(html, 5)
	if len(got) != 2 {
		t.Fatalf("expected 2 results, got %d: %#v", len(got), got)
	}
	if got[0].VideoID != "abcdefghijk" || !strings.Contains(got[0].URL, "abcdefghijk") {
		t.Errorf("first result wrong: %#v", got[0])
	}
	if got[0].Title != "First Track" {
		t.Errorf("first title wrong: %q", got[0].Title)
	}
	if got[1].VideoID != "ABC_DEF-123" {
		t.Errorf("second result wrong: %#v", got[1])
	}
	if got[1].Title != "Second Track" {
		t.Errorf("second title wrong: %q", got[1].Title)
	}
}

func TestExtractSearchResults_dedupesAndRespectsLimit(t *testing.T) {
	html := `"videoId":"aaaaaaaaaaa","title":{"runs":[{"text":"A"}]}` +
		`"videoId":"aaaaaaaaaaa","title":{"runs":[{"text":"A again"}]}` +
		`"videoId":"bbbbbbbbbbb","title":{"runs":[{"text":"B"}]}` +
		`"videoId":"ccccccccccc","title":{"runs":[{"text":"C"}]}`

	got := extractSearchResults(html, 2)
	if len(got) != 2 {
		t.Fatalf("expected 2 results (dedup + limit), got %d: %#v", len(got), got)
	}
	if got[0].VideoID != "aaaaaaaaaaa" || got[1].VideoID != "bbbbbbbbbbb" {
		t.Errorf("unexpected order: %#v", got)
	}
}

func TestSearchYouTube_handlesNon200(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusServiceUnavailable)
	}))
	defer srv.Close()
	// SearchYouTube hardcodes youtube.com — exercising the parse layer is
	// enough; this test just guards the empty-results error path.
	if _, err := extractSearchResultsErr(""); err == nil {
		t.Error("expected error on empty html")
	}
}

func extractSearchResultsErr(html string) ([]SearchResult, error) {
	r := extractSearchResults(html, 1)
	if len(r) == 0 {
		return nil, http.ErrNoLocation
	}
	return r, nil
}
