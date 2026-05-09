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
	// generation increments on every Start. Stale run() goroutines compare
	// against this and bail before mutating pcmBuf — without it, an
	// in-flight ebml.Unmarshal from the *previous* track keeps appending
	// after a seek and corrupts the new playback.
	generation int64
	// startAt + deliveredSamples drives Position(): the wall-clock time the
	// listener is hearing right now. Used by the UI for lyric sync that
	// stays correct across seeks (instead of a tick timer that races
	// ahead while the buffer is still filling).
	startAt          time.Duration
	deliveredSamples int64
	firstAudioLogged bool
	// currentBody is the HTTP body of the in-flight run(). Held so Start()
	// can force-close it, making ebml.Unmarshal in the previous goroutine
	// return immediately — otherwise it would keep draining the throttled
	// stream connection long after we've moved on.
	currentBody io.Closer
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
	if os.currentBody != nil {
		// Force the previous run's ebml.Unmarshal to return now.
		_ = os.currentBody.Close()
		os.currentBody = nil
	}
	os.closeChan = make(chan struct{})
	os.pcmBuf = nil
	os.pcmIdx = 0
	os.streamClosed = false
	os.err = nil
	os.deliveredSamples = 0
	os.firstAudioLogged = false
	if startAt < 0 {
		startAt = 0
	}
	os.startAt = startAt
	// 48kHz × 2 channels = 96000 int16s per second
	os.skipSamples = int(startAt.Seconds()) * 48000 * 2
	os.generation++
	gen := os.generation
	stop := os.closeChan
	os.mu.Unlock()

	go os.run(video, format, stop, gen)
}

// Position returns the wall-clock time the listener is hearing right now,
// measured from the actual number of post-skip samples delivered to the
// speaker. Lyric sync uses this so the highlighted line tracks audio across
// seeks (instead of drifting based on a tick counter while the decoder is
// still skipping ahead).
func (os *OpusStreamer) Position() time.Duration {
	os.mu.Lock()
	defer os.mu.Unlock()
	return os.startAt + time.Duration(os.deliveredSamples)*time.Second/48000
}

// TrySeek attempts to satisfy a relative seek (positive = forward, negative
// = backward) by walking pcmBuf in place — instant. Returns true if the
// target landed inside the buffered window; false otherwise, in which case
// the caller should fall back to a full Start() restart from the new
// absolute time. Avoids the multi-second re-fetch hit that re-decoding
// from byte 0 always incurs.
func (os *OpusStreamer) TrySeek(delta time.Duration) bool {
	os.mu.Lock()
	defer os.mu.Unlock()

	if os.skipSamples > 0 {
		// Currently skipping ahead from a previous restart; fast-seek would
		// land in unrelated samples. Force the caller to restart cleanly.
		return false
	}

	deltaPerCh := int(delta.Seconds() * 48000)            // per-channel samples
	deltaInterleaved := deltaPerCh * 2                    // int16 entries
	newIdx := os.pcmIdx + deltaInterleaved
	if newIdx < 0 || newIdx > len(os.pcmBuf) {
		return false
	}

	os.pcmIdx = newIdx
	os.deliveredSamples += int64(deltaPerCh)
	return true
}

func (os *OpusStreamer) run(video *youtube.Video, format *youtube.Format, stop chan struct{}, gen int64) {
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

	// Publish our body so a subsequent Start() can force-close it and we
	// exit ebml.Unmarshal immediately. Only register if we're still the
	// current generation — otherwise an even newer Start has already
	// fired and we shouldn't overwrite its body.
	os.mu.Lock()
	if os.generation == gen {
		os.currentBody = resp.Body
	}
	os.mu.Unlock()

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
				if os.generation != gen {
					// A newer Start() superseded us; drop the decoded samples
					// instead of polluting the new playback's buffer.
					os.mu.Unlock()
					return
				}
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

				// Retain ~30s of played history (so a backward seek up to
				// 30s lands in-buffer) plus whatever the decoder has run
				// ahead. Trim only when the played portion grows past 30s
				// AND total buffer exceeds 60s.
				const histSamples = 48000 * 2 * 30
				const maxBuf = 48000 * 2 * 60
				if len(os.pcmBuf) > maxBuf && os.pcmIdx > histSamples {
					os.pcmBuf = os.pcmBuf[os.pcmIdx-histSamples:]
					os.pcmIdx = histSamples
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
	if os.generation == gen {
		os.streamClosed = true
		os.currentBody = nil
	}
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
	os.deliveredSamples += int64(toCopy)

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
