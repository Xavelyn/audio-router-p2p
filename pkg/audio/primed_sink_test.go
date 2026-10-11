package audio

import (
	"bytes"
	"encoding/binary"
	"errors"
	"io"
	"net"
	"runtime"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

type mockPortAudioWriter struct {
	mu         sync.Mutex
	started    atomic.Bool
	stopped    atomic.Bool
	aborted    atomic.Bool
	closed     atomic.Bool
	writeCount atomic.Uint64
	writeErr   error
	writeDelay time.Duration
	writeHook  func()
}

func (m *mockPortAudioWriter) Start() error {
	m.started.Store(true)
	return nil
}

func (m *mockPortAudioWriter) Write() error {
	m.writeCount.Add(1)
	if m.writeHook != nil {
		m.writeHook()
	}
	if m.writeDelay > 0 {
		time.Sleep(m.writeDelay)
	}
	return m.writeErr
}

func (m *mockPortAudioWriter) Stop() error {
	m.stopped.Store(true)
	return nil
}

func (m *mockPortAudioWriter) Abort() error {
	m.aborted.Store(true)
	return nil
}

func (m *mockPortAudioWriter) Close() error {
	m.closed.Store(true)
	return nil
}

func TestPrimedBufferedSink_LazyStartWatermark(t *testing.T) {
	mockWriter := &mockPortAudioWriter{
		writeDelay: time.Millisecond,
	}

	sampleRate := 48000
	channels := 2
	frameSize := channels * 2
	watermarkDuration := 60 * time.Millisecond
	framesPerBuffer := 256

	sink := NewPrimedBufferedSink(
		mockWriter,
		nil,
		sampleRate,
		channels,
		WithPreRollWatermark(watermarkDuration),
		WithFramesPerBuffer(framesPerBuffer),
	)
	defer sink.Close()

	if sink.State() != StateUnprimed {
		t.Fatalf("expected initial state %v, got %v", StateUnprimed, sink.State())
	}
	if mockWriter.started.Load() {
		t.Fatal("expected PortAudio stream NOT to be started initially")
	}

	// 1. Write audio below watermark: 20ms of audio (960 frames = 3840 bytes)
	// Watermark is 60ms = 2880 frames = 11520 bytes
	pcm20ms := make([]byte, 960*frameSize)
	n, err := sink.Write(pcm20ms)
	if err != nil {
		t.Fatalf("Write error: %v", err)
	}
	if n != len(pcm20ms) {
		t.Fatalf("wrote %d bytes, expected %d", n, len(pcm20ms))
	}

	// Give pump worker time to react
	time.Sleep(20 * time.Millisecond)

	if mockWriter.started.Load() {
		t.Fatal("PortAudio stream started prematurely before reaching pre-roll watermark")
	}
	if sink.State() != StateUnprimed {
		t.Fatalf("expected state %v before watermark, got %v", StateUnprimed, sink.State())
	}

	// 2. Write second 20ms packet (cumulative 40ms, still below 60ms)
	_, _ = sink.Write(pcm20ms)
	time.Sleep(20 * time.Millisecond)
	if mockWriter.started.Load() {
		t.Fatal("PortAudio stream started prematurely at 40ms before 60ms watermark")
	}

	// 3. Write third 20ms packet (cumulative 60ms >= watermark)
	_, _ = sink.Write(pcm20ms)

	// Pump worker should transition to StatePriming -> StateStreaming and call Start()
	deadline := time.Now().Add(500 * time.Millisecond)
	for time.Now().Before(deadline) {
		if mockWriter.started.Load() && sink.State() == StateStreaming {
			break
		}
		time.Sleep(5 * time.Millisecond)
	}

	if !mockWriter.started.Load() {
		t.Fatal("expected PortAudio stream to start after reaching pre-roll watermark")
	}
	if sink.State() != StateStreaming {
		t.Fatalf("expected state %v after watermark, got %v", StateStreaming, sink.State())
	}
}

func TestPrimedBufferedSink_ComfortSilenceInjection(t *testing.T) {
	var capturedBlocks [][]int16
	var captureMu sync.Mutex

	mockWriter := &mockPortAudioWriter{
		writeDelay: time.Millisecond,
	}

	sampleRate := 48000
	channels := 2
	framesPerBuffer := 256
	buf := make([]int16, framesPerBuffer*channels)

	mockWriter.writeHook = func() {
		captureMu.Lock()
		defer captureMu.Unlock()
		blk := make([]int16, len(buf))
		copy(blk, buf)
		capturedBlocks = append(capturedBlocks, blk)
	}

	sink := NewPrimedBufferedSink(
		mockWriter,
		buf,
		sampleRate,
		channels,
		WithPreRollWatermark(20*time.Millisecond), // 960 frames
		WithFramesPerBuffer(framesPerBuffer),
	)
	defer sink.Close()

	// Fill non-zero PCM: 1000 for left, 2000 for right
	pcmOnePacket := make([]byte, 960*channels*2)
	for i := 0; i < 960; i++ {
		binary.LittleEndian.PutUint16(pcmOnePacket[i*4:], uint16(1000))
		binary.LittleEndian.PutUint16(pcmOnePacket[i*4+2:], uint16(2000))
	}

	_, err := sink.Write(pcmOnePacket)
	if err != nil {
		t.Fatalf("Write: %v", err)
	}

	// Wait for stream to start and consume valid frames + transition into comfort silence
	deadline := time.Now().Add(500 * time.Millisecond)
	for time.Now().Before(deadline) {
		if sink.ComfortSilenceBlocks() >= 2 {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}

	silenceCount := sink.ComfortSilenceBlocks()
	if silenceCount < 2 {
		t.Fatalf("expected at least 2 comfort silence blocks, got %d", silenceCount)
	}

	captureMu.Lock()
	defer captureMu.Unlock()
	if len(capturedBlocks) < 4 {
		t.Fatalf("expected >= 4 blocks written, got %d", len(capturedBlocks))
	}

	// Verify the latter captured blocks consist strictly of comfort silence (0x0000)
	lastBlock := capturedBlocks[len(capturedBlocks)-1]
	for idx, val := range lastBlock {
		if val != 0 {
			t.Fatalf("expected comfort silence 0 at index %d, got %d", idx, val)
		}
	}
}

func TestPrimedBufferedSink_OverrunClamping(t *testing.T) {
	mockWriter := &mockPortAudioWriter{
		writeDelay: 50 * time.Millisecond, // slow consumer to induce overrun
	}

	sampleRate := 48000
	channels := 2
	frameSize := channels * 2
	framesPerBuffer := 256
	capacityDuration := 200 * time.Millisecond // 9600 frames = 38400 bytes

	sink := NewPrimedBufferedSink(
		mockWriter,
		nil,
		sampleRate,
		channels,
		WithPreRollWatermark(40*time.Millisecond),
		WithCapacity(capacityDuration),
		WithFramesPerBuffer(framesPerBuffer),
	)
	defer sink.Close()

	// Write 500ms of audio rapidly into 200ms capacity ring buffer
	burstFrames := int(sampleRate * 500 / 1000) // 24000 frames
	burstBytes := make([]byte, burstFrames*frameSize)
	for i := 0; i < len(burstBytes); i++ {
		burstBytes[i] = byte(i)
	}

	_, err := sink.Write(burstBytes)
	if err != nil {
		t.Fatalf("Write burst: %v", err)
	}

	dropped := sink.OverrunDroppedFrames()
	if dropped == 0 {
		t.Fatal("expected overrun frames to be dropped, got 0")
	}

	bufferedBytes := sink.Buffered()
	maxAllowedBytes := sink.capacityBytes
	if bufferedBytes > maxAllowedBytes {
		t.Fatalf("buffered bytes %d exceeded max capacity %d", bufferedBytes, maxAllowedBytes)
	}
}

func TestPrimedBufferedSink_ZeroAllocs(t *testing.T) {
	mockWriter := &mockPortAudioWriter{
		writeDelay: 0,
	}

	sampleRate := 48000
	channels := 2
	framesPerBuffer := 256

	sink := NewPrimedBufferedSink(
		mockWriter,
		nil,
		sampleRate,
		channels,
		WithPreRollWatermark(20*time.Millisecond),
		WithFramesPerBuffer(framesPerBuffer),
	)
	defer sink.Close()

	pcm := make([]byte, framesPerBuffer*channels*2)
	// Warm up
	_, _ = sink.Write(pcm)
	time.Sleep(20 * time.Millisecond)

	allocs := testing.AllocsPerRun(100, func() {
		_, _ = sink.Write(pcm)
	})

	if allocs != 0 {
		t.Fatalf("expected 0 allocs/op on Write path, got %v", allocs)
	}
}

func TestPrimedBufferedSink_CleanCloseNoGoroutineLeaks(t *testing.T) {
	initialGoroutines := runtime.NumGoroutine()

	var closedHookCalled atomic.Bool
	mockWriter := &mockPortAudioWriter{
		writeDelay: time.Millisecond,
	}

	sink := NewPrimedBufferedSink(
		mockWriter,
		nil,
		48000,
		2,
		WithPreRollWatermark(20*time.Millisecond),
		WithOnClose(func() {
			closedHookCalled.Store(true)
		}),
	)

	// Feed enough to start stream
	pcm := make([]byte, 1920*4)
	_, _ = sink.Write(pcm)
	time.Sleep(30 * time.Millisecond)

	if !mockWriter.started.Load() {
		t.Fatal("expected writer to have started")
	}

	// Close sink
	err := sink.Close()
	if err != nil {
		t.Fatalf("Close error: %v", err)
	}

	if !mockWriter.aborted.Load() {
		t.Fatal("expected PortAudio Abort() to be called on active stream close")
	}
	if !mockWriter.closed.Load() {
		t.Fatal("expected PortAudio Close() to be called")
	}
	if !closedHookCalled.Load() {
		t.Fatal("expected onClose hook to be invoked")
	}

	// Verify post-close behavior
	if n, err := sink.Write(pcm); !errors.Is(err, net.ErrClosed) || n != 0 {
		t.Fatalf("expected net.ErrClosed on post-close Write, got n=%d err=%v", n, err)
	}
	if n, err := sink.Read(pcm); !errors.Is(err, io.EOF) || n != 0 {
		t.Fatalf("expected io.EOF on post-close Read, got n=%d err=%v", n, err)
	}

	// Verify idempotency of Close()
	if err := sink.Close(); err != nil {
		t.Fatalf("second Close returned error: %v", err)
	}

	// Allow goroutines to exit and verify no leak
	deadline := time.Now().Add(1 * time.Second)
	for time.Now().Before(deadline) {
		currentGoroutines := runtime.NumGoroutine()
		if currentGoroutines <= initialGoroutines+1 { // allow test runner variance of 1
			return
		}
		runtime.Gosched()
		time.Sleep(10 * time.Millisecond)
	}

	if current := runtime.NumGoroutine(); current > initialGoroutines+2 {
		t.Errorf("potential goroutine leak: initial=%d, current=%d", initialGoroutines, current)
	}
}

func TestSPSCRingBuffer_WrapAround(t *testing.T) {
	frameSize := 4 // stereo 16-bit
	capacity := 16 // holds 4 frames (8 samples = 16 bytes)
	ring := newSPSCRingBuffer(capacity, frameSize)

	// Write 12 bytes (3 frames: [1, 2, 3])
	f1 := []byte{1, 0, 1, 0, 2, 0, 2, 0, 3, 0, 3, 0}
	ring.push(f1)
	if ring.buffered() != 12 {
		t.Fatalf("expected 12 buffered, got %d", ring.buffered())
	}

	// Pop 1 block of 2 frames (4 samples)
	dst := make([]int16, 4)
	hasData := ring.popBlock(dst)
	if !hasData {
		t.Fatal("expected hasData true")
	}
	if dst[0] != 1 || dst[1] != 1 || dst[2] != 2 || dst[3] != 2 {
		t.Fatalf("unexpected popped samples: %v", dst)
	}
	if ring.buffered() != 4 { // 1 frame remains (frame 3)
		t.Fatalf("expected 4 buffered, got %d", ring.buffered())
	}

	// Push 2 more frames (8 bytes) causing wrap around
	f2 := []byte{4, 0, 4, 0, 5, 0, 5, 0}
	ring.push(f2)
	if ring.buffered() != 12 {
		t.Fatalf("expected 12 buffered, got %d", ring.buffered())
	}

	// Pop 2 frames: should return frame 3 and frame 4 across wrap boundary
	hasData = ring.popBlock(dst)
	if !hasData {
		t.Fatal("expected hasData true")
	}
	if dst[0] != 3 || dst[1] != 3 || dst[2] != 4 || dst[3] != 4 {
		t.Fatalf("wrap around data corrupt: %v", dst)
	}

	// Pop remaining 1 frame + partial comfort silence
	hasData = ring.popBlock(dst)
	if !hasData {
		t.Fatal("expected hasData true for partial frame")
	}
	if dst[0] != 5 || dst[1] != 5 {
		t.Fatalf("frame 5 sample mismatch: %v", dst)
	}
	if dst[2] != 0 || dst[3] != 0 {
		t.Fatalf("expected partial fill to zero-pad remaining samples: %v", dst)
	}

	// Pop on empty buffer -> pure comfort silence
	hasData = ring.popBlock(dst)
	if hasData {
		t.Fatal("expected hasData false on empty buffer")
	}
	for i, v := range dst {
		if v != 0 {
			t.Fatalf("expected zero at index %d, got %d", i, v)
		}
	}
}

func TestSPSCRingBuffer_ByteConversionEndianness(t *testing.T) {
	src := []byte{0x00, 0x00, 0xFF, 0x7F, 0x00, 0x80, 0x01, 0x00}
	dst := make([]int16, 4)
	convertBytesToInt16LE(src, dst)

	expected := []int16{0, 32767, -32768, 1}
	for i, exp := range expected {
		if dst[i] != exp {
			t.Errorf("sample %d: expected %d, got %d", i, exp, dst[i])
		}
	}

	buf := new(bytes.Buffer)
	for _, v := range dst {
		_ = binary.Write(buf, binary.LittleEndian, v)
	}
	if !bytes.Equal(buf.Bytes(), src) {
		t.Fatalf("roundtrip conversion mismatch")
	}
}

func TestPrimedBufferedSink_PartialFrameAlignmentAndSpill(t *testing.T) {
	mockWriter := &mockPortAudioWriter{writeDelay: 0}
	channels := 2
	framesPerBuffer := 256
	sink := NewPrimedBufferedSink(
		mockWriter,
		nil,
		48000,
		channels,
		WithPreRollWatermark(10*time.Millisecond),
		WithFramesPerBuffer(framesPerBuffer),
	)
	defer sink.Close()

	// frameSize is 4 bytes (stereo 16-bit)
	// Write 5 bytes: 1 full frame (4 bytes: [1000, 2000]) + 1 partial byte
	p1 := []byte{0xe8, 0x03, 0xd0, 0x07, 0x42}
	n, err := sink.Write(p1)
	if err != nil || n != len(p1) {
		t.Fatalf("Write 1: n=%d err=%v", n, err)
	}

	// Write 3 bytes completing the second frame: 3 bytes + previous 1 byte = 4 bytes ([3000, 4000])
	// Second frame: 3000 (0x0BB8), 4000 (0x0FA0) -> [0xB8, 0x0B, 0xA0, 0x0F]
	// First byte was 0x42 (so lower byte of left sample is 0x42).
	// Let's supply the remaining 3 bytes: [0x0B, 0xA0, 0x0F]
	p2 := []byte{0x0B, 0xA0, 0x0F}
	n, err = sink.Write(p2)
	if err != nil || n != len(p2) {
		t.Fatalf("Write 2: n=%d err=%v", n, err)
	}

	// Verify buffer has 2 full frames (8 bytes)
	if sink.Buffered() != 8 {
		t.Fatalf("expected 8 bytes buffered, got %d", sink.Buffered())
	}
}

func TestPrimedBufferedSink_RapidConcurrentOpenClose(t *testing.T) {
	for iter := 0; iter < 20; iter++ {
		mockWriter := &mockPortAudioWriter{writeDelay: time.Microsecond}
		sink := NewPrimedBufferedSink(
			mockWriter,
			nil,
			48000,
			2,
			WithPreRollWatermark(10*time.Millisecond),
			WithFramesPerBuffer(256),
		)

		var wg sync.WaitGroup
		wg.Add(2)

		// Goroutine 1: Rapid writes
		go func() {
			defer wg.Done()
			pcm := make([]byte, 1024)
			for i := 0; i < 50; i++ {
				_, err := sink.Write(pcm)
				if err != nil {
					return
				}
				time.Sleep(100 * time.Microsecond)
			}
		}()

		// Goroutine 2: Concurrent Close
		go func() {
			defer wg.Done()
			time.Sleep(time.Duration(iter*100) * time.Microsecond)
			_ = sink.Close()
		}()

		wg.Wait()
		_ = sink.Close()

		if sink.State() != StateClosed {
			t.Fatalf("iter %d: expected StateClosed, got %v", iter, sink.State())
		}
	}
}

func TestPrimedBufferedSink_ShortWriteContract(t *testing.T) {
	mockWriter := &mockPortAudioWriter{writeDelay: 10 * time.Millisecond}
	sink := NewPrimedBufferedSink(
		mockWriter,
		nil,
		48000,
		2,
		WithCapacity(50*time.Millisecond),
		WithFramesPerBuffer(256),
	)
	defer sink.Close()

	// Write 100ms of data (exceeding 50ms capacity)
	pcm := make([]byte, 48000*4*100/1000)
	n, err := sink.Write(pcm)
	if err != nil {
		t.Fatalf("Write returned error: %v", err)
	}
	if n != len(pcm) {
		t.Fatalf("io.Writer contract violated: wrote %d, got n=%d", len(pcm), n)
	}
	if sink.OverrunDroppedFrames() == 0 {
		t.Fatal("expected overrun dropped frames > 0")
	}
}
