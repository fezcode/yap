package main

import (
	"fmt"
	"io"
	"net/http"
	"net/url"
	"regexp"
	"strings"
)

type SearchResult struct {
	VideoID string
	Title   string
	URL     string
}

const userAgent = "Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 " +
	"(KHTML, like Gecko) Chrome/123.0.0.0 Safari/537.36"

// SearchYouTube fetches the YouTube search results page and extracts up to
// `limit` playable video IDs. It does not require an API key — it scrapes the
// videoId tokens out of the embedded ytInitialData JSON payload.
func SearchYouTube(query string, limit int) ([]SearchResult, error) {
	if limit <= 0 {
		limit = 1
	}
	q := url.Values{}
	q.Set("search_query", query)
	// sp=EgIQAQ%253D%253D restricts results to the "Videos" tab — filters out
	// channels, playlists, ads, and shelves we can't play directly.
	q.Set("sp", "EgIQAQ%3D%3D")

	req, err := http.NewRequest("GET", "https://www.youtube.com/results?"+q.Encode(), nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("User-Agent", userAgent)
	req.Header.Set("Accept-Language", "en-US,en;q=0.9")

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("youtube search returned %s", resp.Status)
	}

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, err
	}
	html := string(body)

	results := extractSearchResults(html, limit)
	if len(results) == 0 {
		return nil, fmt.Errorf("no playable results parsed from search page")
	}
	return results, nil
}

var (
	// videoRendererRe captures the videoId and one nearby title text run.
	// We scan the raw HTML/JSON without unmarshalling — YouTube's response
	// schema mutates frequently, so a tolerant regex pass is more durable.
	videoIDRe   = regexp.MustCompile(`"videoId":"([a-zA-Z0-9_-]{11})"`)
	titleRunRe  = regexp.MustCompile(`"title":\s*\{\s*"runs":\s*\[\s*\{\s*"text":\s*"((?:[^"\\]|\\.)*)"`)
	titleSimpleRe = regexp.MustCompile(`"title":\s*\{\s*"simpleText":\s*"((?:[^"\\]|\\.)*)"`)
)

func extractSearchResults(html string, limit int) []SearchResult {
	seen := map[string]struct{}{}
	out := make([]SearchResult, 0, limit)

	// Walk the HTML looking for videoId tokens, then look ahead a bounded
	// window for the closest title run — this keeps each result paired with
	// its own title even when the JSON schema reshuffles fields.
	idMatches := videoIDRe.FindAllStringSubmatchIndex(html, -1)
	for _, m := range idMatches {
		id := html[m[2]:m[3]]
		if _, ok := seen[id]; ok {
			continue
		}
		seen[id] = struct{}{}

		end := m[1] + 4096
		if end > len(html) {
			end = len(html)
		}
		window := html[m[1]:end]

		title := ""
		if t := titleRunRe.FindStringSubmatch(window); len(t) > 1 {
			title = unescapeJSONString(t[1])
		} else if t := titleSimpleRe.FindStringSubmatch(window); len(t) > 1 {
			title = unescapeJSONString(t[1])
		}

		out = append(out, SearchResult{
			VideoID: id,
			Title:   title,
			URL:     "https://www.youtube.com/watch?v=" + id,
		})
		if len(out) >= limit {
			break
		}
	}
	return out
}

func unescapeJSONString(s string) string {
	r := strings.NewReplacer(
		`\"`, `"`,
		`\\`, `\`,
		`\/`, `/`,
		`\n`, " ",
		`\t`, " ",
		`\r`, "",
	)
	return r.Replace(s)
}
