package main

import (
	"flag"
	"fmt"
	"log"
	"math/rand"
	"os"
	"path/filepath"
	"time"

	"github.com/charmbracelet/bubbles/progress"
	tea "github.com/charmbracelet/bubbletea"
	"github.com/gopxl/beep"
	"github.com/gopxl/beep/effects"
	"github.com/gopxl/beep/speaker"
	"github.com/kkdai/youtube/v2"
)

const (
)

var Version = "dev" // Overwritten by gobake build

func main() {
	if len(os.Args) > 1 && (os.Args[1] == "-v" || os.Args[1] == "--version") {
		fmt.Printf("yap v%s\n", Version)
		return
	}
	if len(os.Args) > 1 && (os.Args[1] == "-h" || os.Args[1] == "--help") {
		fmt.Println("yap - YouTube Audio Player TUI")
		fmt.Println("Usage: yap [--file <path>] <youtube-url1> [youtube-url2] ...")
		fmt.Println("Options:")
		fmt.Println("  --file string   Path to a file containing YouTube URLs (one per line) or .piml file")
		fmt.Println("  -v, --version   Show version")
		fmt.Println("  -h, --help      Show help")
		return
	}

	fmt.Println("Main starting...")
	home, _ := os.UserHomeDir()
	logPath := filepath.Join(home, fmt.Sprintf("yap_debug_%d.log", time.Now().Unix()))
	f, err := os.OpenFile(logPath, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0666)
	if err == nil {
		debugLog = log.New(f, "DEBUG: ", log.LstdFlags)
		debugLog.Printf("Application started - logging to %s\n", logPath)
		fmt.Printf("Logging to: %s\n", logPath)
	} else {
		fmt.Printf("Failed to open log file: %v\n", err)
	}

	rand.Seed(time.Now().UnixNano())
	filePath := flag.String("file", "", "Path to a file containing YouTube URLs (one per line) or .piml file")
	flag.Parse()

	var queue []Track
	if *filePath != "" {
		err = nil // Use existing err
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

	sr := beep.SampleRate(48000) // Opus is 48kHz
	err = speaker.Init(sr, sr.N(time.Second/5)) // 200ms buffer
	if err != nil {
		log.Fatalf("Error initializing speaker: %v", err)
	}

	fmt.Printf("Initializing streamer...\n")
	streamer := NewOpusStreamer(&client)
	ctrl := &beep.Ctrl{Streamer: streamer, Paused: false}
	volume := &effects.Volume{Streamer: ctrl, Base: 2, Volume: 0, Silent: false}
	speaker.Play(volume)

	fmt.Printf("Starting TUI...\n")
	m := model{
		queue:    queue,
		client:   &client,
		progress: progress.New(progress.WithGradient("#EE9480", "#0B132B")),
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
