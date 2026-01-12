package main

import (
	"bufio"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"log"
	"math/rand"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/charmbracelet/bubbles/progress"
	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"
	"github.com/fezcode/go-piml"
	"github.com/gopxl/beep"
	"github.com/gopxl/beep/effects"
	"github.com/gopxl/beep/speaker"
	"github.com/kkdai/youtube/v2"
)

const (
	sampleRate = 44100
)

type LyricLine struct {
	Time time.Duration
	Text string
}

type Track struct {
	URL  string `piml:"url"`
	Name string `piml:"name"`
}

type PimlPlaylist struct {
	Videos []Track `piml:"videos"`
}

type model struct {
	queue          []Track
	currentIndex   int
	client         *youtube.Client
	video          *youtube.Video
	streamURL      string
	progress       progress.Model
	currTime       time.Duration
	totalTime      time.Duration
	paused         bool
	loading        bool
	err            error
	quitting       bool
	ffmpegCmd      *exec.Cmd
	ffmpegStdout   io.ReadCloser
	ctrl           *beep.Ctrl
	volume         *effects.Volume
	volLevel       int // 0 to 100
	prevVolLevel   int // To restore after unmute
	streamer       *pcmStreamer
	mu             sync.Mutex
	showLyrics     bool
	lyrics         []LyricLine
	lyricsLoading  bool
	repeatOne      bool
	showPlaylist   bool
	manualLyricIdx int
	playlistCursor int
}

type tickMsg time.Time
type videoLoadedMsg struct {
	video     *youtube.Video
	streamURL string
	url       string
}
type lyricsMsg struct {
	lyrics []LyricLine
	err    error
	url    string
}

func (m *model) Init() tea.Cmd {
	return tea.Batch(
		m.loadVideo(m.queue[m.currentIndex].URL),
		m.tick(),
	)
}

func (m *model) loadVideo(urlStr string) tea.Cmd {
	return func() tea.Msg {
		video, err := m.client.GetVideo(urlStr)
		if err != nil {
			return errMsg{err}
		}

		formats := video.Formats.Select(func(f youtube.Format) bool {
			return f.AudioChannels > 0 && strings.HasPrefix(f.MimeType, "audio/")
		})
		if len(formats) == 0 {
			formats = video.Formats.WithAudioChannels()
		}
		if len(formats) == 0 {
			return errMsg{fmt.Errorf("no audio formats found")}
		}

		streamURL, err := m.client.GetStreamURL(video, &formats[0])
		if err != nil {
			return errMsg{err}
		}

		return videoLoadedMsg{
			video:     video,
			streamURL: streamURL,
			url:       urlStr,
		}
	}
}

func (m *model) fetchLyrics(video *youtube.Video, videoURL string) tea.Cmd {
	return func() tea.Msg {
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
			return lyricsMsg{err: err, url: videoURL}
		}
		defer resp.Body.Close()

		if resp.StatusCode == http.StatusNotFound {
			msg := m.searchFallback(author + " " + title)
			if lMsg, ok := msg.(lyricsMsg); ok {
				lMsg.url = videoURL
				return lMsg
			}
			return msg
		}

		var res struct {
			SyncedLyrics string `json:"syncedLyrics"`
			PlainLyrics  string `json:"plainLyrics"`
		}
		if err := json.NewDecoder(resp.Body).Decode(&res); err != nil {
			return lyricsMsg{err: err, url: videoURL}
		}

		if res.SyncedLyrics != "" {
			return lyricsMsg{lyrics: parseLRC(res.SyncedLyrics), url: videoURL}
		} else if res.PlainLyrics != "" {
			return lyricsMsg{lyrics: []LyricLine{{Time: 0, Text: "[Not Synced]\n" + res.PlainLyrics}}, url: videoURL}
		}

		return lyricsMsg{err: fmt.Errorf("no lyrics found"), url: videoURL}
	}
}

