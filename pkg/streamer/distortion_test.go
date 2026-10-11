package streamer

import (
	"encoding/binary"
	"math"
	"net"
	"strconv"
	"testing"
	"time"

	"github.com/audio-router-p2p/pkg/audio"
	"github.com/audio-router-p2p/pkg/transport"
)

func generatePacedSineWave(sampleRate, channels, numPackets, packetTimeMs int, freq float64) [][]byte {
	samplesPerPacket := (sampleRate * packetTimeMs) / 1000
	packets := make([][]byte, numPackets)

	t := 0.0
	step := 2.0 * math.Pi * freq / float64(sampleRate)

	for p := 0; p < numPackets; p++ {
		buf := make([]byte, samplesPerPacket*channels*2)
		for s := 0; s < samplesPerPacket; s++ {
			sampleVal := int16(20000.0 * math.Sin(t))
			t += step
			for c := 0; c < channels; c++ {
				offset := (s*channels + c) * 2
				binary.LittleEndian.PutUint16(buf[offset:], uint16(sampleVal))
			}
		}
		packets[p] = buf
	}
	return packets
}

func checkDistortion(t *testing.T, name string, captured []byte, sampleRate, channels int, freq float64) {
	if len(captured) < 4800*channels*2 {
		t.Fatalf("[%s] captured too few bytes: %d", name, len(captured))
		return
	}
	totalSamples := len(captured) / (2 * channels)
	t.Logf("[%s] Captured %d frames (%d bytes)", name, totalSamples, len(captured))

	// Inspect samples in channel 0
	samples := make([]float64, totalSamples)
	for i := 0; i < totalSamples; i++ {
		samples[i] = float64(int16(binary.LittleEndian.Uint16(captured[i*channels*2:])))
	}

	// Look at consecutive differences or zero crossings or pitch
	zeroCrossings := 0
	silentSamples := 0
	maxVal := 0.0
	for i := 0; i < totalSamples; i++ {
		if math.Abs(samples[i]) > maxVal {
			maxVal = math.Abs(samples[i])
		}
		if math.Abs(samples[i]) < 10 {
			silentSamples++
		}
		if i > 0 && ((samples[i-1] <= 0 && samples[i] > 0) || (samples[i-1] >= 0 && samples[i] < 0)) {
			zeroCrossings++
		}
	}
	durationSec := float64(totalSamples) / float64(sampleRate)
	estimatedFreq := float64(zeroCrossings) / (2.0 * durationSec)
	t.Logf("[%s] maxVal=%.1f silentRatio=%.2f zeroCrossings=%d estimatedFreq=%.1f Hz (expected ~%.1f Hz)",
		name, maxVal, float64(silentSamples)/float64(totalSamples), zeroCrossings, estimatedFreq, freq)

	// Check consecutive jumps (clicks / discontinuities)
	largeJumps := 0
	for i := 1; i < totalSamples; i++ {
		diff := math.Abs(samples[i] - samples[i-1])
		// For 440 Hz at 48000 Hz with amp 20000, max slope is 20000 * 2 * pi * 440 / 48000 ≈ 1150
		if diff > 3000 {
			largeJumps++
			if largeJumps <= 10 {
				t.Logf("[%s] Large discontinuity at frame %d: from %.1f to %.1f (diff=%.1f)", name, i, samples[i-1], samples[i], diff)
			}
		}
	}
	t.Logf("[%s] Large jumps (discontinuities): %d", name, largeJumps)

	if largeJumps > 0 {
		t.Fatalf("[%s] FAIL: detected %d large discontinuities/jumps (robotic distortion)", name, largeJumps)
	}
	freqDiff := math.Abs(estimatedFreq - freq)
	if freqDiff > 10.0 {
		t.Fatalf("[%s] FAIL: pitch shifted / frequency distortion: got %.1f Hz, want ~%.1f Hz", name, estimatedFreq, freq)
	}
}

