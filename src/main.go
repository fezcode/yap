package main

import (
	"flag"
	"fmt"
	"log"
	"math/rand"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/charmbracelet/bubbles/progress"
	tea "github.com/charmbracelet/bubbletea"
	"github.com/gopxl/beep"
	"github.com/gopxl/beep/effects"
	"github.com/gopxl/beep/speaker"
	"github.com/kkdai/youtube/v2"
)

var Version = "dev" // Overwritten by gobake build

func printHelp() {
	fmt.Println("atlas.yap - YouTube Audio Player TUI")
	fmt.Println("Usage:")
	fmt.Println("  atlas.yap [--file <path>] <youtube-url1> [youtube-url2] ...")
	fmt.Println("  atlas.yap --search \"<query>\"")
	fmt.Println("  atlas.yap <free text query, no URL>")
	fmt.Println("Options:")
	fmt.Println("  --file string     Path to a playlist file (.txt or .piml)")
	fmt.Println("  -s, --search      Treat the rest of the args as a YouTube search query")
	fmt.Println("  -n int            Number of search results to enqueue (default 1)")
	fmt.Println("  -v, --version     Show version")
	fmt.Println("  -h, --help        Show help")
}

func main() {
	if len(os.Args) > 1 && (os.Args[1] == "-v" || os.Args[1] == "--version") {
		fmt.Printf("atlas.yap v%s\n", Version)
		return
	}
	if len(os.Args) > 1 && (os.Args[1] == "-h" || os.Args[1] == "--help") {
		printHelp()
		return
	}

	fmt.Println("Main starting...")
	home, _ := os.UserHomeDir()
	logPath := filepath.Join(home, fmt.Sprintf("atlas.yap_debug_%d.log", time.Now().Unix()))
	f, err := os.OpenFile(logPath, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0666)
	if err == nil {
		debugLog = log.New(f, "DEBUG: ", log.LstdFlags)
		debugLog.Printf("Application started - logging to %s\n", logPath)
		fmt.Printf("Logging to: %s\n", logPath)
	} else {
		fmt.Printf("Failed to open log file: %v\n", err)
	}

	rand.Seed(time.Now().UnixNano())

	fs := flag.NewFlagSet("atlas.yap", flag.ExitOnError)
	filePath := fs.String("file", "", "Path to a file containing YouTube URLs (one per line) or .piml file")
	searchFlag := fs.Bool("search", false, "Treat positional args as a YouTube search query")
	fs.BoolVar(searchFlag, "s", false, "Alias for --search")
	searchN := fs.Int("n", 1, "Number of search results to enqueue (used with --search)")
	_ = fs.Parse(os.Args[1:])

	var queue []Track
	if *filePath != "" {
		queue, err = LoadPlaylist(*filePath)
		if err != nil {
			log.Fatalf("Error loading playlist: %v", err)
		}
	}

	args := fs.Args()

	if *searchFlag {
		query := strings.TrimSpace(strings.Join(args, " "))
		if query == "" {
			fmt.Println("--search requires a query")
			return
		}
		fmt.Printf("Searching YouTube for: %s\n", query)
		results, err := SearchYouTube(query, *searchN)
		if err != nil {
			log.Fatalf("Search failed: %v", err)
		}
		if len(results) == 0 {
			log.Fatalf("No results found for: %s", query)
		}
		for _, r := range results {
			fmt.Printf("  -> %s (%s)\n", r.Title, r.URL)
			queue = append(queue, Track{URL: r.URL, Name: r.Title})
		}
	} else {
		// If no positional URL is provided, but free-text args exist, treat the
		// whole tail as a search query (one result).
		if !hasURL(args) && len(args) > 0 {
			query := strings.TrimSpace(strings.Join(args, " "))
			fmt.Printf("Searching YouTube for: %s\n", query)
			results, err := SearchYouTube(query, *searchN)
			if err != nil {
				log.Fatalf("Search failed: %v", err)
			}
			if len(results) == 0 {
				log.Fatalf("No results found for: %s", query)
			}
			for _, r := range results {
				fmt.Printf("  -> %s (%s)\n", r.Title, r.URL)
				queue = append(queue, Track{URL: r.URL, Name: r.Title})
			}
		} else {
			for _, arg := range args {
				queue = append(queue, Track{URL: arg})
			}
		}
	}

	if len(queue) == 0 {
		printHelp()
		return
	}

	client := youtube.Client{}

	sr := beep.SampleRate(48000)                 // Opus is 48kHz
	err = speaker.Init(sr, sr.N(time.Second/5)) // 200ms buffer
	if err != nil {
		log.Fatalf("Error initializing speaker: %v", err)
	}
	if debugLog != nil {
		debugLog.Printf("MAIN: speaker.Init OK sr=%d bufferSamples=%d", int(sr), sr.N(time.Second/5))
	}

	fmt.Printf("Initializing streamer...\n")
	streamer := NewOpusStreamer(&client)
	ctrl := &beep.Ctrl{Streamer: streamer, Paused: false}
	volume := &effects.Volume{Streamer: ctrl, Base: 2, Volume: 0, Silent: false}
	speaker.Play(volume)
	if debugLog != nil {
		debugLog.Printf("MAIN: speaker.Play(volume) called")
	}

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

func hasURL(args []string) bool {
	for _, a := range args {
		if strings.HasPrefix(a, "http://") || strings.HasPrefix(a, "https://") ||
			strings.Contains(a, "youtube.com/") || strings.Contains(a, "youtu.be/") {
			return true
		}
	}
	return false
}
