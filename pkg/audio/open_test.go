//go:build !noaudio

package audio

import (
	"encoding/binary"
	"errors"
	"io"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/gordonklaus/portaudio"
)

type mockPAStream struct {
	buf       []int16
	sampleSeq int16
	channels  int
}

func (m *mockPAStream) Read() error {
	frames := len(m.buf) / m.channels
	for i := 0; i < frames; i++ {
		for c := 0; c < m.channels; c++ {
			m.buf[i*m.channels+c] = m.sampleSeq
		}
		m.sampleSeq++
	}
	return nil
}

func (m *mockPAStream) Stop() error  { return nil }
func (m *mockPAStream) Close() error { return nil }

func TestPortAudioInput_SubBufferRemainderPreserved(t *testing.T) {
	const (
		channels        = 2
		blocksPerBuf    = 256
		packetFrames    = 960 // 20ms @ 48kHz
		numReads        = 20  // total 19200 frames
		bytesPerFrame   = channels * 2
		packetByteCount = packetFrames * bytesPerFrame // 3840 bytes
	)

	buf := make([]int16, blocksPerBuf*channels)
	mock := &mockPAStream{
		buf:      buf,
		channels: channels,
	}

	in := NewPortaudioInputForTest(mock, buf)
	defer in.Close()

	packet := make([]byte, packetByteCount)
	var allFrames []int16

	for r := 0; r < numReads; r++ {
		offset := 0
		for offset < packetByteCount {
			n, err := in.Read(packet[offset:])
			if err != nil {
				t.Fatalf("read %d: %v", r, err)
			}
			if n == 0 {
				t.Fatalf("read %d returned 0 bytes", r)
			}
			offset += n
		}

		for i := 0; i < packetFrames; i++ {
			sample := int16(binary.LittleEndian.Uint16(packet[i*bytesPerFrame:]))
			allFrames = append(allFrames, sample)
		}
	}

	if len(allFrames) != numReads*packetFrames {
		t.Fatalf("expected %d frames, got %d", numReads*packetFrames, len(allFrames))
	}

	// Verify sample sequence continuity without any drops or phase jumps
	droppedFrames := 0
	for i := 1; i < len(allFrames); i++ {
		expected := allFrames[i-1] + 1
		actual := allFrames[i]
		if actual != expected {
			droppedFrames++
			if droppedFrames <= 10 {
				t.Errorf("frame %d: expected %d, got %d (gap=%d)", i, expected, actual, actual-expected)
			}
		}
	}

	if droppedFrames > 0 {
		t.Fatalf("total dropped/discontinuous frames: %d (audio would be robotic/distorted)", droppedFrames)
	}
}

func TestPortAudioInput_SpillDrainBeforeEOF(t *testing.T) {
	buf := make([]int16, 256*2)
	mock := &mockPAStream{
		buf:      buf,
		channels: 2,
	}

	in := NewPortaudioInputForTest(mock, buf)

	// Read 500 bytes (less than 1024 bytes need)
	part := make([]byte, 500)
	n, err := in.Read(part)
	if err != nil || n != 500 {
		t.Fatalf("expected 500 bytes, got %d (err: %v)", n, err)
	}

	// Now close the stream
	_ = in.Close()

	// Subsequent read should drain remaining 524 bytes from spill before returning EOF
	rem := make([]byte, 1024)
	n, err = in.Read(rem)
	if err != nil {
		t.Fatalf("expected spill drain, got err: %v", err)
	}
	if n != 524 {
		t.Fatalf("expected 524 spill bytes, got %d", n)
	}

	// Next read should return EOF
	n, err = in.Read(rem)
	if err != io.EOF {
		t.Fatalf("expected EOF after spill drained, got n=%d err=%v", n, err)
	}
}

func TestBuildRateCandidates_Prioritization(t *testing.T) {
	// 1. Without activeRates: requested rate should come first
	rates := buildRateCandidates(48000, 44100)
	if len(rates) == 0 || rates[0] != 48000 {
		t.Fatalf("expected 48000 at index 0, got %v", rates)
	}

	// 2. With activeRates (e.g. procfs detected 44100 locked hardware rate):
	// Active rate MUST be index 0
	rates = buildRateCandidates(48000, 48000, 44100)
	if len(rates) == 0 || rates[0] != 44100 {
		t.Fatalf("expected active rate 44100 at index 0, got %v", rates)
	}

	// Verify deduplication
	seen := make(map[int]int)
	for _, r := range rates {
		seen[r]++
		if seen[r] > 1 {
			t.Errorf("duplicate rate %d in %v", r, rates)
		}
	}
}