func TestCheckUDPLoopback(t *testing.T) {
	const (
		rate       = 48000
		channels   = 2
		packetTime = 20
		numPackets = 50
	)
	port := freePort(t)
	addr := net.JoinHostPort("127.0.0.1", strconv.Itoa(port))

	trans, err := transport.NewUDPTransport(addr)
	if err != nil {
		t.Fatalf("NewUDPTransport: %v", err)
	}
	defer trans.Close()

	cfg := DefaultStreamConfig(rate, channels)
	cfg.PacketTime = packetTime

	inDev := audio.NewVirtualAudioDevice()
	defer inDev.Close()
	outDev := audio.NewVirtualAudioDevice()
	defer outDev.Close()

	// Empty localIPs so it sends over UDP
	st, err := NewStreamer(trans, []string{}, cfg, "loopback-node")
	if err != nil {
		t.Fatalf("NewStreamer: %v", err)
	}
	defer st.Stop()

	streamID := DeriveStreamID("loopback-secret", "intra-node-test")
	st.SetStreamID(streamID)

	if err := st.Start(inDev, []string{addr}, outDev); err != nil {
		t.Fatalf("Start: %v", err)
	}

	testPackets := generatePacedSineWave(rate, channels, numPackets, packetTime, 440.0)
	go func() {
		// Pre-buffer initial packets to simulate hardware ADC DMA cushion
		if len(testPackets) > 2 {
			inDev.InjectPush(testPackets[0])
			inDev.InjectPush(testPackets[1])
			testPackets = testPackets[2:]
		}
		for _, pkt := range testPackets {
			inDev.InjectPush(pkt)
			time.Sleep(20 * time.Millisecond)
		}
	}()

	time.Sleep(1000 * time.Millisecond)
	captured := outDev.CapturedBytes()
	checkDistortion(t, "UDPLoopback", captured, rate, channels, 440.0)
}

func TestCheckDirectLocalBypass(t *testing.T) {
	const (
		rate       = 48000
		channels   = 2
		packetTime = 20
		numPackets = 50
	)
	localIPs := []string{"127.0.0.1"}

	cfg := DefaultStreamConfig(rate, channels)
	cfg.PacketTime = packetTime

	inDev := audio.NewVirtualAudioDevice()
	defer inDev.Close()
	outDev := audio.NewVirtualAudioDevice()
	defer outDev.Close()

	st, err := NewStreamer(nil, localIPs, cfg, "local-node")
	if err != nil {
		t.Fatalf("NewStreamer: %v", err)
	}
	defer st.Stop()

	if err := st.Start(inDev, []string{"127.0.0.1:45678"}, outDev); err != nil {
		t.Fatalf("Start: %v", err)
	}

	testPackets := generatePacedSineWave(rate, channels, numPackets, packetTime, 440.0)
	go func() {
		for _, pkt := range testPackets {
			inDev.InjectPush(pkt)
			time.Sleep(20 * time.Millisecond)
		}
	}()

	time.Sleep(1200 * time.Millisecond)
	captured := outDev.CapturedBytes()
	checkDistortion(t, "DirectLocalBypass", captured, rate, channels, 440.0)
}

func TestCheckFormatAdapter(t *testing.T) {
	rates := []struct {
		inRate  int
		outRate int
	}{
		{44100, 48000},
		{48000, 44100},
		{48000, 48000},
		{16000, 48000},
	}

	for _, r := range rates {
		t.Run(strconv.Itoa(r.inRate)+"->"+strconv.Itoa(r.outRate), func(t *testing.T) {
			adapter := NewIngressAdapter(r.outRate, 2)
			testPackets := generatePacedSineWave(r.inRate, 2, 50, 20, 440.0)

			var outTotal []byte
			for _, pkt := range testPackets {
				out, err := adapter.Process(pkt, r.inRate, 2)
				if err != nil {
					t.Fatalf("Process: %v", err)
				}
				outTotal = append(outTotal, out...)
			}
			checkDistortion(t, strconv.Itoa(r.inRate)+"->"+strconv.Itoa(r.outRate), outTotal, r.outRate, 2, 440.0)
		})
	}
}

type sinePAStreamReader struct {
	buf      []int16
	t        float64
	step     float64
	channels int
}

func (s *sinePAStreamReader) Read() error {
	frames := len(s.buf) / s.channels
	for i := 0; i < frames; i++ {
		val := int16(20000.0 * math.Sin(s.t))
		s.t += s.step
		for c := 0; c < s.channels; c++ {
			s.buf[i*s.channels+c] = val
		}
	}
	return nil
}

func (s *sinePAStreamReader) Stop() error  { return nil }
func (s *sinePAStreamReader) Close() error { return nil }

