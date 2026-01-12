package main

import "time"

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
