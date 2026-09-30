package v1

import (
	"encoding/json"
	"net/http"
	"testing"
)

func TestV1DevicesCRUD(t *testing.T) {
	_, mux := newTestV1Server(t)

	// 1. Initial list devices
	w := doV1Request(mux, http.MethodGet, "/api/v1/devices?kind=all", nil, "Bearer test-v1-token")
	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", w.Code)
	}
	var devs []AudioDevice
	if err := json.Unmarshal(w.Body.Bytes(), &devs); err != nil {
		t.Fatalf("unmarshal error: %v", err)
	}

	// 2. Create virtual loopback device
	createReq := CreateVirtualDeviceRequest{
		Name:             "Virtual-DAW-Bridge",
		Direction:        "duplex",
		Channels:         2,
		SampleRate:       48000,
		BufferSizeFrames: 480,
	}
	w = doV1Request(mux, http.MethodPost, "/api/v1/devices/virtual", createReq, "Bearer test-v1-token")
	if w.Code != http.StatusCreated {
		t.Fatalf("expected 201, got %d: %s", w.Code, w.Body.String())
	}
	var created AudioDevice
	if err := json.Unmarshal(w.Body.Bytes(), &created); err != nil {
		t.Fatalf("unmarshal created dev: %v", err)
	}
	if !created.IsVirtual {
		t.Error("expected IsVirtual to be true")
	}
	if created.MaxInput != 2 || created.MaxOutput != 2 {
		t.Errorf("expected 2 in / 2 out duplex, got in=%d, out=%d", created.MaxInput, created.MaxOutput)
	}

	// 3. GET /api/v1/devices/{deviceId}
	w = doV1Request(mux, http.MethodGet, "/api/v1/devices/"+created.ID, nil, "Bearer test-v1-token")
	if w.Code != http.StatusOK {
		t.Fatalf("expected 200 for created device, got %d", w.Code)
	}

	// 4. PATCH /api/v1/devices/{deviceId}
	newVol := 0.75
	muted := true
	patchReq := UpdateDeviceRequest{
		Volume: &newVol,
		Muted:  &muted,
	}
	w = doV1Request(mux, http.MethodPatch, "/api/v1/devices/"+created.ID, patchReq, "Bearer test-v1-token")
	if w.Code != http.StatusOK {
		t.Fatalf("expected 200 for patch, got %d: %s", w.Code, w.Body.String())
	}
	var patched AudioDevice
	if err := json.Unmarshal(w.Body.Bytes(), &patched); err != nil {
		t.Fatalf("unmarshal patched dev: %v", err)
	}
	if patched.Volume != 0.75 || !patched.Muted {
		t.Errorf("volume or muted mismatch: vol=%f, muted=%v", patched.Volume, patched.Muted)
	}

	// Invalid PATCH (volume > 2.0)
	invalidVol := 2.5
	w = doV1Request(mux, http.MethodPatch, "/api/v1/devices/"+created.ID, UpdateDeviceRequest{Volume: &invalidVol}, "Bearer test-v1-token")
	if w.Code != http.StatusBadRequest {
		t.Errorf("expected 400 for out-of-bounds volume, got %d", w.Code)
	}

	// 5. Query virtual kind
	w = doV1Request(mux, http.MethodGet, "/api/v1/devices?kind=virtual", nil, "Bearer test-v1-token")
	if w.Code != http.StatusOK {
		t.Fatalf("expected 200 for kind=virtual, got %d", w.Code)
	}
	var virtList []AudioDevice
	if err := json.Unmarshal(w.Body.Bytes(), &virtList); err != nil {
		t.Fatalf("unmarshal virtList: %v", err)
	}
	if len(virtList) == 0 {
		t.Fatal("expected at least 1 virtual device")
	}

	// 6. DELETE /api/v1/devices/virtual/{deviceId}
	w = doV1Request(mux, http.MethodDelete, "/api/v1/devices/virtual/"+created.ID, nil, "Bearer test-v1-token")
	if w.Code != http.StatusNoContent {
		t.Fatalf("expected 204 for delete virtual, got %d", w.Code)
	}

	// 7. Verify device is gone
	w = doV1Request(mux, http.MethodGet, "/api/v1/devices/"+created.ID, nil, "Bearer test-v1-token")
	if w.Code != http.StatusNotFound {
		t.Errorf("expected 404 after deletion, got %d", w.Code)
	}
}

