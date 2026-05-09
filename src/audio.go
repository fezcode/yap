package main

import (
	"io"
	"log"
	"net/http"
	"sync"
	"time"

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
	// skipSamples counts interleaved int16 samples (so 1 sec stereo @48kHz =
	// 96000) that the decoder should drop before publishing audio. Set on
	// Start() to implement seeking by re-decoding from the beginning and
	// discarding everything before the target time.
	skipSamples int
	firstAudioLogged bool
}

func NewOpusStreamer(client *youtube.Client) *OpusStreamer {
	dec, _ := opus.NewOpusDecoder(48000, 2)
	return &OpusStreamer{
		decoder:   dec,
		closeChan: make(chan struct{}),
		client:    client,
	}
}

func (os *OpusStreamer) Start(video *youtube.Video, format *youtube.Format, startAt time.Duration) {
	debugLog.Printf("OpusStreamer.Start: %s (startAt=%s)", video.Title, startAt)
	os.mu.Lock()
	if os.closeChan != nil {
		close(os.closeChan)
	}
	os.closeChan = make(chan struct{})
	os.pcmBuf = nil
	os.pcmIdx = 0
	os.streamClosed = false
	os.err = nil
	if startAt < 0 {
		startAt = 0
	}
	// 48kHz × 2 channels = 96000 int16s per second
	os.skipSamples = int(startAt.Seconds()) * 48000 * 2
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
				produced := pcm[:n*2]
				if os.skipSamples > 0 {
					if len(produced) <= os.skipSamples {
						os.skipSamples -= len(produced)
						produced = nil
					} else {
						produced = produced[os.skipSamples:]
						os.skipSamples = 0
					}
				}
				if len(produced) > 0 {
					os.pcmBuf = append(os.pcmBuf, produced...)
				}

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
	available := len(os.pcmBuf) - os.pcmIdx
	if (os.streamCount == 1 || os.streamCount%50 == 0) && debugLog != nil {
		debugLog.Printf("STREAM: call#%d len(samples)=%d pcm_available=%d pcmIdx=%d closed=%v",
			os.streamCount, len(samples), available, os.pcmIdx, os.streamClosed)
	}

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
	var maxAbs int16
	for i := 0; i < toCopy; i++ {
		l := os.pcmBuf[os.pcmIdx]
		r := os.pcmBuf[os.pcmIdx+1]
		samples[i][0] = float64(l) / 32768.0
		samples[i][1] = float64(r) / 32768.0
		if l != 0 || r != 0 {
			hasAudio = true
		}
		if l > maxAbs {
			maxAbs = l
		} else if -l > maxAbs {
			maxAbs = -l
		}
		os.pcmIdx += 2
	}

	// Fill remainder with silence
	for i := toCopy; i < len(samples); i++ {
		samples[i] = [2]float64{0, 0}
	}

	if hasAudio && debugLog != nil {
		// Log the first sample we deliver, then every 50th call after.
		if !os.firstAudioLogged {
			os.firstAudioLogged = true
			debugLog.Printf("STREAM: FIRST audio delivery call#%d toCopy=%d peak_int16=%d", os.streamCount, toCopy, maxAbs)
		} else if os.streamCount%50 == 0 {
			debugLog.Printf("STREAM: delivering audio toCopy=%d peak_int16=%d", toCopy, maxAbs)
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
