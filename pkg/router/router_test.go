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

func TestReceiveRoutePersists(t *testing.T) {
	path := t.TempDir() + "/routes.json"
	r, err := New(path)
	if err != nil {
		t.Fatal(err)
	}
	route := StreamRoute{ID: "remote-to-local", Direction: "receive", DestDevice: 1}
	if err := r.Add(route); err != nil {
		t.Fatal(err)
	}
	reloaded, err := New(path)
	if err != nil {
		t.Fatal(err)
	}
	got, ok := reloaded.Get(route.ID)
	if !ok || got.Direction != "receive" || got.DestDevice != 1 {
		t.Fatalf("reloaded receive route = %+v, found = %v", got, ok)
	}
}
