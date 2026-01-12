# YouTube Audio Player (CLI)

A lightweight, terminal-based YouTube audio player written in Go. It streams audio directly from YouTube URLs and supports playback queuing, synced lyrics, and advanced playback controls.

## Prerequisites

- **Go**: Version 1.18 or higher.
- **FFmpeg**: Required for decoding YouTube's audio streams (Opus/AAC) into PCM format for playback. 
  - Ensure `ffmpeg` is in your system's `PATH`.

## Libraries & Dependencies

This project leverages several high-quality Go libraries:

| Library | Purpose |
| :--- | :--- |
| `github.com/kkdai/youtube/v2` | Handles YouTube video metadata fetching and stream URL extraction. |
| `github.com/charmbracelet/bubbletea` | The TUI (Terminal User Interface) framework used for handling the application's lifecycle and events. |
| `github.com/charmbracelet/bubbles` | Provides the progress bar component. |
| `github.com/charmbracelet/lipgloss` | Used for styling and layout of the terminal UI. |
| `github.com/gopxl/beep` | A powerful audio library used for PCM playback and speaker management. |
| `github.com/fezcode/go-piml` | Used for parsing `.piml` playlist files. |

## Building the Project

To build the executable, run the following command in the project root:

```bash
go build -o yt-audio-player main.go
```

## Usage

Provide one or more YouTube URLs as arguments or use a playlist file:

```bash
# Direct URLs
./yt-audio-player <url1> [url2] ...

# From a playlist file
./yt-audio-player --file playlist.txt
./yt-audio-player --file playlist.piml
```

## Playlist Formats

The player supports two types of playlist files via the `--file` flag:

### 1. Plain Text (`.txt`)
A simple list of YouTube URLs, one per line. Lines starting with `#` are ignored.

**Example (`playlist.txt`):**
```text
https://www.youtube.com/watch?v=d8_YZ7QVQlQ
https://www.youtube.com/watch?v=VpKiN_mMB1g
# This is a comment
https://www.youtube.com/watch?v=27lPAUdE1ys
```

### 2. PIML (`.piml`)
A structured format using the PIML language. This allows you to define custom names for tracks which will be displayed in the **Playlist View (`V`)**.

**Example (`playlist.piml`):**
```piml
(videos)
  > (video)
    (name) Tears by Health
    (url) https://www.youtube.com/watch?v=d8_YZ7QVQlQ

  > (video)
    (name) Even Though - Morcheeba
    (url) https://www.youtube.com/watch?v=VpKiN_mMB1g
```

## Controls

- **Space**: Pause / Resume playback.
- **+ / -**: Adjust volume.
- **M**: Mute / Unmute.
- **L**: Toggle Synced Lyrics (A/D to scroll non-synced lyrics).
- **V**: Toggle Playlist view.
- **R**: Randomize (Shuffle) the current list.
- **T**: Toggle Loop-One (Repeat track) mode.
- **N**: Skip to the next track in the queue.
- **P**: Previous track (restarts current track if it has played for > 3 seconds).
- **Left Arrow**: Seek backward 10 seconds.
- **Right Arrow**: Seek forward 10 seconds.
- **Q / Ctrl+C**: Quit the player.

*Note: The player will automatically loop back to the first song after the last track finishes.*
