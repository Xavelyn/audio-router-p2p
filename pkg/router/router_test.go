package router

import (
	"os"
	"testing"
)

func TestRouterAddGetDelete(t *testing.T) {
	// use a temporary file
	f, err := os.CreateTemp("", "routes_*.json")
	if err != nil {
		t.Fatalf("tmp file: %v", err)
	}
	path := f.Name()
	f.Close()
	defer os.Remove(path)

	r, err := New(path)
	if err != nil {
		t.Fatalf("router init: %v", err)
	}

	sr := StreamRoute{ID: "s1", SourceDevice: 0, Destinations: []string{"peer1"}}
	if err := r.Add(sr); err != nil {
		t.Fatalf("add: %v", err)
	}

	got, ok := r.Get("s1")
	if !ok {
		t.Fatalf("route not found after add")
	}
	if got.SourceDevice != sr.SourceDevice || len(got.Destinations) != 1 || got.Destinations[0] != "peer1" {
		t.Fatalf("route mismatch: %+v", got)
	}

	if err := r.Delete("s1"); err != nil {
		t.Fatalf("delete: %v", err)
	}
	if _, ok := r.Get("s1"); ok {
		t.Fatalf("route still present after delete")
	}
}

func TestMatrixRouteToStreamRoute_SampleRate(t *testing.T) {
	tests := []struct {
		name         string
		inputRate    int
		expectedRate int
	}{
		{
			name:         "zero rate defaults to 48000",
			inputRate:    0,
			expectedRate: 48000,
		},
		{
			name:         "negative rate defaults to 48000",
			inputRate:    -1,
			expectedRate: 48000,
		},
		{
			name:         "explicit 44100 preserved",
			inputRate:    44100,
			expectedRate: 44100,
		},
		{
			name:         "explicit 96000 preserved",
			inputRate:    96000,
			expectedRate: 96000,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			mr := MatrixRoute{
				ID:          "mr-1",
				Source:      MatrixEndpoint{NodeID: "local", DeviceID: "hw:1"},
				Destination: MatrixEndpoint{NodeID: "local", DeviceID: "hw:2"},
				InChannels:  2,
				OutChannels: 2,
				SampleRate:  tc.inputRate,
			}

			sr := MatrixRouteToStreamRoute(mr)
			if sr.SampleRate != tc.expectedRate {
				t.Fatalf("expected SampleRate %d, got %d", tc.expectedRate, sr.SampleRate)
			}
			if sr.ID != "mr-1" {
				t.Fatalf("expected ID mr-1, got %s", sr.ID)
			}
			if sr.SourceDevice != 1 || sr.DestDevice != 2 {
				t.Fatalf("unexpected devices: src=%d, dst=%d", sr.SourceDevice, sr.DestDevice)
			}
		})
	}
}

func TestStreamRouteToMatrixRoute_SampleRate(t *testing.T) {
	tests := []struct {
		name         string
		inputRate    int
		expectedRate int
	}{
		{
			name:         "zero rate defaults to 48000",
			inputRate:    0,
			expectedRate: 48000,
		},
		{
			name:         "negative rate defaults to 48000",
			inputRate:    -1,
			expectedRate: 48000,
		},
		{
			name:         "explicit 44100 preserved",
			inputRate:    44100,
			expectedRate: 44100,
		},
		{
			name:         "explicit 96000 preserved",
			inputRate:    96000,
			expectedRate: 96000,
		},
		{
			name:         "explicit 48000 preserved",
			inputRate:    48000,
			expectedRate: 48000,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			sr := StreamRoute{
				ID:           "sr-test",
				Channels:     2,
				SampleRate:   tc.inputRate,
				SourceDevice: 0,
				DestDevice:   1,
			}

			mr := StreamRouteToMatrixRoute(sr)
			if mr.SampleRate != tc.expectedRate {
				t.Fatalf("expected MatrixRoute.SampleRate %d, got %d", tc.expectedRate, mr.SampleRate)
			}
			if mr.ID != "sr-test" {
				t.Fatalf("expected ID sr-test, got %s", mr.ID)
			}
		})
	}
}
