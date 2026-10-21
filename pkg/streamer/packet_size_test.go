package streamer

import (
	"testing"
	"time"

	"github.com/audio-router-p2p/pkg/transport"
)

func TestNetworkPCMConfigFitsDatagram(t *testing.T) {
	for _, tc := range []struct {
		rate, channels int
	}{
		{44100, 1}, {44100, 2}, {48000, 1}, {48000, 2}, {48000, 8}, {96000, 2},
	} {
		cfg, err := NetworkPCMConfig(tc.rate, tc.channels)
		if err != nil {
			t.Fatalf("%d Hz/%d ch: %v", tc.rate, tc.channels, err)
		}
		if cfg.PacketTime < 1 || cfg.PacketTime > 20 {
			t.Fatalf("%d Hz/%d ch: invalid packet time %d", tc.rate, tc.channels, cfg.PacketTime)
		}
		if cfg.DTX.FrameDuration != time.Duration(cfg.PacketTime)*time.Millisecond {
			t.Fatalf("%d Hz/%d ch: DTX duration %v differs from packet time %dms", tc.rate, tc.channels, cfg.DTX.FrameDuration, cfg.PacketTime)
		}
		bytes := (cfg.SampleRate * cfg.PacketTime / 1000) * cfg.Channels * 2
		if got := bytes + transport.HeaderSize + transport.FECHeaderSize; got > maxAudioDatagramBytes {
			t.Fatalf("%d Hz/%d ch: datagram %d exceeds %d", tc.rate, tc.channels, got, maxAudioDatagramBytes)
		}
	}
}

func TestNetworkPCMConfigRejectsOversizeFormat(t *testing.T) {
	for _, tc := range []struct{ rate, channels int }{
		{0, 2}, {48000, 0}, {48000, 9}, {384000, 8},
	} {
		if _, err := NetworkPCMConfig(tc.rate, tc.channels); err == nil {
			t.Fatalf("expected unsupported format error for %d Hz/%d ch", tc.rate, tc.channels)
		}
	}
}

func TestNetworkPCMConfigUsesShortStereoPackets(t *testing.T) {
	cfg, err := NetworkPCMConfig(48000, 2)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.PacketTime != 5 {
		t.Fatalf("48k stereo packet time = %dms, want 5ms", cfg.PacketTime)
	}
	if cfg.FEC.Enabled {
		t.Fatal("FEC should remain opt-in")
	}
}
