//go:build !noaudio

package audio

import (
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"log"
	"net"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/audio-router-p2p/pkg/audio/discovery"
	"github.com/audio-router-p2p/pkg/audio/urn"
	"github.com/gordonklaus/portaudio"
)

const blocksPerBuffer = 256

var (
	paMu             sync.Mutex
	paInitCount      int32
	urnCardDevRegex  = regexp.MustCompile(`c=([a-zA-Z0-9_\-]+)(?:.*:d=(\d+))?`)
)

func initPortAudio() error {
	paMu.Lock()
	defer paMu.Unlock()
	return withSuppressedStderrErr(func() error {
		if paInitCount == 0 {
			if err := portaudio.Initialize(); err != nil {
				return err
			}
		}
		paInitCount++
		return nil
	})
}

func termPortAudio() {
	paMu.Lock()
	defer paMu.Unlock()
	if paInitCount > 0 {
		paInitCount--
		if paInitCount == 0 {
			suppressStderr(func() {
				_ = portaudio.Terminate()
			})
		}
	}
}

// portAudioStreamReader abstracts the blocking read and lifecycle from a PortAudio stream.
type portAudioStreamReader interface {
	Read() error
	Stop() error
	Close() error
}

// portAudioStreamWriter abstracts the blocking write and lifecycle from a PortAudio stream.
type portAudioStreamWriter interface {
	Write() error
	Abort() error
	Close() error
}

// portaudioInput wraps a blocking PortAudio input stream and exposes it as a
// byte-oriented AudioStream (little-endian int16 PCM).
type portaudioInput struct {
	mu           sync.Mutex
	stream       portAudioStreamReader
	buf          []int16
	rawBuf       []byte
	spill        []byte
	closed       bool
	lastOverflow atomic.Int64
	// xrunHook is invoked exactly once per input XRun (hardware overflow) at
	// the existing InputOverflowed error site (ADR 0034). It is loaded only
	// in the rare XRun branch, so the happy path pays nothing.
	xrunHook atomic.Pointer[func()]
	isRealPA bool
}

// SetXRunHook installs a hook invoked exactly once per input XRun, from
// within Read() after the unchanged 1/s rate-limited log line (ADR 0034). A
// nil hook clears the installed hook. The hook runs on the audio thread and
// must be lock-free, allocation-free, and non-blocking.
func (p *portaudioInput) SetXRunHook(hook func()) {
	if hook == nil {
		p.xrunHook.Store(nil)
		return
	}
	p.xrunHook.Store(&hook)
}

func buildRateCandidates(requested, defaultRate int, activeRates ...int) []int {
	raw := make([]int, 0, len(activeRates)+5)
	for _, ar := range activeRates {
		if ar > 0 {
			raw = append(raw, ar)
		}
	}
	raw = append(raw, requested, defaultRate, 48000, 44100, 96000)
	candidates := make([]int, 0, len(raw))
	seen := make(map[int]bool, len(raw))
	for _, r := range raw {
		if r > 0 && !seen[r] {
			seen[r] = true
			candidates = append(candidates, r)
		}
	}
	return candidates
}

