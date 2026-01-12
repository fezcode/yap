package main

import (
	"flag"
	"fmt"
	"log"
	"math/rand"
	"os"
	"os/exec"
	"time"

	"github.com/charmbracelet/bubbles/progress"
	tea "github.com/charmbracelet/bubbletea"
	"github.com/gopxl/beep"
	"github.com/gopxl/beep/effects"
	"github.com/gopxl/beep/speaker"
	"github.com/kkdai/youtube/v2"
)

const (
	sampleRate = 44100
)

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
		var err error
		queue, err = LoadPlaylist(*filePath)
		if err != nil {
			log.Fatalf("Error loading playlist: %v", err)
		}
	}

	for _, arg := range flag.Args() {
		queue = append(queue, Track{URL: arg})
	}

	if len(queue) == 0 {
		fmt.Println("Usage: yap [--file <path>] <youtube-url1> [youtube-url2] ...")
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
