package main

import (
	"encoding/binary"
	"testing"
	"time"

	"github.com/audio-router-p2p/pkg/audio"
)

func TestOutputBusMixAndCloseIsolation(t *testing.T) {
	device := audio.NewVirtualAudioDevice()
	bus := newOutputBus(device, 1000, 1, 10) // ten samples per tick
	a, b := bus.add("a"), bus.add("b")
	defer a.Close()
	defer b.Close()
	block := func(value int16) []byte {
		pcm := make([]byte, 20)
		for i := 0; i < 10; i++ {
			binary.LittleEndian.PutUint16(pcm[2*i:], uint16(value))
		}
		return pcm
	}
	if _, err := a.Write(block(18000)); err != nil {
		t.Fatal(err)
	}
	if _, err := b.Write(block(18000)); err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(time.Second)
	for device.Buffered() == 0 && time.Now().Before(deadline) {
		time.Sleep(time.Millisecond)
	}
	got := device.CapturedBytes()
	if len(got) != 20 {
		t.Fatalf("mixed block length=%d, want 20", len(got))
	}
	if int16(binary.LittleEndian.Uint16(got)) != 32767 {
		t.Fatalf("mixed block length=%d first sample=%d, want 20 and 32767", len(got), int16(binary.LittleEndian.Uint16(got)))
	}
	if err := a.Close(); err != nil {
		t.Fatal(err)
	}
	if _, err := b.Write(block(-12345)); err != nil {
		t.Fatal(err)
	}
	deadline = time.Now().Add(time.Second)
	for device.Buffered() < 40 && time.Now().Before(deadline) {
		time.Sleep(time.Millisecond)
	}
	got = device.CapturedBytes()
	if len(got) < 40 || int16(binary.LittleEndian.Uint16(got[20:])) != -12345 {
		t.Fatalf("remaining route did not play after first closed: %d bytes", len(got))
	}
	if err := b.Close(); err != nil {
		t.Fatal(err)
	}
	if _, err := device.Write(block(1)); err == nil {
		t.Fatal("last route did not close device")
	}
}
