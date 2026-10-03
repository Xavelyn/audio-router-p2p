package audio

import (
	"encoding/binary"
	"errors"
	"io"
	"math"
	"reflect"
	"sync"
	"testing"
	"time"
)

// mockAudioStream provides an in-memory AudioStream for unit tests.
type mockAudioStream struct {
	mu     sync.Mutex
	data   []byte
	pos    int
	closed bool
}

func newMockAudioStream(initialData []byte) *mockAudioStream {
	cp := make([]byte, len(initialData))
	copy(cp, initialData)
	return &mockAudioStream{data: cp}
}

func (m *mockAudioStream) Read(dst []byte) (int, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.pos >= len(m.data) {
		if m.closed {
			return 0, io.EOF
		}
		return 0, io.EOF
	}
	n := copy(dst, m.data[m.pos:])
	m.pos += n
	return n, nil
}

func (m *mockAudioStream) Write(src []byte) (int, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.closed {
		return 0, errors.New("stream closed")
	}
	m.data = append(m.data, src...)
	return len(src), nil
}

func (m *mockAudioStream) Close() error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.closed = true
	return nil
}

func (m *mockAudioStream) BytesWritten() []byte {
	m.mu.Lock()
	defer m.mu.Unlock()
	cp := make([]byte, len(m.data))
	copy(cp, m.data)
	return cp
}

// generateSineWave generates stereo 16-bit little-endian PCM samples.
func generateSineWave(freq float64, sampleRate, frames int) []byte {
	buf := make([]byte, frames*4) // 2 channels * 2 bytes
	for i := 0; i < frames; i++ {
		t := float64(i) / float64(sampleRate)
		val := int16(math.Sin(2*math.Pi*freq*t) * 16000)
		binary.LittleEndian.PutUint16(buf[i*4:], uint16(val))
		binary.LittleEndian.PutUint16(buf[i*4+2:], uint16(val))
	}
	return buf
}

func TestBuildRateCandidates(t *testing.T) {
	tests := []struct {
		name      string
		requested int
		defRate   int
		want      []int
	}{
		{
			name:      "Standard 48k requested with 44.1k default",
			requested: 48000,
			defRate:   44100,
			want:      []int{48000, 44100, 96000},
		},
		{
			name:      "Standard 44.1k requested with 44.1k default",
			requested: 44100,
			defRate:   44100,
			want:      []int{44100, 48000, 96000},
		},
		{
			name:      "High-res 96k requested with 48k default",
			requested: 96000,
			defRate:   48000,
			want:      []int{96000, 48000, 44100},
		},
		{
			name:      "192k requested with 0 default",
			requested: 192000,
			defRate:   0,
			want:      []int{192000, 48000, 44100, 96000},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := buildRateCandidates(tt.requested, tt.defRate)
			if !reflect.DeepEqual(got, tt.want) {
				t.Errorf("buildRateCandidates(%d, %d) = %v, want %v", tt.requested, tt.defRate, got, tt.want)
			}
		})
	}
}

func TestResampleStreamValidation(t *testing.T) {
	mock := newMockAudioStream(nil)

	if _, err := NewResampleInputStream(nil, 44100, 48000, 2); err == nil {
		t.Error("expected error for nil underlying input stream")
	}
	if _, err := NewResampleOutputStream(nil, 48000, 44100, 2); err == nil {
		t.Error("expected error for nil underlying output stream")
	}

	if _, err := NewResampleInputStream(mock, 0, 48000, 2); err == nil {
		t.Error("expected error for zero hwRate")
	}
	if _, err := NewResampleOutputStream(mock, 48000, -1, 2); err == nil {
		t.Error("expected error for negative hwRate")
	}

	if _, err := NewResampleInputStream(mock, 44100, 48000, 0); err == nil {
		t.Error("expected error for 0 channels")
	}
	if _, err := NewResampleOutputStream(mock, 48000, 44100, 9); err == nil {
		t.Error("expected error for >8 channels")
	}
}

