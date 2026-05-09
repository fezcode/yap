package main

import (
	"fmt"
	"math/rand"
	"strings"
	"sync"
	"time"

	"github.com/charmbracelet/bubbles/progress"
	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"
	"github.com/gopxl/beep"
	"github.com/gopxl/beep/effects"
	"github.com/gopxl/beep/speaker"
	"github.com/kkdai/youtube/v2"
)

type model struct {
	queue          []Track
	currentIndex   int
	client         *youtube.Client
	video          *youtube.Video
	selectedFormat *youtube.Format
	progress       progress.Model
	currTime       time.Duration
	totalTime      time.Duration
	paused         bool
	loading        bool
	err            error
	quitting       bool
	ctrl           *beep.Ctrl
	volume         *effects.Volume
	volLevel       int // 0 to 100
	prevVolLevel   int // To restore after unmute
	streamer       *OpusStreamer
	mu             sync.Mutex
	showLyrics     bool
	lyrics         []LyricLine
	lyricsLoading  bool
	showPlaylist   bool
	manualLyricIdx int
	playlistCursor int
}

type tickMsg time.Time
type videoLoadedMsg struct {
	video          *youtube.Video
	selectedFormat *youtube.Format
	url            string
}
type lyricsMsg struct {
	lyrics []LyricLine
	err    error
	url    string
}
type errMsg struct{ err error }

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

		if debugLog != nil {
			debugLog.Printf("LOADVIDEO: %d formats available for %q", len(video.Formats), video.Title)
			for i := range video.Formats {
				f := &video.Formats[i]
				debugLog.Printf("  [%d] itag=%d mime=%q channels=%d bitrate=%d",
					i, f.ItagNo, f.MimeType, f.AudioChannels, f.Bitrate)
			}
		}

		// The audio pipeline only handles WebM/Opus — try opus itags in order
		// of quality (251 = ~160kbps, 250 = ~70kbps, 249 = ~50kbps), then any
		// format whose MIME type advertises opus.
		var selectedFormat *youtube.Format
		for _, want := range []int{251, 250, 249} {
			for i := range video.Formats {
				f := &video.Formats[i]
				if f.ItagNo == want {
					selectedFormat = f
					break
				}
			}
			if selectedFormat != nil {
				break
			}
		}
		if selectedFormat == nil {
			for i := range video.Formats {
				f := &video.Formats[i]
				if f.AudioChannels > 0 &&
					(strings.Contains(f.MimeType, "opus") || strings.Contains(f.MimeType, "webm")) {
					selectedFormat = f
					break
				}
			}
		}

		if selectedFormat == nil {
			return errMsg{fmt.Errorf("no Opus/WebM audio format available for this video (only Opus/WebM is supported)")}
		}

		if debugLog != nil {
			debugLog.Printf("LOADVIDEO: selected itag=%d mime=%q bitrate=%d",
				selectedFormat.ItagNo, selectedFormat.MimeType, selectedFormat.Bitrate)
		}

		return videoLoadedMsg{
			video:          video,
			selectedFormat: selectedFormat,
			url:            urlStr,
		}
	}
}

func (m *model) fetchLyricsCmd(video *youtube.Video, videoURL string) tea.Cmd {
	return func() tea.Msg {
		lyrics, err := FetchLyrics(video)
		return lyricsMsg{lyrics: lyrics, err: err, url: videoURL}
	}
}

type playbackStartedMsg struct {
	startTime time.Duration
}

func (m *model) nextVideo() tea.Cmd {
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
	m.selectedFormat = nil
	m.loading = true
	m.currTime = 0
	m.totalTime = 0
	m.lyrics = nil
	m.manualLyricIdx = 0
	return m.loadVideo(m.queue[m.currentIndex].URL)
}

func (m *model) tick() tea.Cmd {
	return tea.Tick(time.Millisecond*500, func(t time.Time) tea.Msg {
		return tickMsg(t)
	})
}