func (m *model) searchFallback(query string) tea.Msg {
	apiURL := fmt.Sprintf("https://lrclib.net/api/search?q=%s", url.QueryEscape(query))
	resp, err := http.Get(apiURL)
	if err != nil {
		return lyricsMsg{err: err}
	}
	defer resp.Body.Close()

	var results []struct {
		SyncedLyrics string `json:"syncedLyrics"`
		PlainLyrics  string `json:"plainLyrics"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&results); err != nil {
		return lyricsMsg{err: err}
	}

	for _, res := range results {
		if res.SyncedLyrics != "" {
			return lyricsMsg{lyrics: parseLRC(res.SyncedLyrics)}
		}
	}
	if len(results) > 0 && results[0].PlainLyrics != "" {
		return lyricsMsg{lyrics: []LyricLine{{Time: 0, Text: "[Not Synced]\n" + results[0].PlainLyrics}}}
	}

	return lyricsMsg{err: fmt.Errorf("no lyrics found")}
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

func (m *model) nextVideo() tea.Cmd {
	if m.repeatOne {
		return m.startPlayback(0)
	}

	nextIdx := m.currentIndex + 1
	if nextIdx >= len(m.queue) {
		nextIdx = 0
	}
	return m.gotoTrack(nextIdx)
}

func (m *model) prevVideo() tea.Cmd {
	if m.currTime > 3*time.Second {
		return m.startPlayback(0)
	}

	prevIdx := m.currentIndex - 1
	if prevIdx < 0 {
		prevIdx = len(m.queue) - 1
	}
	return m.gotoTrack(prevIdx)
}

func (m *model) gotoTrack(index int) tea.Cmd {
	m.currentIndex = index
	m.video = nil
	m.loading = true
	m.currTime = 0
	m.totalTime = 0
	m.lyrics = nil
	m.manualLyricIdx = 0
	return m.loadVideo(m.queue[m.currentIndex].URL)
}

type quittingMsg struct{}

func (m *model) tick() tea.Cmd {
	return tea.Tick(time.Millisecond*500, func(t time.Time) tea.Msg {
		return tickMsg(t)
	})
}

func (m *model) startPlayback(startAt time.Duration) tea.Cmd {
	return func() tea.Msg {
		m.mu.Lock()
		defer m.mu.Unlock()

		if m.ffmpegCmd != nil {
			m.ffmpegCmd.Process.Kill()
			_ = m.ffmpegCmd.Wait()
		}
		if m.ffmpegStdout != nil {
			_ = m.ffmpegStdout.Close()
		}

		startTime := fmt.Sprintf("%.3f", startAt.Seconds())
		cmd := exec.Command("ffmpeg",
			"-ss", startTime,
			"-i", m.streamURL,
			"-f", "s16le",
			"-ac", "2",
			"-ar", "44100",
			"-acodec", "pcm_s16le",
			"pipe:1",
		)
		cmd.Stderr = nil
		stdout, err := cmd.StdoutPipe()
		if err != nil {
			return errMsg{err}
		}

		if err := cmd.Start(); err != nil {
			return errMsg{err}
		}

		m.ffmpegCmd = cmd
		m.ffmpegStdout = stdout
		m.currTime = startAt

		speaker.Lock()
		m.streamer.Lock()
		m.streamer.reader = stdout
		m.streamer.Unlock()
		m.ctrl.Paused = m.paused
		speaker.Unlock()

		return nil
	}
}

func (m *model) updateVolume() {
	speaker.Lock()
	if m.volLevel == 0 {
		m.volume.Silent = true
	} else {
		m.volume.Silent = false
		if m.volLevel == 50 {
			m.volume.Volume = 0
		} else if m.volLevel < 50 {
			m.volume.Volume = (float64(m.volLevel-50) / 49.0) * 5.0
		} else {
			m.volume.Volume = (float64(m.volLevel-50) / 50.0) * 1.0
		}
	}
	speaker.Unlock()
}

type errMsg struct{ err error }

func (m *model) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	switch msg := msg.(type) {
	case videoLoadedMsg:
		if msg.url != m.queue[m.currentIndex].URL {
			return m, nil
		}
		m.video = msg.video
		m.streamURL = msg.streamURL
		m.totalTime = msg.video.Duration
		m.loading = false
		m.lyricsLoading = true
		return m, tea.Batch(
			m.startPlayback(0),
			m.fetchLyrics(msg.video, msg.url),
		)

	case lyricsMsg:
		if msg.url != m.queue[m.currentIndex].URL {
			return m, nil
		}
		m.lyricsLoading = false
		if msg.err == nil {
			m.lyrics = msg.lyrics
		}
		return m, nil

	case tea.KeyMsg:
		switch msg.String() {
		case "q", "ctrl+c":
			m.quitting = true
			m.mu.Lock()
			if m.ffmpegCmd != nil {
				m.ffmpegCmd.Process.Kill()
			}
			m.mu.Unlock()
			return m, tea.Quit
		case " ":
			if m.loading {
				return m, nil
			}
			m.paused = !m.paused
			speaker.Lock()
			m.ctrl.Paused = m.paused
			speaker.Unlock()
			return m, nil
		case "n":
			return m, m.nextVideo()
		case "p":
			return m, m.prevVideo()
		case "l":
			m.showLyrics = !m.showLyrics
			return m, nil
		case "a":
			if m.showLyrics && len(m.lyrics) == 1 && strings.HasPrefix(m.lyrics[0].Text, "[Not Synced]") {
				if m.manualLyricIdx > 0 {
					m.manualLyricIdx--
				}
			}
			return m, nil
		case "d":
			if m.showLyrics && len(m.lyrics) == 1 && strings.HasPrefix(m.lyrics[0].Text, "[Not Synced]") {
				lines := strings.Split(m.lyrics[0].Text, "\n")
				if m.manualLyricIdx < len(lines)-1 {
					m.manualLyricIdx++
				}
			}
			return m, nil
		case "r":
			rand.Shuffle(len(m.queue), func(i, j int) {
				m.queue[i], m.queue[j] = m.queue[j], m.queue[i]
			})
			m.currentIndex = 0
			m.video = nil
			m.loading = true
			m.currTime = 0
			m.totalTime = 0
			m.lyrics = nil
			m.manualLyricIdx = 0
			return m, m.loadVideo(m.queue[0].URL)
		case "t":
			m.repeatOne = !m.repeatOne
			return m, nil
		case "v":
			m.showPlaylist = !m.showPlaylist
			if m.showPlaylist {
				m.playlistCursor = m.currentIndex
			}
			return m, nil
		case "up":
			if m.showPlaylist {
				m.playlistCursor--
				if m.playlistCursor < 0 {
					m.playlistCursor = len(m.queue) - 1
				}
				return m, nil
			}
		case "down":
			if m.showPlaylist {
				m.playlistCursor++
				if m.playlistCursor >= len(m.queue) {
					m.playlistCursor = 0
				}
				return m, nil
			}
		case "enter":
			if m.showPlaylist {
				return m, m.gotoTrack(m.playlistCursor)
			}
		case "m":
			if m.volLevel > 0 {
				m.prevVolLevel = m.volLevel
				m.volLevel = 0
			} else {
				if m.prevVolLevel == 0 {
					m.volLevel = 50
				} else {
					m.volLevel = m.prevVolLevel
				}
			}
			m.updateVolume()
			return m, nil
		case "+", "=":
			m.volLevel += 5
			if m.volLevel > 100 {
				m.volLevel = 100
			}
			m.updateVolume()
			return m, nil
		case "-":
			m.volLevel -= 5
			if m.volLevel < 0 {
				m.volLevel = 0
			}
			m.updateVolume()
			return m, nil
		case "right":
			if m.loading {
				return m, nil
			}
			newTime := m.currTime + 10*time.Second
			if newTime > m.totalTime {
				newTime = m.totalTime
			}
			return m, m.startPlayback(newTime)
		case "left":
			if m.loading {
				return m, nil
			}
			newTime := m.currTime - 10*time.Second
			if newTime < 0 {
				newTime = 0
			}
			return m, m.startPlayback(newTime)
		}

	case tickMsg:
		if !m.paused && !m.quitting && !m.loading && m.video != nil {
			m.currTime += time.Millisecond * 500
			if m.currTime >= m.totalTime {
				return m, m.nextVideo()
			}
		}
		return m, m.tick()

	case quittingMsg:
		m.quitting = true
		return m, tea.Quit

	case errMsg:
		m.err = msg.err
		return m, tea.Quit

	case tea.WindowSizeMsg:
		m.progress.Width = msg.Width - 10
		return m, nil
	}

	return m, nil
}

var (
	titleStyle    = lipgloss.NewStyle().Bold(true).Foreground(lipgloss.Color("#FFD700"))
	authorStyle   = lipgloss.NewStyle().Foreground(lipgloss.Color("#C0C0C0"))
	helpStyle     = lipgloss.NewStyle().Foreground(lipgloss.Color("#626262")).MarginTop(1)
	queueStyle    = lipgloss.NewStyle().Foreground(lipgloss.Color("#87CEEB"))
	lyricsStyle   = lipgloss.NewStyle().Foreground(lipgloss.Color("#AAAAAA")).Italic(true)
	currentStyle  = lipgloss.NewStyle().Foreground(lipgloss.Color("#00FF00")).Bold(true)
	playlistStyle = lipgloss.NewStyle().Foreground(lipgloss.Color("#6A5ACD"))
	volumeStyle   = lipgloss.NewStyle().Foreground(lipgloss.Color("#FF69B4"))
	cursorStyle   = lipgloss.NewStyle().Foreground(lipgloss.Color("#FFFF00")).Bold(true)
)

func (m *model) View() string {
	if m.err != nil {
		return fmt.Sprintf("Error: %v\n", m.err)
	}

	if m.quitting {
		return "Goodbye!\n"
	}

	if m.loading || m.video == nil {
		return fmt.Sprintf("\n  Loading video %d/%d...\n  %s", m.currentIndex+1, len(m.queue), m.queue[m.currentIndex].URL)
	}

	statusStr := "PLAYING"
	if m.paused {
		statusStr = "PAUSED"
	}
	if m.repeatOne {
		statusStr += " [LOOP-1]"
	}

	volStr := fmt.Sprintf("%d%%", m.volLevel)
	if m.volLevel == 0 {
		volStr = "MUTE"
	}

	percent := float64(m.currTime) / float64(m.totalTime)
	if percent > 1.0 {
		percent = 1.0
	}

	s := fmt.Sprintf(
		"\n  %s\n  %s\n  %s\n\n  %s %s / %s  %s\n\n  %s\n",
		queueStyle.Render(fmt.Sprintf("Track %d/%d", m.currentIndex+1, len(m.queue))),
		titleStyle.Render(m.video.Title),
		authorStyle.Render(m.video.Author),
		statusStr,
		formatDuration(m.currTime),
		formatDuration(m.totalTime),
		volumeStyle.Render("VOL: "+volStr),
		m.progress.ViewAs(percent),
	)

	if m.showLyrics {
		s += "\n  LYRICS:\n"
		if m.lyricsLoading {
			s += "  Loading lyrics...\n"
		} else if len(m.lyrics) == 0 {
			s += "  No lyrics found.\n"
		} else if len(m.lyrics) == 1 && strings.HasPrefix(m.lyrics[0].Text, "[Not Synced]") {
			lines := strings.Split(m.lyrics[0].Text, "\n")
			start := m.manualLyricIdx - 2
			if start < 0 {
				start = 0
			}
			end := start + 5
			if end > len(lines) {
				end = len(lines)
			}
			for i := start; i < end; i++ {
				line := lines[i]
				if i == m.manualLyricIdx {
					s += "  " + currentStyle.Render("> "+line) + "\n"
				} else {
					s += "  " + lyricsStyle.Render("  "+line) + "\n"
				}
			}
		} else {
			currentIndex := -1
			adjustedTime := m.currTime + 200*time.Millisecond
			for i, line := range m.lyrics {
				if adjustedTime >= line.Time {
					currentIndex = i
				} else {
					break
				}
			}

			start := currentIndex - 2
			if start < 0 {
				start = 0
			}
			end := start + 5
			if end > len(m.lyrics) {
				end = len(m.lyrics)
			}

			for i := start; i < end; i++ {
				lineText := m.lyrics[i].Text
				if i == currentIndex {
					s += "  " + currentStyle.Render("> "+lineText) + "\n"
				} else {
					s += "  " + lyricsStyle.Render("  "+lineText) + "\n"
				}
			}
		}
	}

	if m.showPlaylist {
		s += "\n  PLAYLIST:\n"
		// Show a window of 7 items centered on playlistCursor
		start := m.playlistCursor - 3
		if start < 0 {
			start = 0
		}
		end := start + 7
		if end > len(m.queue) {
			end = len(m.queue)
			start = end - 7
			if start < 0 {
				start = 0
			}
		}

		if start > 0 {
			s += "  ...\n"
		}

		for i := start; i < end; i++ {
			prefix := "  "
			if i == m.playlistCursor {
				prefix = "> "
			}
			
			displayName := m.queue[i].Name
			if displayName == "" {
				displayName = m.queue[i].URL
			}
			
			line := fmt.Sprintf("%s%d. %s", prefix, i+1, displayName)
			if i == m.currentIndex {
				line += " (playing)"
			}

			if i == m.playlistCursor {
				s += cursorStyle.Render(line) + "\n"
			} else if i == m.currentIndex {
				s += currentStyle.Render(line) + "\n"
			} else {
				s += playlistStyle.Render(line) + "\n"
			}
		}

		if end < len(m.queue) {
			s += "  ...\n"
		}
	}

	s += "\n" + helpStyle.Render("Space: Pause • +/-: Vol • M: Mute • P: Prev • N: Next • L: Lyrics (A/D: Scroll) • R: Randomize • T: Loop-1 • V: Playlist (Up/Down: Select, Enter: Play) • Q: Quit")

	return s
}

func formatDuration(d time.Duration) string {
	d = d.Round(time.Second)
	h := d / time.Hour
	d -= h * time.Hour
	m := d / time.Minute
	d -= m * time.Minute
	s := d / time.Second

	if h > 0 {
		return fmt.Sprintf("%02d:%02d:%02d", h, m, s)
	}
	return fmt.Sprintf("%02d:%02d", m, s)
}

func main() {
	rand.Seed(time.Now().UnixNano())
	filePath := flag.String("file", "", "Path to a file containing YouTube URLs (one per line) or .piml file")
	flag.Parse()

	if _, err := exec.LookPath("ffmpeg"); err != nil {
		fmt.Println("Error: ffmpeg is not installed or not in your PATH.")
		fmt.Println("FFmpeg is required to decode YouTube's audio formats (Opus/AAC).")
		os.Exit(1)
	}

	var queue []Track
	if *filePath != "" {
		ext := strings.ToLower(filepath.Ext(*filePath))
		if ext == ".piml" {
			data, err := os.ReadFile(*filePath)
			if err != nil {
				log.Fatalf("Error reading piml file: %v", err)
			}
			var pimlData PimlPlaylist
			if err := piml.Unmarshal(data, &pimlData); err != nil {
				log.Fatalf("Error unmarshaling piml: %v", err)
			}
			queue = pimlData.Videos
		} else {
			file, err := os.Open(*filePath)
			if err != nil {
				log.Fatalf("Error opening file: %v", err)
			}
			defer file.Close()

			scanner := bufio.NewScanner(file)
			for scanner.Scan() {
				line := strings.TrimSpace(scanner.Text())
				if line != "" && !strings.HasPrefix(line, "#") {
					queue = append(queue, Track{URL: line})
				}
			}
		}
	}

	for _, arg := range flag.Args() {
		queue = append(queue, Track{URL: arg})
	}

	if len(queue) == 0 {
		fmt.Println("Usage: yt-audio-player [--file <path>] <youtube-url1> [youtube-url2] ...")
		return
	}

	client := youtube.Client{}

	sr := beep.SampleRate(sampleRate)
	err := speaker.Init(sr, sr.N(time.Second/10))
	if err != nil {
		log.Fatalf("Error initializing speaker: %v", err)
	}

	streamer := &pcmStreamer{}
	ctrl := &beep.Ctrl{Streamer: streamer, Paused: false}
	volume := &effects.Volume{Streamer: ctrl, Base: 2, Volume: 0, Silent: false}
	speaker.Play(volume)

	m := model{
		queue:    queue,
		client:   &client,
		progress: progress.New(progress.WithGradient("#7300ab", "#0087ff")),
		streamer: streamer,
		ctrl:     ctrl,
		volume:   volume,
		volLevel: 50,
		loading:  true,
	}

	p := tea.NewProgram(&m, tea.WithAltScreen())
	if _, err := p.Run(); err != nil {
		fmt.Printf("Error running program: %v", err)
		os.Exit(1)
	}
}

type pcmStreamer struct {
	reader io.Reader
	mu     sync.Mutex
}

func (p *pcmStreamer) Lock()   { p.mu.Lock() }
func (p *pcmStreamer) Unlock() { p.mu.Unlock() }

func (p *pcmStreamer) Stream(samples [][2]float64) (n int, ok bool) {
	p.mu.Lock()
	defer p.mu.Unlock()

	if p.reader == nil {
		for i := range samples {
			samples[i] = [2]float64{0, 0}
		}
		return len(samples), true
	}

	var buf [4]byte
	for i := range samples {
		_, err := io.ReadFull(p.reader, buf[:])
		if err != nil {
			samples[i] = [2]float64{0, 0}
			continue
		}
		left := int16(buf[0]) | int16(buf[1])<<8
		right := int16(buf[2]) | int16(buf[3])<<8
		samples[i][0] = float64(left) / 32768.0
		samples[i][1] = float64(right) / 32768.0
		n++
	}
	return len(samples), true
}

func (p *pcmStreamer) Err() error { return nil }