func extractAlsaCardAndDevice(name string, fallbackNames ...string) (int, int, bool) {
	allNames := append([]string{name}, fallbackNames...)
	for _, candidate := range allNames {
		if candidate == "" {
			continue
		}
		// 1. Check (hw:C,D) or hw:C,D or plughw:C,D
		if matches := hwIndexRegex.FindStringSubmatch(candidate); len(matches) >= 2 {
			c, err := strconv.Atoi(matches[1])
			if err == nil {
				d := 0
				if len(matches) >= 3 && matches[2] != "" {
					if parsedD, err := strconv.Atoi(matches[2]); err == nil {
						d = parsedD
					}
				}
				return c, d, true
			}
		}

		// 2. Check CARD=...,DEV=...
		if matches := alsaCardDevRegex.FindStringSubmatch(candidate); len(matches) >= 2 && matches[1] != "" {
			cStr := matches[1]
			d := 0
			if len(matches) >= 3 && matches[2] != "" {
				if parsedD, err := strconv.Atoi(matches[2]); err == nil {
					d = parsedD
				}
			}
			if c, err := strconv.Atoi(cStr); err == nil {
				return c, d, true
			}
			hwDevs, _ := discovery.DiscoverDevices()
			cLower := strings.ToLower(cStr)
			for _, hd := range hwDevs {
				if strings.ToLower(hd.CardID) == cLower || urn.Slugify(hd.CardName) == cLower {
					return hd.CardIndex, d, true
				}
			}
		}

		// 3. Check c=...,d=... in URN
		if matches := urnCardDevRegex.FindStringSubmatch(candidate); len(matches) >= 2 && matches[1] != "" {
			cStr := matches[1]
			d := 0
			if len(matches) >= 3 && matches[2] != "" {
				if parsedD, err := strconv.Atoi(matches[2]); err == nil {
					d = parsedD
				}
			}
			if c, err := strconv.Atoi(cStr); err == nil {
				return c, d, true
			}
			hwDevs, _ := discovery.DiscoverDevices()
			cLower := strings.ToLower(cStr)
			for _, hd := range hwDevs {
				if strings.ToLower(hd.CardID) == cLower || urn.Slugify(hd.CardName) == cLower {
					return hd.CardIndex, d, true
				}
			}
		}

		// 4. Check loopback keyword
		if strings.Contains(strings.ToLower(candidate), "loopback") {
			hwDevs, _ := discovery.DiscoverDevices()
			for _, hd := range hwDevs {
				if strings.Contains(strings.ToLower(hd.CardID), "loopback") || strings.Contains(strings.ToLower(hd.CardName), "loopback") {
					d := hd.DeviceIndex
					if strings.Contains(candidate, ",1") || strings.Contains(candidate, "DEV=1") || strings.Contains(candidate, ":1") || strings.Contains(candidate, "d=1") {
						d = 1
					} else if strings.Contains(candidate, ",0") || strings.Contains(candidate, "DEV=0") || strings.Contains(candidate, ":0") || strings.Contains(candidate, "d=0") {
						d = 0
					}
					return hd.CardIndex, d, true
				}
			}
		}
	}
	return 0, 0, false
}

func resolveInputDevice(deviceID int, fallbackNames ...string) (*portaudio.DeviceInfo, error) {
	return withSuppressedStderr(func() (*portaudio.DeviceInfo, error) {
		if deviceID < 0 {
			return portaudio.DefaultInputDevice()
		}
		devs, err := portaudio.Devices()
		if err != nil {
			return nil, err
		}
		for _, d := range devs {
			if d.Index == deviceID && d.MaxInputChannels >= 1 {
				return d, nil
			}
		}
		// Device index may be stale or shifted; scan by name or path
		for _, fn := range fallbackNames {
			if fn == "" {
				continue
			}
			fnLower := strings.ToLower(fn)
			for _, d := range devs {
				if d.MaxInputChannels < 1 {
					continue
				}
				dNameLower := strings.ToLower(d.Name)
				if dNameLower == fnLower || strings.Contains(dNameLower, fnLower) || strings.Contains(fnLower, dNameLower) {
					log.Printf("[AUDIO:IO] Re-resolved stale PortAudio input device %d -> %d (%q)", deviceID, d.Index, d.Name)
					return d, nil
				}
			}
		}
		return nil, fmt.Errorf("input device %d not found", deviceID)
	})
}