func (m *model) startPlayback(startAt time.Duration) tea.Cmd {
	return func() tea.Msg {
		m.mu.Lock()
		defer m.mu.Unlock()

		m.streamer.Start(m.video, m.selectedFormat, startAt)

		if debugLog != nil {
			debugLog.Printf("UI: startPlayback startAt=%s, before speaker.Play", startAt)
		}
		// speaker.Clear/Play each take the speaker mutex internally — calling
		// them inside speaker.Lock()/Unlock() deadlocks because beep's mutex
		// is non-reentrant.
		speaker.Clear()
		speaker.Play(m.volume)
		speaker.Lock()
		m.ctrl.Paused = m.paused
		speaker.Unlock()
		if debugLog != nil {
			debugLog.Printf("UI: startPlayback after speaker.Play paused=%v", m.paused)
		}

		return playbackStartedMsg{startTime: startAt}
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

func (m *model) updateTitle() tea.Cmd {
	if m.video == nil {
		return tea.SetWindowTitle("atlas.yap - Loading...")
	}
	status := "PLAYING"
	if m.loading {
		status = "BUFFERING"
	} else if m.paused {
		status = "PAUSED"
	}
	return tea.SetWindowTitle(fmt.Sprintf("atlas.yap - %s [%s]", status, m.video.Title))
}

func (m *model) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	switch msg := msg.(type) {
	case videoLoadedMsg:
		if msg.url != m.queue[m.currentIndex].URL {
			return m, nil
		}
		m.video = msg.video
		m.selectedFormat = msg.selectedFormat
		m.totalTime = msg.video.Duration
		m.loading = false
		m.lyricsLoading = true
		return m, tea.Batch(
			m.startPlayback(0),
			m.fetchLyricsCmd(msg.video, msg.url),
			m.updateTitle(),
		)

	case playbackStartedMsg:
		m.currTime = msg.startTime
		m.loading = false
		return m, m.updateTitle()

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
			m.streamer.Close()
			return m, tea.Quit
		case " ":
			if m.loading {
				return m, nil
			}
			m.paused = !m.paused
			speaker.Lock()
			m.ctrl.Paused = m.paused
			speaker.Unlock()
			return m, m.updateTitle()
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
			return m, tea.Batch(m.gotoTrack(0), m.updateTitle())
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
				return m, tea.Batch(m.gotoTrack(m.playlistCursor), m.updateTitle())
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
			if m.loading || m.video == nil {
				return m, nil
			}
			newTime := m.currTime + 10*time.Second
			if newTime > m.totalTime {
				newTime = m.totalTime
			}
			m.loading = true
			m.currTime = newTime
			return m, tea.Batch(m.startPlayback(newTime), m.updateTitle())
		case "left":
			if m.loading || m.video == nil {
				return m, nil
			}
			newTime := m.currTime - 10*time.Second
			if newTime < 0 {
				newTime = 0
			}
			m.loading = true
			m.currTime = newTime
			return m, tea.Batch(m.startPlayback(newTime), m.updateTitle())
		}

	case tickMsg:
		if !m.paused && !m.quitting && !m.loading && m.video != nil {
			m.currTime += time.Millisecond * 500
			if m.currTime >= m.totalTime {
				return m, m.nextVideo()
			}
		}
		return m, m.tick()

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

	// Only flicker to loading screen if we don't have metadata yet.
	if m.video == nil {
		return fmt.Sprintf("\n  Loading video %d/%d...\n  %s", m.currentIndex+1, len(m.queue), m.queue[m.currentIndex].URL)
	}

	statusStr := "PLAYING"
	if m.loading {
		statusStr = "BUFFERING"
	} else if m.paused {
		statusStr = "PAUSED"
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
		s += "\n  LYRICS (provided by LRCLib):\n"
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

	s += "\n" + helpStyle.Render("Space: Pause • ←/→: Seek 10s • +/-: Vol • M: Mute • P: Prev • N: Next • L: Lyrics (A/D: Scroll) • R: Randomize • V: Playlist (Up/Down: Select, Enter: Play) • Q: Quit")

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
