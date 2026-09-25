package api

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	v1 "github.com/audio-router-p2p/pkg/api/v1"
	"github.com/audio-router-p2p/pkg/router"
)

func TestLegacyAndV1Coexistence(t *testing.T) {
	s := newTestServer(t, nil)
	handler := s.Handler()

	// 1. Verify legacy /status works
	reqLegacy := httptest.NewRequest(http.MethodGet, "/status", nil)
	reqLegacy.Header.Set("Authorization", "Bearer test-token")
	wLegacy := httptest.NewRecorder()
	handler.ServeHTTP(wLegacy, reqLegacy)
	if wLegacy.Code != http.StatusOK {
		t.Fatalf("expected legacy /status 200, got %d: %s", wLegacy.Code, wLegacy.Body.String())
	}

	// 2. Verify canonical /api/v1/system/status works
	reqV1 := httptest.NewRequest(http.MethodGet, "/api/v1/system/status", nil)
	reqV1.Header.Set("Authorization", "Bearer test-token")
	wV1 := httptest.NewRecorder()
	handler.ServeHTTP(wV1, reqV1)
	if wV1.Code != http.StatusOK {
		t.Fatalf("expected /api/v1/system/status 200, got %d: %s", wV1.Code, wV1.Body.String())
	}
	var nodeStatus v1.SystemStatus
	if err := json.Unmarshal(wV1.Body.Bytes(), &nodeStatus); err != nil {
		t.Fatalf("failed to decode v1 node status: %v", err)
	}
	if nodeStatus.Status != "ok" {
		t.Errorf("expected node status 'ok', got %q", nodeStatus.Status)
	}

	// 3. Post route via legacy endpoint
	legRoute := router.StreamRoute{
		ID:           "route-cross-compat",
		SourceDevice: 0,
		Destinations: []string{"192.168.1.100:4410"},
		SampleRate:   48000,
		Channels:     2,
	}
	legBody, _ := json.Marshal(legRoute)
	reqPostLeg := httptest.NewRequest(http.MethodPost, "/routes", bytes.NewReader(legBody))
	reqPostLeg.Header.Set("Authorization", "Bearer test-token")
	wPostLeg := httptest.NewRecorder()
	handler.ServeHTTP(wPostLeg, reqPostLeg)
	if wPostLeg.Code != http.StatusCreated {
		t.Fatalf("expected legacy /routes POST 201, got %d: %s", wPostLeg.Code, wPostLeg.Body.String())
	}

	// 4. Query via canonical v1 /api/v1/routes
	reqGetV1 := httptest.NewRequest(http.MethodGet, "/api/v1/routes", nil)
	reqGetV1.Header.Set("Authorization", "Bearer test-token")
	wGetV1 := httptest.NewRecorder()
	handler.ServeHTTP(wGetV1, reqGetV1)
	if wGetV1.Code != http.StatusOK {
		t.Fatalf("expected v1 /api/v1/routes 200, got %d: %s", wGetV1.Code, wGetV1.Body.String())
	}
	var matrixRoutes []router.MatrixRoute
	if err := json.Unmarshal(wGetV1.Body.Bytes(), &matrixRoutes); err != nil {
		t.Fatalf("failed to decode v1 routes: %v", err)
	}
	found := false
	for _, mr := range matrixRoutes {
		if mr.ID == "route-cross-compat" {
			found = true
			if mr.Source.DeviceID != "0" {
				t.Errorf("expected source device '0', got %s", mr.Source.DeviceID)
			}
			break
		}
	}
	if !found {
		t.Errorf("expected route-cross-compat in v1 routes list")
	}

	// 5. Query via legacy GET /routes
	reqGetLeg := httptest.NewRequest(http.MethodGet, "/routes", nil)
	reqGetLeg.Header.Set("Authorization", "Bearer test-token")
	wGetLeg := httptest.NewRecorder()
	handler.ServeHTTP(wGetLeg, reqGetLeg)
	if wGetLeg.Code != http.StatusOK {
		t.Fatalf("expected legacy /routes GET 200, got %d: %s", wGetLeg.Code, wGetLeg.Body.String())
	}
	var legRoutes []router.StreamRoute
	if err := json.Unmarshal(wGetLeg.Body.Bytes(), &legRoutes); err != nil {
		t.Fatalf("failed to decode legacy routes list: %v", err)
	}
	foundInLeg := false
	for _, r := range legRoutes {
		if r.ID == "route-cross-compat" {
			foundInLeg = true
			break
		}
	}
	if !foundInLeg {
		t.Errorf("expected route-cross-compat in legacy routes list")
	}

	// 6. Delete via legacy DELETE /routes?id=route-cross-compat
	reqDelLeg := httptest.NewRequest(http.MethodDelete, "/routes?id=route-cross-compat", nil)
	reqDelLeg.Header.Set("Authorization", "Bearer test-token")
	wDelLeg := httptest.NewRecorder()
	handler.ServeHTTP(wDelLeg, reqDelLeg)
	if wDelLeg.Code != http.StatusNoContent {
		t.Fatalf("expected legacy DELETE /routes 204, got %d: %s", wDelLeg.Code, wDelLeg.Body.String())
	}

	// Confirm deletion in v1
	reqGetV1After := httptest.NewRequest(http.MethodGet, "/api/v1/routes/route-cross-compat", nil)
	reqGetV1After.Header.Set("Authorization", "Bearer test-token")
	wGetV1After := httptest.NewRecorder()
	handler.ServeHTTP(wGetV1After, reqGetV1After)
	if wGetV1After.Code != http.StatusNotFound {
		t.Errorf("expected 404 for deleted route in v1, got %d", wGetV1After.Code)
	}
}

func TestServerHubAndV1Accessors(t *testing.T) {
	s := newTestServer(t, nil)
	if s.Hub() == nil {
		t.Fatal("expected non-nil Hub")
	}
	if s.V1() == nil {
		t.Fatal("expected non-nil V1Server")
	}
	if s.V1().Hub() != s.Hub() {
		t.Fatal("expected V1Server to share Server's Hub instance")
	}
}