func TestV1DevicesURNAndLegacyAliases(t *testing.T) {
	_, mux := newTestV1Server(t)

	// 1. Create a virtual device
	createReq := CreateVirtualDeviceRequest{
		Name:       "Stage Monitor Bus",
		Direction:  "output",
		Channels:   2,
		SampleRate: 48000,
	}
	w := doV1Request(mux, http.MethodPost, "/api/v1/devices/virtual", createReq, "Bearer test-v1-token")
	if w.Code != http.StatusCreated {
		t.Fatalf("failed to create virtual device: %d", w.Code)
	}
	var dev AudioDevice
	if err := json.Unmarshal(w.Body.Bytes(), &dev); err != nil {
		t.Fatal(err)
	}

	// Validate URN format
	expectedURN := "urn:audio:virt:stage-monitor-bus"
	if dev.URN != expectedURN || dev.ID != expectedURN {
		t.Fatalf("expected URN %q, got ID=%q URN=%q", expectedURN, dev.ID, dev.URN)
	}
	if dev.LegacyID == "" {
		t.Fatal("expected non-empty LegacyID")
	}

	// 2. Lookup by legacy ID
	w = doV1Request(mux, http.MethodGet, "/api/v1/devices/"+dev.LegacyID, nil, "Bearer test-v1-token")
	if w.Code != http.StatusOK {
		t.Fatalf("failed to lookup by legacy ID %q: %d", dev.LegacyID, w.Code)
	}
	var devByLegacy AudioDevice
	_ = json.Unmarshal(w.Body.Bytes(), &devByLegacy)
	if devByLegacy.URN != dev.URN {
		t.Fatalf("lookup by legacy ID returned mismatched URN: %s != %s", devByLegacy.URN, dev.URN)
	}

	// 3. Mutate via legacy ID
	vol := 0.42
	patchReq := UpdateDeviceRequest{Volume: &vol}
	w = doV1Request(mux, http.MethodPatch, "/api/v1/devices/"+dev.LegacyID, patchReq, "Bearer test-v1-token")
	if w.Code != http.StatusOK {
		t.Fatalf("failed to patch via legacy ID: %d", w.Code)
	}

	// 4. Verify mutation reflected when queried by canonical URN
	w = doV1Request(mux, http.MethodGet, "/api/v1/devices/"+dev.URN, nil, "Bearer test-v1-token")
	if w.Code != http.StatusOK {
		t.Fatalf("failed to query by canonical URN: %d", w.Code)
	}
	var devByURN AudioDevice
	_ = json.Unmarshal(w.Body.Bytes(), &devByURN)
	if devByURN.Volume != 0.42 {
		t.Fatalf("expected updated volume 0.42, got %f", devByURN.Volume)
	}

	// 5. Delete using legacy ID
	w = doV1Request(mux, http.MethodDelete, "/api/v1/devices/virtual/"+dev.LegacyID, nil, "Bearer test-v1-token")
	if w.Code != http.StatusNoContent {
		t.Fatalf("failed to delete virtual device by legacy ID: %d", w.Code)
	}

	// 6. Verify gone
	w = doV1Request(mux, http.MethodGet, "/api/v1/devices/"+dev.URN, nil, "Bearer test-v1-token")
	if w.Code != http.StatusNotFound {
		t.Fatalf("expected 404 after deletion, got %d", w.Code)
	}
}

func TestV1DevicesPhysicalLegacyAliases(t *testing.T) {
	_, mux := newTestV1Server(t)

	// Fetch device list
	w := doV1Request(mux, http.MethodGet, "/api/v1/devices?kind=all", nil, "Bearer test-v1-token")
	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", w.Code)
	}
	var devs []AudioDevice
	_ = json.Unmarshal(w.Body.Bytes(), &devs)
	if len(devs) == 0 {
		t.Skip("no devices available in environment")
	}

	first := devs[0]
	if first.LegacyID != "" {
		// Lookup by legacy alias (e.g. "0")
		w = doV1Request(mux, http.MethodGet, "/api/v1/devices/"+first.LegacyID, nil, "Bearer test-v1-token")
		if w.Code != http.StatusOK {
			t.Fatalf("expected 200 querying legacy ID %q, got %d", first.LegacyID, w.Code)
		}

		// Mutate by legacy alias
		vol := 0.85
		patchReq := UpdateDeviceRequest{Volume: &vol}
		w = doV1Request(mux, http.MethodPatch, "/api/v1/devices/"+first.LegacyID, patchReq, "Bearer test-v1-token")
		if w.Code != http.StatusOK {
			t.Fatalf("expected 200 patching legacy ID %q, got %d", first.LegacyID, w.Code)
		}
	}
}
