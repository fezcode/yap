package main

import (
	"bufio"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/kkdai/youtube/v2"
)

// LyricsSource is a single lyrics provider. Fetch returns lines that may be
// synced (per-line timestamps) or one big "[Not Synced]\n…" blob when only
// plain text is available.
type LyricsSource struct {
	Name  string
	Fetch func(video *youtube.Video) ([]LyricLine, error)
}

// LyricsSources is the ordered list of providers users can cycle through.
// Order matters — LRCLib first because it usually has synced lyrics.
var LyricsSources = []LyricsSource{
	{Name: "LRCLib", Fetch: fetchLRCLib},
	{Name: "NetEase", Fetch: fetchNetEase},
	{Name: "Lyrics.ovh", Fetch: fetchLyricsOVH},
}

var noiseRe = regexp.MustCompile(`(?i)\(official video\)|\(official music video\)|\(official audio\)|\(lyric video\)|\(lyrics\)|\(audio\)|\[official video\]|\[official music video\]|\[official audio\]|\[lyric video\]|\[lyrics\]|\[audio\]|\(official\)|\(hd\)|\(4k\)|\[official\]|\[hd\]|\[4k\]|video official|official video|music video|official music video`)

func cleanTitle(t string) string {
	return strings.TrimSpace(noiseRe.ReplaceAllString(t, ""))
}

func cleanAuthor(a string) string {
	a = strings.TrimSuffix(a, " - Topic")
	a = strings.TrimSuffix(a, " VEVO")
	a = strings.TrimSuffix(a, "VEVO")
	return strings.TrimSpace(a)
}

// ---------- LRCLib ----------

func fetchLRCLib(video *youtube.Video) ([]LyricLine, error) {
	title := cleanTitle(video.Title)
	author := cleanAuthor(video.Author)

	apiURL := fmt.Sprintf("https://lrclib.net/api/get?artist_name=%s&track_name=%s&duration=%d",
		url.QueryEscape(author),
		url.QueryEscape(title),
		int(video.Duration.Seconds()),
	)

	resp, err := http.Get(apiURL)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	if resp.StatusCode == http.StatusNotFound {
		return lrcLibSearchFallback(author + " " + title)
	}

	var res struct {
		SyncedLyrics string `json:"syncedLyrics"`
		PlainLyrics  string `json:"plainLyrics"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&res); err != nil {
		return nil, err
	}

	if res.SyncedLyrics != "" {
		return parseLRC(res.SyncedLyrics), nil
	} else if res.PlainLyrics != "" {
		return []LyricLine{{Time: 0, Text: "[Not Synced]\n" + res.PlainLyrics}}, nil
	}

	return nil, fmt.Errorf("no lyrics found")
}

func lrcLibSearchFallback(query string) ([]LyricLine, error) {
	apiURL := fmt.Sprintf("https://lrclib.net/api/search?q=%s", url.QueryEscape(query))
	resp, err := http.Get(apiURL)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	var results []struct {
		SyncedLyrics string `json:"syncedLyrics"`
		PlainLyrics  string `json:"plainLyrics"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&results); err != nil {
		return nil, err
	}

	for _, res := range results {
		if res.SyncedLyrics != "" {
			return parseLRC(res.SyncedLyrics), nil
		}
	}
	if len(results) > 0 && results[0].PlainLyrics != "" {
		return []LyricLine{{Time: 0, Text: "[Not Synced]\n" + results[0].PlainLyrics}}, nil
	}

	return nil, fmt.Errorf("no lyrics found")
}

// ---------- Lyrics.ovh (plain text only, no synced) ----------

func fetchLyricsOVH(video *youtube.Video) ([]LyricLine, error) {
	title := cleanTitle(video.Title)
	author := cleanAuthor(video.Author)

	// lyrics.ovh wants /v1/{artist}/{title}; fall back to splitting "Artist - Title"
	// from the YouTube title when author is generic (e.g. uploader is a channel).
	if author == "" || strings.EqualFold(author, "various artists") {
		if a, t, ok := splitArtistTitle(title); ok {
			author, title = a, t
		}
	}

	apiURL := fmt.Sprintf("https://api.lyrics.ovh/v1/%s/%s",
		url.PathEscape(author),
		url.PathEscape(title),
	)

	resp, err := http.Get(apiURL)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		// Retry once with the YouTube title split, in case our author guess was wrong.
		if a, t, ok := splitArtistTitle(video.Title); ok && (a != author || t != title) {
			retryURL := fmt.Sprintf("https://api.lyrics.ovh/v1/%s/%s",
				url.PathEscape(a), url.PathEscape(t))
			resp2, err2 := http.Get(retryURL)
			if err2 == nil {
				defer resp2.Body.Close()
				if resp2.StatusCode == http.StatusOK {
					return decodeOVH(resp2.Body)
				}
			}
		}
		return nil, fmt.Errorf("lyrics.ovh returned %s", resp.Status)
	}

	return decodeOVH(resp.Body)
}