func TestResampleInputStream_44100_to_48000(t *testing.T) {
	const (
		inRate   = 44100
		outRate  = 48000
		channels = 2
		inFrames = 44100 // 1 second
	)

	pcmIn := generateSineWave(440, inRate, inFrames)
	mock := newMockAudioStream(pcmIn)

	resStream, err := NewResampleInputStream(mock, inRate, outRate, channels)
	if err != nil {
		t.Fatalf("NewResampleInputStream: %v", err)
	}
	defer resStream.Close()

	readBuf := make([]byte, 1024)
	var totalRead int

	for {
		n, rErr := resStream.Read(readBuf)
		if n > 0 {
			totalRead += n
			if n%(channels*2) != 0 {
				t.Fatalf("Read returned %d bytes, not frame aligned", n)
			}
		}
		if rErr == io.EOF {
			break
		}
		if rErr != nil {
			t.Fatalf("unexpected read error: %v", rErr)
		}
	}

	outFrames := totalRead / (channels * 2)
	// Expected ~48000 frames (within 0.2%)
	expectedFrames := outRate
	tolerance := float64(expectedFrames) * 0.005
	diff := math.Abs(float64(outFrames - expectedFrames))
	if diff > tolerance {
		t.Errorf("expected ~%d frames, got %d (diff: %f > tolerance: %f)", expectedFrames, outFrames, diff, tolerance)
	}
}

func TestResampleInputStream_48000_to_44100(t *testing.T) {
	const (
		inRate   = 48000
		outRate  = 44100
		channels = 2
		inFrames = 48000 // 1 second
	)

	pcmIn := generateSineWave(440, inRate, inFrames)
	mock := newMockAudioStream(pcmIn)

	resStream, err := NewResampleInputStream(mock, inRate, outRate, channels)
	if err != nil {
		t.Fatalf("NewResampleInputStream: %v", err)
	}
	defer resStream.Close()

	readBuf := make([]byte, 1024)
	var totalRead int

	for {
		n, rErr := resStream.Read(readBuf)
		if n > 0 {
			totalRead += n
			if n%(channels*2) != 0 {
				t.Fatalf("Read returned %d bytes, not frame aligned", n)
			}
		}
		if rErr == io.EOF {
			break
		}
		if rErr != nil {
			t.Fatalf("unexpected read error: %v", rErr)
		}
	}

	outFrames := totalRead / (channels * 2)
	expectedFrames := outRate
	tolerance := float64(expectedFrames) * 0.005
	diff := math.Abs(float64(outFrames - expectedFrames))
	if diff > tolerance {
		t.Errorf("expected ~%d frames, got %d (diff: %f > tolerance: %f)", expectedFrames, outFrames, diff, tolerance)
	}
}

func TestResampleOutputStream_44100_to_48000(t *testing.T) {
	const (
		reqRate  = 44100
		hwRate   = 48000
		channels = 2
		frames   = 44100
	)

	mock := newMockAudioStream(nil)
	resStream, err := NewResampleOutputStream(mock, reqRate, hwRate, channels)
	if err != nil {
		t.Fatalf("NewResampleOutputStream: %v", err)
	}
	defer resStream.Close()

	pcmIn := generateSineWave(440, reqRate, frames)

	// Write in multiple blocks
	blockSize := 1024 * channels * 2
	for off := 0; off < len(pcmIn); off += blockSize {
		end := off + blockSize
		if end > len(pcmIn) {
			end = len(pcmIn)
		}
		n, wErr := resStream.Write(pcmIn[off:end])
		if wErr != nil {
			t.Fatalf("Write error: %v", wErr)
		}
		if n != (end - off) {
			t.Fatalf("Write returned %d, want %d", n, end-off)
		}
	}

	written := mock.BytesWritten()
	hwFrames := len(written) / (channels * 2)
	expectedFrames := hwRate
	tolerance := float64(expectedFrames) * 0.005
	diff := math.Abs(float64(hwFrames - expectedFrames))
	if diff > tolerance {
		t.Errorf("expected ~%d hw frames, got %d (diff: %f > tolerance: %f)", expectedFrames, hwFrames, diff, tolerance)
	}
}