// OpenInput returns an AudioStream for the given device ID. With useEmulator it
// returns a synthetic sine-wave source; otherwise it opens a real PortAudio
// device. Optional fallback devNames help re-resolve devices if deviceID is stale.
func OpenInput(deviceID int, sampleRate, channels int, useEmulator bool, devNames ...string) (AudioStream, error) {
	if useEmulator {
		return newEmulatorInput(deviceID, sampleRate, channels), nil
	}
	if sampleRate <= 0 {
		return nil, fmt.Errorf("invalid sample rate %d", sampleRate)
	}
	if channels < 1 {
		return nil, fmt.Errorf("invalid channel count %d", channels)
	}
	if err := initPortAudio(); err != nil {
		return nil, err
	}
	dev, err := resolveInputDevice(deviceID, devNames...)
	if err != nil {
		termPortAudio()
		return nil, err
	}
	if channels > dev.MaxInputChannels {
		channels = dev.MaxInputChannels
	}
	latency := dev.DefaultLowInputLatency
	if dev.DefaultHighInputLatency > latency {
		latency = dev.DefaultHighInputLatency
	}
	if latency < 40*time.Millisecond {
		latency = 40 * time.Millisecond
	}
	log.Printf("[AUDIO:IO] Opening PortAudio input: device=%q channels=%d sampleRate=%d latency=%v", dev.Name, channels, sampleRate, latency)

	var activeRate int
	if card, dIdx, ok := extractAlsaCardAndDevice(dev.Name, devNames...); ok {
		if ar, detected := discovery.DetectActiveHardwareRate(card, dIdx, "c"); detected {
			activeRate = ar
			log.Printf("[AUDIO:IO] Detected active hardware rate for card %d dev %d: %d Hz", card, dIdx, activeRate)
		} else if strings.Contains(strings.ToLower(dev.Name), "loopback") {
			// On snd-aloop, Cable 0 capture (dev 1) is locked by Cable 0 playback (dev 0).
			// Inspect the paired subdevice playback side if capture is idle.
			pairedDev := 1 - dIdx
			if pairedDev >= 0 {
				if ar, detected := discovery.DetectActiveHardwareRate(card, pairedDev, "p"); detected {
					activeRate = ar
					log.Printf("[AUDIO:IO] Detected active hardware rate from paired loopback playback card %d dev %d: %d Hz", card, pairedDev, activeRate)
				}
			}
		}
	}

	candidates := buildRateCandidates(sampleRate, int(dev.DefaultSampleRate), activeRate)
	var actualRate int
	var st *portaudio.Stream
	var buf []int16
	var lastErr error

	for _, candidateRate := range candidates {
		candidateBuf := make([]int16, blocksPerBuffer*channels)
		s, err := withSuppressedStderr(func() (*portaudio.Stream, error) {
			ps, oErr := portaudio.OpenStream(portaudio.StreamParameters{
				Input: portaudio.StreamDeviceParameters{
					Device:   dev,
					Channels: channels,
					Latency:  latency,
				},
				SampleRate:      float64(candidateRate),
				FramesPerBuffer: blocksPerBuffer,
			}, &candidateBuf)
			if oErr != nil {
				return nil, oErr
			}
			if sErr := ps.Start(); sErr != nil {
				_ = ps.Close()
				return nil, sErr
			}
			return ps, nil
		})
		if err == nil {
			st = s
			buf = candidateBuf
			actualRate = candidateRate
			break
		}
		lastErr = err
	}

	if st == nil {
		log.Printf("[AUDIO:IO] ERROR opening PortAudio input device %q across candidate rates %v: %v", dev.Name, candidates, lastErr)
		termPortAudio()
		return nil, lastErr
	}

	log.Printf("[AUDIO:IO] PortAudio input online: device=%q channels=%d sampleRate=%d (actual=%d)", dev.Name, channels, sampleRate, actualRate)
	var baseStream AudioStream = &portaudioInput{
		stream:   st,
		buf:      buf,
		rawBuf:   make([]byte, len(buf)*2),
		spill:    make([]byte, 0, len(buf)*2),
		isRealPA: true,
	}

	if actualRate != sampleRate {
		log.Printf("[AUDIO:IO] Wrapping input %q with ASRC resampler (%d -> %d Hz)", dev.Name, actualRate, sampleRate)
		resampled, err := NewResampleInputStream(baseStream, actualRate, sampleRate, channels)
		if err != nil {
			_ = baseStream.Close()
			return nil, err
		}
		return resampled, nil
	}
	return baseStream, nil
}