func decodeOVH(r io.Reader) ([]LyricLine, error) {
	var res struct {
		Lyrics string `json:"lyrics"`
		Error  string `json:"error"`
	}
	if err := json.NewDecoder(r).Decode(&res); err != nil {
		return nil, err
	}
	if res.Error != "" {
		return nil, fmt.Errorf("%s", res.Error)
	}
	if strings.TrimSpace(res.Lyrics) == "" {
		return nil, fmt.Errorf("no lyrics found")
	}
	return []LyricLine{{Time: 0, Text: "[Not Synced]\n" + strings.TrimSpace(res.Lyrics)}}, nil
}

// splitArtistTitle parses YouTube-style "Artist - Title" headers into their
// two pieces. Returns ok=false when no separator is present.
func splitArtistTitle(s string) (artist, title string, ok bool) {
	for _, sep := range []string{" - ", " – ", " — "} {
		if i := strings.Index(s, sep); i > 0 {
			return strings.TrimSpace(s[:i]), strings.TrimSpace(s[i+len(sep):]), true
		}
	}
	return "", "", false
}

// ---------- NetEase Cloud Music ----------
//
// Two-step: search by query → take first songId → fetch lyric. NetEase
// usually has synced LRC for popular tracks (and very strong coverage for
// CN/JP/KR catalog).

func fetchNetEase(video *youtube.Video) ([]LyricLine, error) {
	title := cleanTitle(video.Title)
	author := cleanAuthor(video.Author)
	query := strings.TrimSpace(author + " " + title)
	if query == "" {
		return nil, fmt.Errorf("empty query")
	}

	searchURL := fmt.Sprintf("https://music.163.com/api/search/get/web?s=%s&type=1&offset=0&total=true&limit=1",
		url.QueryEscape(query))
	req, _ := http.NewRequest("GET", searchURL, nil)
	req.Header.Set("Referer", "https://music.163.com/")
	req.Header.Set("User-Agent", "Mozilla/5.0")

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	var sr struct {
		Result struct {
			Songs []struct {
				ID int64 `json:"id"`
			} `json:"songs"`
		} `json:"result"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&sr); err != nil {
		return nil, err
	}
	if len(sr.Result.Songs) == 0 {
		return nil, fmt.Errorf("no NetEase results")
	}
	id := sr.Result.Songs[0].ID

	lyricURL := fmt.Sprintf("https://music.163.com/api/song/lyric?id=%d&lv=1&kv=1&tv=-1", id)
	req2, _ := http.NewRequest("GET", lyricURL, nil)
	req2.Header.Set("Referer", "https://music.163.com/")
	req2.Header.Set("User-Agent", "Mozilla/5.0")

	resp2, err := http.DefaultClient.Do(req2)
	if err != nil {
		return nil, err
	}
	defer resp2.Body.Close()

	var lr struct {
		Lrc struct {
			Lyric string `json:"lyric"`
		} `json:"lrc"`
		Tlyric struct {
			Lyric string `json:"lyric"`
		} `json:"tlyric"`
	}
	if err := json.NewDecoder(resp2.Body).Decode(&lr); err != nil {
		return nil, err
	}

	if strings.TrimSpace(lr.Lrc.Lyric) == "" {
		return nil, fmt.Errorf("no NetEase lyrics for id=%d", id)
	}

	parsed := parseLRC(lr.Lrc.Lyric)
	if len(parsed) > 0 {
		return parsed, nil
	}
	// Some NetEase responses return non-timestamped plain text (older entries).
	return []LyricLine{{Time: 0, Text: "[Not Synced]\n" + strings.TrimSpace(lr.Lrc.Lyric)}}, nil
}

// ---------- Shared LRC parser ----------

func parseLRC(lrc string) []LyricLine {
	var lines []LyricLine
	re := regexp.MustCompile(`\[(\d+):(\d+)\.(\d+)\](.*)`)
	scanner := bufio.NewScanner(strings.NewReader(lrc))
	for scanner.Scan() {
		matches := re.FindStringSubmatch(scanner.Text())
		if len(matches) == 5 {
			min, _ := strconv.Atoi(matches[1])
			sec, _ := strconv.Atoi(matches[2])
			msecStr := matches[3]
			msec, _ := strconv.Atoi(msecStr)
			if len(msecStr) == 2 {
				msec *= 10
			}
			t := time.Duration(min)*time.Minute + time.Duration(sec)*time.Second + time.Duration(msec)*time.Millisecond
			lines = append(lines, LyricLine{
				Time: t,
				Text: strings.TrimSpace(matches[4]),
			})
		}
	}
	return lines
}