// TestCheckPortAudioInputChunkedDistortion validates that an input stream delivering
// hardware-sized buffers (256 frames = 1024 bytes) read in 20ms chunks (960 frames = 3840 bytes)
// suffers zero dropped frames, zero phase jumps/clicks, and zero frequency distortion when routed locally.
func TestCheckPortAudioInputChunkedDistortion(t *testing.T) {
	const (
		rate       = 48000
		channels   = 2
		packetTime = 20
		blocksBuf  = 256
		numPackets = 50
	)
	localIPs := []string{"127.0.0.1"}

	cfg := DefaultStreamConfig(rate, channels)
	cfg.PacketTime = packetTime

	paBuf := make([]int16, blocksBuf*channels)
	mockStream := &sinePAStreamReader{
		buf:      paBuf,
		step:     2.0 * math.Pi * 440.0 / float64(rate),
		channels: channels,
	}

	inDev := audio.NewPortaudioInputForTest(mockStream, paBuf)
	defer inDev.Close()

	outDev := audio.NewVirtualAudioDevice()
	defer outDev.Close()

	st, err := NewStreamer(nil, localIPs, cfg, "local-node")
	if err != nil {
		t.Fatalf("NewStreamer: %v", err)
	}
	defer st.Stop()

	if err := st.Start(inDev, []string{"127.0.0.1:45678"}, outDev); err != nil {
		t.Fatalf("Start: %v", err)
	}

	// Read 50 packets (1 second) of audio through streamer
	minExpected := rate * channels * 2 // 192000 bytes for 1 sec
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if len(outDev.CapturedBytes()) >= minExpected {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}

	captured := outDev.CapturedBytes()
	checkDistortion(t, "PortAudioInputChunkedBypass", captured, rate, channels, 440.0)
}

// TestCheckLocalRoute_QuietAudioNoDTXChopping tests that low-energy audio below the DTX
// silence threshold (-50 dBFS) is not gated/chopped when routed locally.
func TestCheckLocalRoute_QuietAudioNoDTXChopping(t *testing.T) {
	const (
		rate       = 48000
		channels   = 2
		packetTime = 20
		numPackets = 50
	)
	localIPs := []string{"127.0.0.1"}

	cfg := DefaultStreamConfig(rate, channels)
	cfg.PacketTime = packetTime

	inDev := audio.NewVirtualAudioDevice()
	defer inDev.Close()
	outDev := audio.NewVirtualAudioDevice()
	defer outDev.Close()

	st, err := NewStreamer(nil, localIPs, cfg, "local-node")
	if err != nil {
		t.Fatalf("NewStreamer: %v", err)
	}
	defer st.Stop()

	if err := st.Start(inDev, []string{"127.0.0.1:45678"}, outDev); err != nil {
		t.Fatalf("Start: %v", err)
	}

	// Generate quiet sine wave (amp 50 out of 32767 is ~ -56 dBFS, below -50 dBFS DTX threshold)
	samplesPerPacket := (rate * packetTime) / 1000
	step := 2.0 * math.Pi * 440.0 / float64(rate)
	tCur := 0.0
	testPackets := make([][]byte, numPackets)
	for p := 0; p < numPackets; p++ {
		buf := make([]byte, samplesPerPacket*channels*2)
		for s := 0; s < samplesPerPacket; s++ {
			sampleVal := int16(50.0 * math.Sin(tCur))
			tCur += step
			for c := 0; c < channels; c++ {
				offset := (s*channels + c) * 2
				binary.LittleEndian.PutUint16(buf[offset:], uint16(sampleVal))
			}
		}
		testPackets[p] = buf
	}

	go func() {
		for _, pkt := range testPackets {
			inDev.InjectPush(pkt)
			time.Sleep(10 * time.Millisecond)
		}
	}()

	minExpected := samplesPerPacket * channels * 2 * numPackets
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if len(outDev.CapturedBytes()) >= minExpected {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}

	captured := outDev.CapturedBytes()
	if len(captured) < minExpected {
		t.Fatalf("expected >= %d captured bytes for quiet audio, got %d", minExpected, len(captured))
	}

	// Verify the quiet audio wasn't muted to zero silence (sound arrived)
	totalSamples := len(captured) / (2 * channels)
	nonZero := 0
	for i := 0; i < totalSamples; i++ {
		sample := int16(binary.LittleEndian.Uint16(captured[i*channels*2:]))
		if sample != 0 {
			nonZero++
		}
	}
	if nonZero == 0 {
		t.Fatalf("quiet audio was completely muted to zeroes by DTX on local route")
	}
}