// NewPortaudioInputForTest instantiates a portaudioInput with a custom stream reader for unit testing.
func NewPortaudioInputForTest(stream portAudioStreamReader, buf []int16) AudioStream {
	return &portaudioInput{
		stream:   stream,
		buf:      buf,
		rawBuf:   make([]byte, len(buf)*2),
		spill:    make([]byte, 0, len(buf)*2),
		isRealPA: false,
	}
}

func (p *portaudioInput) Read(dst []byte) (int, error) {
	p.mu.Lock()
	defer p.mu.Unlock()

	// 1. Drain any remaining spillover bytes from a previous read first
	if len(p.spill) > 0 {
		n := copy(dst, p.spill)
		p.spill = p.spill[n:]
		return n, nil
	}

	if p.closed {
		return 0, io.EOF
	}

	// 2. Read next buffer from hardware / stream reader
	if err := p.stream.Read(); err != nil {
		if errors.Is(err, portaudio.InputOverflowed) {
			now := time.Now().UnixNano()
			last := p.lastOverflow.Load()
			if now-last > int64(time.Second) && p.lastOverflow.CompareAndSwap(last, now) {
				log.Printf("[AUDIO:IO] Input buffer overflowed (XRun)")
			}
			// Fire the device-lifetime XRun hook exactly once per XRun
			// (ADR 0034): one atomic pointer load + one call, no extra
			// locking or allocation on the audio thread.
			if h := p.xrunHook.Load(); h != nil {
				(*h)()
			}
		} else {
			log.Printf("[AUDIO:IO] Input stream read error: %v", err)
			return 0, err
		}
	}

	// 3. Serialize int16 buffer to little-endian bytes without extra allocations
	need := len(p.buf) * 2
	if len(p.rawBuf) < need {
		p.rawBuf = make([]byte, need)
	}
	for i, s := range p.buf {
		binary.LittleEndian.PutUint16(p.rawBuf[i*2:], uint16(s))
	}

	// 4. Fill dst with as many bytes as possible and preserve the remainder in spill
	n := copy(dst, p.rawBuf[:need])
	if n < need {
		p.spill = append(p.spill[:0], p.rawBuf[n:need]...)
	}
	return n, nil
}

func (p *portaudioInput) Write(b []byte) (int, error) { return 0, nil }

func (p *portaudioInput) Close() error {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.closed {
		return nil
	}
	p.closed = true
	var err error
	if p.stream != nil {
		_ = p.stream.Stop()
		suppressStderr(func() {
			err = p.stream.Close()
		})
	}
	if p.isRealPA {
		termPortAudio()
	}
	return err
}

// portaudioOutput wraps a blocking PortAudio output stream exposed as a
// byte-oriented AudioStream. Complete buffer-sized writes are played back in
// real time; smaller writes are parked until a full buffer accumulates.
type portaudioOutput struct {
	mu            sync.Mutex
	stream        portAudioStreamWriter
	buf           []int16
	frameSize     int
	blockBytes    int
	spill         []byte
	closed        bool
	lastUnderflow atomic.Int64
	// xrunHook is invoked exactly once per output XRun (hardware underflow)
	// at the existing OutputUnderflowed error site (ADR 0034). It is loaded
	// only in the rare XRun branch, so the happy path pays nothing.
	xrunHook atomic.Pointer[func()]
}

// SetXRunHook installs a hook invoked exactly once per output XRun, from
// within Write() after the unchanged 1/s rate-limited log line (ADR 0034). A
// nil hook clears the installed hook. The hook runs on the audio thread and
// must be lock-free, allocation-free, and non-blocking.
func (o *portaudioOutput) SetXRunHook(hook func()) {
	if hook == nil {
		o.xrunHook.Store(nil)
		return
	}
	o.xrunHook.Store(&hook)
}