func TestExtractAlsaCardAndDevice(t *testing.T) {
	tests := []struct {
		name     string
		fallback []string
		wantC    int
		wantD    int
		wantOk   bool
	}{
		{
			name:   "hw:Loopback,1",
			wantC:  0,
			wantD:  0,
			wantOk: false, // "Loopback" is not integer card number in hw:
		},
		{
			name:   "hw:0,1",
			wantC:  0,
			wantD:  1,
			wantOk: true,
		},
		{
			name:   "plughw:2,0",
			wantC:  2,
			wantD:  0,
			wantOk: true,
		},
		{
			name:   "ALSA: HDA Intel PCH (hw:0,0)",
			wantC:  0,
			wantD:  0,
			wantOk: true,
		},
		{
			name:     "Invalid name with URN fallback",
			fallback: []string{"urn:audio:hw:alsa:c=1:d=2"},
			wantC:    1,
			wantD:    2,
			wantOk:   true,
		},
		{
			name:     "Invalid name with card-only fallback",
			fallback: []string{"hw:3"},
			wantC:    3,
			wantD:    0,
			wantOk:   true,
		},
	}

	for _, tc := range tests {
		c, d, ok := extractAlsaCardAndDevice(tc.name, tc.fallback...)
		if ok != tc.wantOk {
			t.Errorf("[%s] expected ok=%t, got %t", tc.name, tc.wantOk, ok)
			continue
		}
		if ok {
			if c != tc.wantC || d != tc.wantD {
				t.Errorf("[%s] expected card=%d dev=%d, got card=%d dev=%d", tc.name, tc.wantC, tc.wantD, c, d)
			}
		}
	}
}

// fakePAReader is a portAudioStreamReader that replays a fixed error sequence:
// call i returns errs[i] when i < len(errs), and nil afterwards.
type fakePAReader struct {
	mu    sync.Mutex
	buf   []int16
	errs  []error
	calls int
}

func (f *fakePAReader) Read() error {
	f.mu.Lock()
	defer f.mu.Unlock()
	var err error
	if f.calls < len(f.errs) {
		err = f.errs[f.calls]
	}
	// Keep the stale buffer populated so the swallow-stale-buffer path
	// after an XRun is exercised.
	for i := range f.buf {
		f.buf[i] = int16(i)
	}
	f.calls++
	return err
}

func (f *fakePAReader) Stop() error  { return nil }
func (f *fakePAReader) Close() error { return nil }

// fakePAWriter is a portAudioStreamWriter that replays a fixed error sequence:
// call i returns errs[i] when i < len(errs), and nil afterwards.
type fakePAWriter struct {
	mu    sync.Mutex
	errs  []error
	calls int
}

func (f *fakePAWriter) Write() error {
	f.mu.Lock()
	defer f.mu.Unlock()
	var err error
	if f.calls < len(f.errs) {
		err = f.errs[f.calls]
	}
	f.calls++
	return err
}

func (f *fakePAWriter) callCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.calls
}

func (f *fakePAWriter) Abort() error { return nil }
func (f *fakePAWriter) Close() error { return nil }

func setTestXRunHook(t *testing.T, s AudioStream, fired *atomic.Int64) {
	t.Helper()
	setter, ok := s.(XRunHookSetter)
	if !ok {
		t.Fatalf("%T does not implement XRunHookSetter", s)
	}
	setter.SetXRunHook(func() { fired.Add(1) })
}

// TestPortAudioInput_XRunHookExactness verifies the hook fires EXACTLY once
// per InputOverflowed: k interleaved XRuns → k invocations, zero for
// non-XRun errors, and no panic when no hook is installed.
func TestPortAudioInput_XRunHookExactness(t *testing.T) {
	const (
		channels = 2
		blocks   = 256
		xruns    = 3
	)
	buf := make([]int16, blocks*channels)
	fake := &fakePAReader{buf: buf}
	// Interleave: normal, XRun, normal, XRun, normal, XRun, normal
	for i := 0; i < xruns+1; i++ {
		fake.errs = append(fake.errs, nil)
		if i < xruns {
			fake.errs = append(fake.errs, portaudio.InputOverflowed)
		}
	}

	in := NewPortaudioInputForTest(fake, buf)
	defer in.Close()

	var fired atomic.Int64
	setTestXRunHook(t, in, &fired)

	dst := make([]byte, blocks*channels*2)
	for i, expected := range fake.errs {
		n, err := in.Read(dst)
		if errors.Is(expected, portaudio.InputOverflowed) {
			// The XRun must be swallowed: stale buffer, nil error.
			if err != nil || n == 0 {
				t.Fatalf("read %d: XRun must be swallowed, got n=%d err=%v", i, n, err)
			}
			if got := fired.Load(); got != int64(i/2+1) {
				t.Fatalf("read %d: hook fired %d times so far, want %d", i, got, i/2+1)
			}
		} else if err != nil || n == 0 {
			t.Fatalf("read %d: unexpected failure n=%d err=%v", i, n, err)
		}
	}
	if got := fired.Load(); got != xruns {
		t.Fatalf("hook fired %d times, want exactly %d", got, xruns)
	}
}

