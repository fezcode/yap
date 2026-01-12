package main

import (
	"bufio"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/kkdai/youtube/v2"
)

func FetchLyrics(video *youtube.Video) ([]LyricLine, error) {
	clean := func(t string) string {
		re := regexp.MustCompile(`(?i)\(official video\)|\(official music video\)|\(official audio\)|\(lyric video\)|\(lyrics\)|\(audio\)|\[official video\]|\[official music video\]|\[official audio\]|\[lyric video\]|\[lyrics\]|\[audio\]|\(official\)|\(hd\)|\(4k\)|\[official\]|\[hd\]|\[4k\]|video official|official video|music video|official music video`)
		t = re.ReplaceAllString(t, "")
		return strings.TrimSpace(t)
	}

	title := clean(video.Title)
	author := strings.TrimSuffix(strings.TrimSuffix(video.Author, " - Topic"), " VEVO")

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
		return searchFallback(author + " " + title)
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

func searchFallback(query string) ([]LyricLine, error) {
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