// NewPortaudioOutputForTest instantiates a portaudioOutput with a custom
// stream writer for unit testing. The test stream plays mono (1 channel)
// int16 frames; the physical block size is len(buf) samples.
func NewPortaudioOutputForTest(stream portAudioStreamWriter, buf []int16) AudioStream {
	return &portaudioOutput{
		stream:     stream,
		buf:        buf,
		frameSize:  2,
		blockBytes: len(buf) * 2,
	}
}

func resolveOutputDevice(deviceID int, fallbackNames ...string) (*portaudio.DeviceInfo, error) {
	return withSuppressedStderr(func() (*portaudio.DeviceInfo, error) {
		if deviceID < 0 {
			return portaudio.DefaultOutputDevice()
		}
		devs, err := portaudio.Devices()
		if err != nil {
			return nil, err
		}
		for _, d := range devs {
			if d.Index == deviceID && d.MaxOutputChannels >= 1 {
				return d, nil
			}
		}
		for _, fn := range fallbackNames {
			if fn == "" {
				continue
			}
			fnLower := strings.ToLower(fn)
			for _, d := range devs {
				if d.MaxOutputChannels < 1 {
					continue
				}
				dNameLower := strings.ToLower(d.Name)
				if dNameLower == fnLower || strings.Contains(dNameLower, fnLower) || strings.Contains(fnLower, dNameLower) {
					log.Printf("[AUDIO:IO] Re-resolved stale PortAudio output device %d -> %d (%q)", deviceID, d.Index, d.Name)
					return d, nil
				}
			}
		}
		return nil, fmt.Errorf("output device %d not found", deviceID)
	})
}

// OpenOutput returns an AudioStream for the given device ID. With useEmulator
// it returns an in-memory buffer; otherwise it opens a real PortAudio device.
// Optional fallback devNames help re-resolve devices if deviceID is stale.
func OpenOutput(deviceID int, sampleRate, channels int, useEmulator bool, devNames ...string) (AudioStream, error) {
	if useEmulator {
		return newEmulatorOutput(deviceID), nil
	}
	if sampleRate <= 0 || channels < 1 {
		return nil, fmt.Errorf("invalid sample rate %d / channels %d", sampleRate, channels)
	}
	if err := initPortAudio(); err != nil {
		return nil, err
	}
	dev, err := resolveOutputDevice(deviceID, devNames...)
	if err != nil {
		termPortAudio()
		return nil, err
	}
	if channels > dev.MaxOutputChannels {
		channels = dev.MaxOutputChannels
	}
	latency := dev.DefaultLowOutputLatency
	if dev.DefaultHighOutputLatency > latency {
		latency = dev.DefaultHighOutputLatency
	}
	if latency < 40*time.Millisecond {
		latency = 40 * time.Millisecond
	}
	log.Printf("[AUDIO:IO] Opening PortAudio output: device=%q channels=%d sampleRate=%d latency=%v", dev.Name, channels, sampleRate, latency)

	var activeRate int
	if card, dIdx, ok := extractAlsaCardAndDevice(dev.Name, devNames...); ok {
		if ar, detected := discovery.DetectActiveHardwareRate(card, dIdx, "p"); detected {
			activeRate = ar
			log.Printf("[AUDIO:IO] Detected active hardware rate for card %d dev %d: %d Hz", card, dIdx, activeRate)
		} else if strings.Contains(strings.ToLower(dev.Name), "loopback") {
			// On snd-aloop, Cable 0 playback (dev 0) is locked by Cable 0 capture (dev 1).
			// Inspect the paired subdevice capture side if playback is idle.
			pairedDev := 1 - dIdx
			if pairedDev >= 0 {
				if ar, detected := discovery.DetectActiveHardwareRate(card, pairedDev, "c"); detected {
					activeRate = ar
					log.Printf("[AUDIO:IO] Detected active hardware rate from paired loopback capture card %d dev %d: %d Hz", card, pairedDev, activeRate)
				}
			}
		}
	}

	candidates := buildRateCandidates(sampleRate, int(dev.DefaultSampleRate), activeRate)
	var actualRate int
	var st *portaudio.Stream
	var buf []int16
	var lastErr error

	for _, candidateRate := range candidates {
		candidateBuf := make([]int16, blocksPerBuffer*channels)
		s, err := withSuppressedStderr(func() (*portaudio.Stream, error) {
			ps, oErr := portaudio.OpenStream(portaudio.StreamParameters{
				Output: portaudio.StreamDeviceParameters{
					Device:   dev,
					Channels: channels,
					Latency:  latency,
				},
				SampleRate:      float64(candidateRate),
				FramesPerBuffer: blocksPerBuffer,
			}, &candidateBuf)
			if oErr != nil {
				return nil, oErr
			}
			if sErr := ps.Start(); sErr != nil {
				_ = ps.Close()
				return nil, sErr
			}
			return ps, nil
		})
		if err == nil {
			st = s
			buf = candidateBuf
			actualRate = candidateRate
			break
		}
		lastErr = err
	}

	if st == nil {
		log.Printf("[AUDIO:IO] ERROR opening PortAudio output device %q across candidate rates %v: %v", dev.Name, candidates, lastErr)
		termPortAudio()
		return nil, lastErr
	}

	log.Printf("[AUDIO:IO] PortAudio output online: device=%q channels=%d sampleRate=%d (actual=%d)", dev.Name, channels, sampleRate, actualRate)
	var baseStream AudioStream = &portaudioOutput{
		stream:     st,
		buf:        buf,
		frameSize:  channels * 2,
		blockBytes: len(buf) * 2,
	}

	if actualRate != sampleRate {
		log.Printf("[AUDIO:IO] Wrapping output %q with ASRC resampler (%d -> %d Hz)", dev.Name, sampleRate, actualRate)
		resampled, err := NewResampleOutputStream(baseStream, sampleRate, actualRate, channels)
		if err != nil {
			_ = baseStream.Close()
			return nil, err
		}
		return resampled, nil
	}
	return baseStream, nil
}