func TestResampleOutputStream_48000_to_44100(t *testing.T) {
	const (
		reqRate  = 48000
		hwRate   = 44100
		channels = 2
		frames   = 48000
	)

	mock := newMockAudioStream(nil)
	resStream, err := NewResampleOutputStream(mock, reqRate, hwRate, channels)
	if err != nil {
		t.Fatalf("NewResampleOutputStream: %v", err)
	}
	defer resStream.Close()

	pcmIn := generateSineWave(440, reqRate, frames)

	blockSize := 1024 * channels * 2
	for off := 0; off < len(pcmIn); off += blockSize {
		end := off + blockSize
		if end > len(pcmIn) {
			end = len(pcmIn)
		}
		n, wErr := resStream.Write(pcmIn[off:end])
		if wErr != nil {
			t.Fatalf("Write error: %v", wErr)
		}
		if n != (end - off) {
			t.Fatalf("Write returned %d, want %d", n, end-off)
		}
	}

	written := mock.BytesWritten()
	hwFrames := len(written) / (channels * 2)
	expectedFrames := hwRate
	tolerance := float64(expectedFrames) * 0.005
	diff := math.Abs(float64(hwFrames - expectedFrames))
	if diff > tolerance {
		t.Errorf("expected ~%d hw frames, got %d (diff: %f > tolerance: %f)", expectedFrames, hwFrames, diff, tolerance)
	}
}

func TestResampleStream_SpillBufferManagement(t *testing.T) {
	// Tests that reading with a tiny destination buffer (exercising spillBuf) drains accurately.
	pcmIn := generateSineWave(1000, 44100, 4410)
	mock := newMockAudioStream(pcmIn)

	resStream, err := NewResampleInputStream(mock, 44100, 48000, 2)
	if err != nil {
		t.Fatalf("NewResampleInputStream: %v", err)
	}
	defer resStream.Close()

	tinyBuf := make([]byte, 40) // 10 frames of stereo
	totalRead := 0

	for {
		n, rErr := resStream.Read(tinyBuf)
		if n > 0 {
			totalRead += n
			if n%4 != 0 {
				t.Fatalf("unaligned read: %d", n)
			}
		}
		if rErr == io.EOF {
			break
		}
		if rErr != nil {
			t.Fatalf("Read error: %v", rErr)
		}
	}

	outFrames := totalRead / 4
	if outFrames < 4700 || outFrames > 4900 {
		t.Errorf("unexpected total frames via tiny buffer reads: %d", outFrames)
	}
}

func TestResampleStream_LifecycleAndClose(t *testing.T) {
	mock := newMockAudioStream([]byte{1, 2, 3, 4, 5, 6, 7, 8})
	st, err := NewResampleInputStream(mock, 44100, 48000, 2)
	if err != nil {
		t.Fatalf("NewResampleInputStream: %v", err)
	}

	if err := st.Close(); err != nil {
		t.Fatalf("Close error: %v", err)
	}
	if !mock.closed {
		t.Error("expected underlying stream to be closed")
	}

	// Read after close and drain should return EOF
	buf := make([]byte, 16)
	_, rErr := st.Read(buf)
	if rErr != io.EOF {
		t.Errorf("expected io.EOF on read after close, got %v", rErr)
	}
}

func TestResampleStream_ZeroDynamicAllocationsOnHotPath(t *testing.T) {
	inPcm := generateSineWave(440, 44100, 1024)
	mockIn := newMockAudioStream(inPcm)
	inStream, err := NewResampleInputStream(mockIn, 44100, 48000, 2)
	if err != nil {
		t.Fatalf("NewResampleInputStream: %v", err)
	}
	defer inStream.Close()

	// Prime stream
	readDst := make([]byte, 512)
	_, _ = inStream.Read(readDst)

	allocsRead := testing.AllocsPerRun(100, func() {
		// Read hot path
		_, _ = inStream.Read(readDst)
	})
	if allocsRead > 0 {
		t.Errorf("expected 0 allocations on Read hot path, got %f", allocsRead)
	}

	mockOut := newMockAudioStream(nil)
	outStream, err := NewResampleOutputStream(mockOut, 48000, 44100, 2)
	if err != nil {
		t.Fatalf("NewResampleOutputStream: %v", err)
	}
	defer outStream.Close()

	writeSrc := generateSineWave(440, 48000, 256)
	// Prime stream
	_, _ = outStream.Write(writeSrc)

	allocsWrite := testing.AllocsPerRun(100, func() {
		_, _ = outStream.Write(writeSrc)
	})
	if allocsWrite > 0 {
		t.Errorf("expected 0 allocations on Write hot path, got %f", allocsWrite)
	}
}

