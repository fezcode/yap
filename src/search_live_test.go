//go:build live

package main

import (
	"testing"
)

// Run with: go test -tags live ./src/... -run TestSearchYouTube_live -v
func TestSearchYouTube_live(t *testing.T) {
	results, err := SearchYouTube("morcheeba even though", 3)
	if err != nil {
		t.Fatalf("search failed: %v", err)
	}
	if len(results) == 0 {
		t.Fatal("no results returned")
	}
	for i, r := range results {
		t.Logf("%d. %s — %s", i+1, r.Title, r.URL)
	}
}
