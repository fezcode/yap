package main

import (
	"bufio"
	"os"
	"path/filepath"
	"strings"

	"github.com/fezcode/go-piml"
)

func LoadPlaylist(path string) ([]Track, error) {
	var queue []Track
	ext := strings.ToLower(filepath.Ext(path))

	if ext == ".piml" {
		data, err := os.ReadFile(path)
		if err != nil {
			return nil, err
		}
		var pimlData PimlPlaylist
		if err := piml.Unmarshal(data, &pimlData); err != nil {
			return nil, err
		}
		queue = pimlData.Videos
	} else {
		file, err := os.Open(path)
		if err != nil {
			return nil, err
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
	return queue, nil
}