func TestPortAudioInput_NonXRunErrorDoesNotFireHook(t *testing.T) {
	buf := make([]int16, 256*2)
	boom := errors.New("device fault")
	fake := &fakePAReader{buf: buf, errs: []error{nil, boom, nil}}

	in := NewPortaudioInputForTest(fake, buf)
	defer in.Close()

	var fired atomic.Int64
	setTestXRunHook(t, in, &fired)

	dst := make([]byte, 1024)
	if _, err := in.Read(dst); err != nil {
		t.Fatalf("read 0: %v", err)
	}
	// Non-XRun errors still propagate to the caller (unchanged behavior).
	if _, err := in.Read(dst); !errors.Is(err, boom) {
		t.Fatalf("read 1: expected the original error to propagate, got %v", err)
	}
	if got := fired.Load(); got != 0 {
		t.Fatalf("hook fired %d times on non-XRun errors, want 0", got)
	}
	if _, err := in.Read(dst); err != nil {
		t.Fatalf("read 2: %v", err)
	}
	if got := fired.Load(); got != 0 {
		t.Fatalf("hook fired %d times overall, want 0", got)
	}
}

func TestPortAudioInput_XRunWithNoHookDoesNotPanic(t *testing.T) {
	buf := make([]int16, 256*2)
	fake := &fakePAReader{buf: buf, errs: []error{portaudio.InputOverflowed, nil}}

	in := NewPortaudioInputForTest(fake, buf)
	defer in.Close()
	// No hook installed: XRuns must still be logged (rate-limited) and
	// swallowed without panicking.
	dst := make([]byte, 1024)
	n, err := in.Read(dst)
	if err != nil || n == 0 {
		t.Fatalf("read with XRun and no hook: n=%d err=%v", n, err)
	}
	if _, err := in.Read(dst); err != nil {
		t.Fatalf("read after XRun: %v", err)
	}
}

// TestPortAudioOutput_XRunHookExactness verifies the hook fires EXACTLY once
// per OutputUnderflowed: k interleaved XRuns → k invocations, zero for
// non-underflow errors (which still propagate), and no panic when no hook is
// installed.
func TestPortAudioOutput_XRunHookExactness(t *testing.T) {
	const xruns = 3
	buf := make([]int16, 256) // mono test stream
	fake := &fakePAWriter{}
	// Each physical Write() plays one full block:
	// ok, XRun, ok, XRun, ok, XRun, ok
	for i := 0; i < xruns; i++ {
		fake.errs = append(fake.errs, nil, portaudio.OutputUnderflowed)
	}
	fake.errs = append(fake.errs, nil)

	out := NewPortaudioOutputForTest(fake, buf)
	defer out.Close()

	var fired atomic.Int64
	setTestXRunHook(t, out, &fired)

	blockBytes := len(buf) * 2
	payload := make([]byte, blockBytes*len(fake.errs))
	n, err := out.Write(payload)
	if err != nil {
		t.Fatalf("Write: XRuns must be swallowed, got %v", err)
	}
	if n != len(payload) {
		t.Fatalf("Write returned n=%d, want %d", n, len(payload))
	}
	if got := fired.Load(); got != xruns {
		t.Fatalf("hook fired %d times, want exactly %d", got, xruns)
	}
	if got := fake.callCount(); got != len(fake.errs) {
		t.Fatalf("underlying Write called %d times, want %d", got, len(fake.errs))
	}
}

func TestPortAudioOutput_NonXRunErrorDoesNotFireHook(t *testing.T) {
	buf := make([]int16, 256)
	boom := errors.New("device fault")
	fake := &fakePAWriter{errs: []error{boom}}

	out := NewPortaudioOutputForTest(fake, buf)
	defer out.Close()

	var fired atomic.Int64
	setTestXRunHook(t, out, &fired)

	payload := make([]byte, len(buf)*2)
	n, err := out.Write(payload)
	if err == nil {
		t.Fatalf("expected non-XRun error to propagate, got n=%d", n)
	}
	if got := fired.Load(); got != 0 {
		t.Fatalf("hook fired %d times on non-XRun error, want 0", got)
	}
}

func TestPortAudioOutput_XRunWithNoHookDoesNotPanic(t *testing.T) {
	buf := make([]int16, 256)
	fake := &fakePAWriter{errs: []error{portaudio.OutputUnderflowed, nil}}

	out := NewPortaudioOutputForTest(fake, buf)
	defer out.Close()
	// No hook installed: the underflow must still be logged (rate-limited)
	// and swallowed without panicking.
	payload := make([]byte, len(buf)*4)
	n, err := out.Write(payload)
	if err != nil || n != len(payload) {
		t.Fatalf("Write with XRun and no hook: n=%d err=%v", n, err)
	}
}
