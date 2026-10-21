package main

import (
	"encoding/binary"
	"testing"

	"github.com/audio-router-p2p/pkg/audio"
	"github.com/audio-router-p2p/pkg/router"
)

func constantStereo(frames int, left, right int16) []byte {
	pcm := make([]byte, frames*4)
	for i := 0; i < frames; i++ {
		binary.LittleEndian.PutUint16(pcm[4*i:], uint16(left))
		binary.LittleEndian.PutUint16(pcm[4*i+2:], uint16(right))
	}
	return pcm
}

func TestRouteMatrixLiveGainAndMute(t *testing.T) {
	sink := audio.NewVirtualAudioDevice()
	defer sink.Close()
	route := router.StreamRoute{
		ID: "matrix", SampleRate: 48000, Channels: 2,
		InChannels: 2, OutChannels: 2, Coefficients: []float64{1, 0, 0, 1},
	}
	filter, err := newRouteMatrix(sink, route)
	if err != nil {
		t.Fatal(err)
	}
	block := constantStereo(960, 10000, 4000)
	check := func(left, right int16) {
		t.Helper()
		sink.Reset()
		if n, err := filter.Write(block); err != nil || n != len(block) {
			t.Fatalf("Write: n=%d err=%v", n, err)
		}
		got := sink.CapturedBytes()
		if len(got) != len(block) {
			t.Fatalf("wrote %d bytes, want %d", len(got), len(block))
		}
		last := got[len(got)-4:]
		if l, r := int16(binary.LittleEndian.Uint16(last)), int16(binary.LittleEndian.Uint16(last[2:])); l != left || r != right {
			t.Fatalf("last frame = (%d, %d), want (%d, %d)", l, r, left, right)
		}
	}
	check(10000, 4000)
	mr := router.StreamRouteToMatrixRoute(route)
	mr.Coefficients = []float64{0.5, 0, 0, 1}
	mr.SlewMS = 10
	if err := filter.Update(mr); err != nil {
		t.Fatal(err)
	}
	check(5000, 4000)
	mr.Muted = true
	if err := filter.Update(mr); err != nil {
		t.Fatal(err)
	}
	check(0, 0)
	mr.Muted = false
	if err := filter.Update(mr); err != nil {
		t.Fatal(err)
	}
	check(5000, 4000)
}

func TestRouteMatrixRejectedChannelConversion(t *testing.T) {
	sink := audio.NewVirtualAudioDevice()
	defer sink.Close()
	if _, err := newRouteMatrix(sink, router.StreamRoute{
		ID: "unsupported", SampleRate: 48000, Channels: 2, InChannels: 1, OutChannels: 2,
	}); err == nil {
		t.Fatal("expected explicit error for unsupported live channel conversion")
	}
}