func (o *portaudioOutput) Write(p []byte) (int, error) {
	o.mu.Lock()
	defer o.mu.Unlock()
	if o.closed {
		return 0, net.ErrClosed
	}
	data := append(o.spill, p...)
	end := len(data) - len(data)%o.frameSize
	if end < o.blockBytes {
		o.spill = data[:end:end]
		return len(p), nil
	}
	off := 0
	for off+o.blockBytes <= end {
		base := off
		for i := range o.buf {
			o.buf[i] = int16(binary.LittleEndian.Uint16(data[base+i*2:]))
		}
		if err := o.stream.Write(); err != nil {
			if errors.Is(err, portaudio.OutputUnderflowed) {
				now := time.Now().UnixNano()
				last := o.lastUnderflow.Load()
				if now-last > int64(time.Second) && o.lastUnderflow.CompareAndSwap(last, now) {
					log.Printf("[AUDIO:IO] Output buffer underflowed (XRun)")
				}
				// Fire the device-lifetime XRun hook exactly once per XRun
				// (ADR 0034): one atomic pointer load + one call, no extra
				// locking or allocation on the audio thread.
				if h := o.xrunHook.Load(); h != nil {
					(*h)()
				}
			} else {
				log.Printf("[AUDIO:IO] Output stream write error: %v", err)
				o.spill = append(o.spill[:0], data[off:end]...)
				return off, err
			}
		}
		off += o.blockBytes
	}
	o.spill = append(o.spill[:0], data[off:end]...)
	return len(p), nil
}

func (o *portaudioOutput) Read(b []byte) (int, error) { return 0, nil }

func (o *portaudioOutput) Close() error {
	o.mu.Lock()
	defer o.mu.Unlock()
	if o.closed {
		return nil
	}
	o.closed = true
	_ = o.stream.Abort()
	var err error
	suppressStderr(func() {
		err = o.stream.Close()
	})
	termPortAudio()
	return err
}

func isPortAudioStreamDirection(s AudioStream) (isInput, isOutput bool) {
	switch s.(type) {
	case *portaudioInput:
		return true, false
	case *portaudioOutput:
		return false, true
	}
	return false, false
}

