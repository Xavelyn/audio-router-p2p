package streamer

import (
	"fmt"
	"time"

	"github.com/audio-router-p2p/pkg/transport"
)

// maxAudioDatagramBytes leaves room below the IPv6 minimum MTU for IP/UDP
// headers and DTLS record overhead. Reserve the FEC header as well so enabling
// parity later does not increase the datagram beyond this limit.
const maxAudioDatagramBytes = 1150

// NetworkPCMConfig picks the longest whole-millisecond PCM16 packet, up to
// 20ms, that fits the datagram budget. Both endpoints must call this with the
// same format so the playout cadence and frame sizes match.
func NetworkPCMConfig(sampleRate, channels int) (StreamConfig, error) {
	if sampleRate <= 0 || channels < 1 || channels > 8 {
		return StreamConfig{}, fmt.Errorf("unsupported PCM format: %d Hz, %d channels", sampleRate, channels)
	}
	// Bound the format before multiplying, including on 32-bit systems.
	if sampleRate > 384000 {
		return StreamConfig{}, fmt.Errorf("unsupported PCM sample rate: %d Hz", sampleRate)
	}
	for ms := 20; ms >= 1; ms-- {
		frames := sampleRate * ms / 1000
		if frames == 0 {
			continue
		}
		bytes := frames * channels * 2
		if bytes+transport.HeaderSize+transport.FECHeaderSize <= maxAudioDatagramBytes {
			cfg := DefaultStreamConfig(sampleRate, channels)
			cfg.PacketTime = ms
			cfg.DTX.FrameDuration = time.Duration(ms) * time.Millisecond
			return cfg, nil
		}
	}
	return StreamConfig{}, fmt.Errorf("PCM16 %d Hz/%d channels cannot fit a 1ms packet within %d bytes; use a lower rate, fewer channels, or a codec", sampleRate, channels, maxAudioDatagramBytes)
}
