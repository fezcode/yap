package main

import (
	"io"
	"sync"
)

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