type blockingUnderlyingStream struct {
	mu     sync.Mutex
	closed chan struct{}
}

func newBlockingUnderlyingStream() *blockingUnderlyingStream {
	return &blockingUnderlyingStream{
		closed: make(chan struct{}),
	}
}

func (b *blockingUnderlyingStream) Read(dst []byte) (int, error) {
	select {
	case <-b.closed:
		return 0, io.EOF
	default:
		if len(dst) >= 4 {
			return 4, nil
		}
		return 0, nil
	}
}

func (b *blockingUnderlyingStream) Write(src []byte) (int, error) {
	select {
	case <-b.closed:
		return 0, errors.New("closed")
	default:
		return len(src), nil
	}
}

func (b *blockingUnderlyingStream) Close() error {
	b.mu.Lock()
	defer b.mu.Unlock()
	select {
	case <-b.closed:
	default:
		close(b.closed)
	}
	return nil
}

func TestResampleStream_ConcurrentReadAndClose_NoDeadlock(t *testing.T) {
	for i := 0; i < 20; i++ {
		mock := newBlockingUnderlyingStream()
		st, err := NewResampleInputStream(mock, 44100, 48000, 2)
		if err != nil {
			t.Fatalf("NewResampleInputStream: %v", err)
		}

		readDone := make(chan struct{})
		closeDone := make(chan struct{})

		go func() {
			defer close(readDone)
			buf := make([]byte, 128)
			for {
				_, rErr := st.Read(buf)
				if rErr != nil {
					return
				}
			}
		}()

		go func() {
			defer close(closeDone)
			_ = st.Close()
		}()

		select {
		case <-readDone:
		case <-time.After(2 * time.Second):
			t.Fatal("deadlock detected: Read did not terminate within timeout")
		}

		select {
		case <-closeDone:
		case <-time.After(2 * time.Second):
			t.Fatal("deadlock detected: Close did not return within timeout")
		}
	}
}

func TestResampleOutputStream_WriteSpillCombinedChunking(t *testing.T) {
	mock := newMockAudioStream(nil)
	channels := 2
	frameSize := channels * 2 // 4 bytes
	st, err := NewResampleOutputStream(mock, 48000, 44100, channels)
	if err != nil {
		t.Fatalf("NewResampleOutputStream: %v", err)
	}
	defer st.Close()

	// Write unaligned bytes: 3 bytes (leaving 3 bytes in writeSpill)
	data1 := []byte{1, 2, 3}
	n1, err1 := st.Write(data1)
	if err1 != nil || n1 != len(data1) {
		t.Fatalf("Write 1 failed: n=%d err=%v", n1, err1)
	}
	// Nothing written to mock yet because less than 1 frame
	if len(mock.BytesWritten()) != 0 {
		t.Fatalf("expected 0 bytes written to underlying stream, got %d", len(mock.BytesWritten()))
	}

	// Write a larger chunk: 1001 bytes (3 + 1001 = 1004 bytes = 251 frames)
	data2 := make([]byte, 1001)
	for i := range data2 {
		data2[i] = byte(i % 256)
	}
	n2, err2 := st.Write(data2)
	if err2 != nil || n2 != len(data2) {
		t.Fatalf("Write 2 failed: n=%d err=%v", n2, err2)
	}

	// Verify underlying received resampled data
	written := mock.BytesWritten()
	if len(written) == 0 {
		t.Fatal("expected resampled output written to underlying stream")
	}
	if len(written)%frameSize != 0 {
		t.Fatalf("written bytes %d not frame-aligned", len(written))
	}
}
