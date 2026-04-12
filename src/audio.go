package main

import (
	"io"
	"log"
	"net/http"
	"sync"

	"github.com/at-wat/ebml-go"
	"github.com/dosgo/libopus/opus"
	"github.com/kkdai/youtube/v2"
)

var debugLog *log.Logger

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

type OpusStreamer struct {
	mu           sync.Mutex
	pcmBuf       []int16
	pcmIdx       int
	decoder      *opus.OpusDecoder
	streamClosed bool
	closeChan    chan struct{}
	err          error
	client       *youtube.Client
	streamCount  uint64
}

func NewOpusStreamer(client *youtube.Client) *OpusStreamer {
	dec, _ := opus.NewOpusDecoder(48000, 2)
	return &OpusStreamer{
		decoder:   dec,
		closeChan: make(chan struct{}),
		client:    client,
	}
}

func (os *OpusStreamer) Start(video *youtube.Video, format *youtube.Format) {
	debugLog.Printf("OpusStreamer.Start: %s", video.Title)
	os.mu.Lock()
	if os.closeChan != nil {
		close(os.closeChan)
	}
	os.closeChan = make(chan struct{})
	os.pcmBuf = nil
	os.pcmIdx = 0
	os.streamClosed = false
	os.err = nil
	os.mu.Unlock()

	go os.run(video, format, os.closeChan)
}

func (os *OpusStreamer) run(video *youtube.Video, format *youtube.Format, stop chan struct{}) {
	debugLog.Printf("RUN: Getting stream URL")
	streamURL, err := os.client.GetStreamURL(video, format)
	if err != nil {
		debugLog.Printf("RUN: GetStreamURL error: %v", err)
		return
	}

	debugLog.Printf("RUN: Requesting stream content")
	req, _ := http.NewRequest("GET", streamURL, nil)
	req.Header.Set("User-Agent", "Mozilla/5.0")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		debugLog.Printf("RUN: HTTP Do error: %v", err)
		return
	}
	defer resp.Body.Close()
	debugLog.Printf("RUN: HTTP Status: %s", resp.Status)

	decodedFrames := 0
	hook := func(elem *ebml.Element) {
		select {
		case <-stop:
			return
		default:
		}

		if elem.Name == "SimpleBlock" {
			var block ebml.Block
			switch v := elem.Value.(type) {
			case ebml.Block:
				block = v
			case *ebml.Block:
				block = *v
			default:
				return
			}

			for _, data := range block.Data {
				pcm := make([]int16, 120*48*2)
				n, err := os.decoder.Decode(data, 0, len(data), pcm, 0, len(pcm)/2, false)
				if err != nil {
					continue
				}
				os.mu.Lock()
				os.pcmBuf = append(os.pcmBuf, pcm[:n*2]...)
				
				// Max buffer 20 seconds
				if len(os.pcmBuf) > 48000*2*20 {
					if os.pcmIdx > 48000*2*10 {
						os.pcmBuf = os.pcmBuf[os.pcmIdx:]
						os.pcmIdx = 0
					}
				}
				os.mu.Unlock()
				
				decodedFrames++
				if decodedFrames % 500 == 0 {
					debugLog.Printf("RUN: Decoded %d audio frames", decodedFrames)
				}
			}
		}
	}

	type Cluster struct {
		Timecode    uint64       `ebml:"Timecode"`
		SimpleBlock []ebml.Block `ebml:"SimpleBlock"`
	}
	type Segment struct {
		Cluster []Cluster `ebml:"Cluster"`
	}
	type Container struct {
		Header  interface{} `ebml:"EBML"`
		Segment Segment     `ebml:"Segment"`
	}

	var container Container
	debugLog.Printf("RUN: Starting ebml.Unmarshal")
	err = ebml.Unmarshal(resp.Body, &container, ebml.WithElementReadHooks(hook), ebml.WithIgnoreUnknown(true))
	debugLog.Printf("RUN: ebml.Unmarshal finished: %v (decoded %d frames)", err, decodedFrames)

	os.mu.Lock()
	os.streamClosed = true
	os.mu.Unlock()
}

func (os *OpusStreamer) Stream(samples [][2]float64) (n int, ok bool) {
	os.mu.Lock()
	defer os.mu.Unlock()

	os.streamCount++
	if os.streamCount % 100 == 0 {
		// available := len(os.pcmBuf) - os.pcmIdx
		// debugLog.Printf("STREAM: call %d, pcm available: %d", os.streamCount, available)
	}

	available := len(os.pcmBuf) - os.pcmIdx
	
	if available <= 0 {
		if os.streamClosed {
			return 0, false
		}
		// Return silence while buffering
		for i := range samples {
			samples[i] = [2]float64{0, 0}
		}
		return len(samples), true
	}

	toCopy := len(samples)
	if toCopy > available/2 {
		toCopy = available / 2
	}

	hasAudio := false
	for i := 0; i < toCopy; i++ {
		samples[i][0] = float64(os.pcmBuf[os.pcmIdx]) / 32768.0
		samples[i][1] = float64(os.pcmBuf[os.pcmIdx+1]) / 32768.0
		if samples[i][0] != 0 {
			hasAudio = true
		}
		os.pcmIdx += 2
	}

	// Fill remainder with silence
	for i := toCopy; i < len(samples); i++ {
		samples[i] = [2]float64{0, 0}
	}

	if hasAudio {
		os.streamCount++
		if os.streamCount % 100 == 0 {
			debugLog.Printf("STREAM: delivering audio, toCopy=%d", toCopy)
		}
	}

	return len(samples), true
}

func (os *OpusStreamer) Err() error {
	os.mu.Lock()
	defer os.mu.Unlock()
	return os.err
}

func (os *OpusStreamer) Close() {
	os.mu.Lock()
	if os.closeChan != nil {
		close(os.closeChan)
		os.closeChan = nil
	}
	os.mu.Unlock()
}
