package v1

import (
	"encoding/json"
	"net/http"
	"testing"

	"github.com/audio-router-p2p/pkg/router"
)

func TestV1MatrixRoutesCRUD(t *testing.T) {
	_, mux := newTestV1Server(t)

	// 1. Initial list routes
	w := doV1Request(mux, http.MethodGet, "/api/v1/routes", nil, "Bearer test-v1-token")
	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", w.Code)
	}
	var routes []router.MatrixRoute
	if err := json.Unmarshal(w.Body.Bytes(), &routes); err != nil {
		t.Fatalf("unmarshal error: %v", err)
	}
	if len(routes) != 0 {
		t.Errorf("expected 0 routes initially, got %d", len(routes))
	}

	// 2. Create route
	createReq := CreateRouteRequest{
		ID:   "matrix-route-1",
		Name: "Control Room Feed",
		Source: router.MatrixEndpoint{
			NodeID:   "local",
			DeviceID: "0",
			Channels: []int{0, 1},
		},
		Destination: router.MatrixEndpoint{
			NodeID:   "local",
			DeviceID: "1",
			Channels: []int{0, 1},
		},
		Coefficients: []float64{1.0, 0.0, 0.0, 1.0},
		SlewMS:       20.0,
	}

	w = doV1Request(mux, http.MethodPost, "/api/v1/routes", createReq, "Bearer test-v1-token")
	if w.Code != http.StatusCreated {
		t.Fatalf("expected 201, got %d: %s", w.Code, w.Body.String())
	}
	var created router.MatrixRoute
	if err := json.Unmarshal(w.Body.Bytes(), &created); err != nil {
		t.Fatalf("unmarshal created: %v", err)
	}
	if created.ID != "matrix-route-1" {
		t.Errorf("route ID mismatch: %s", created.ID)
	}

	// 3. Duplicate POST -> 409 Conflict
	w = doV1Request(mux, http.MethodPost, "/api/v1/routes", createReq, "Bearer test-v1-token")
	if w.Code != http.StatusConflict {
		t.Errorf("expected 409 conflict on duplicate ID, got %d", w.Code)
	}

	// 4. GET /api/v1/routes/{id}
	w = doV1Request(mux, http.MethodGet, "/api/v1/routes/matrix-route-1", nil, "Bearer test-v1-token")
	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", w.Code)
	}
	var fetched router.MatrixRoute
	if err := json.Unmarshal(w.Body.Bytes(), &fetched); err != nil {
		t.Fatalf("unmarshal fetched: %v", err)
	}
	if fetched.ID != "matrix-route-1" {
		t.Errorf("ID mismatch: %s", fetched.ID)
	}

	// 5. POST /api/v1/routes/{id}/crosspoint (Live crosspoint update with slew)
	cpReq := CrosspointGainRequest{
		OutChannel: 0,
		InChannel:  1,
		Gain:       0.7071,
		SlewMS:     20.0,
	}
	w = doV1Request(mux, http.MethodPost, "/api/v1/routes/matrix-route-1/crosspoint", cpReq, "Bearer test-v1-token")
	if w.Code != http.StatusOK {
		t.Fatalf("expected 200 for crosspoint update, got %d: %s", w.Code, w.Body.String())
	}
	var cpRoute router.MatrixRoute
	if err := json.Unmarshal(w.Body.Bytes(), &cpRoute); err != nil {
		t.Fatalf("unmarshal cpRoute: %v", err)
	}
	// Index out=0, in=1 in 2x2 matrix is 0*2 + 1 = 1
	if cpRoute.Coefficients[1] != 0.7071 {
		t.Errorf("crosspoint coeff mismatch: %f; want 0.7071", cpRoute.Coefficients[1])
	}
	if cpRoute.State != "slewing" {
		t.Errorf("expected state slewing, got %s", cpRoute.State)
	}

	// 6. PATCH /api/v1/routes/{id}
	patchName := "Main Studio Master"
	newCoeffs := []float64{0.5, 0.5, 0.5, 0.5}
	patchReq := PatchRouteRequest{
		Name:         &patchName,
		Coefficients: newCoeffs,
	}
	w = doV1Request(mux, http.MethodPatch, "/api/v1/routes/matrix-route-1", patchReq, "Bearer test-v1-token")
	if w.Code != http.StatusOK {
		t.Fatalf("expected 200 for patch, got %d: %s", w.Code, w.Body.String())
	}
	var patched router.MatrixRoute
	if err := json.Unmarshal(w.Body.Bytes(), &patched); err != nil {
		t.Fatalf("unmarshal patched: %v", err)
	}
	if patched.Name != "Main Studio Master" || patched.Coefficients[0] != 0.5 {
		t.Errorf("patch verification failed: %+v", patched)
	}

	// 7. PUT /api/v1/routes/{id} (replace)
	replaceReq := CreateRouteRequest{
		Name: "Replaced Route",
		Source: router.MatrixEndpoint{
			NodeID:   "local",
			DeviceID: "0",
			Channels: []int{0, 1},
		},
		Destination: router.MatrixEndpoint{
			NodeID:   "local",
			DeviceID: "1",
			Channels: []int{0, 1},
		},
		Coefficients: []float64{0.8, 0.0, 0.0, 0.8},
		SlewMS:       10.0,
	}
	w = doV1Request(mux, http.MethodPut, "/api/v1/routes/matrix-route-1", replaceReq, "Bearer test-v1-token")
	if w.Code != http.StatusOK {
		t.Fatalf("expected 200 for replace, got %d: %s", w.Code, w.Body.String())
	}

	// 8. DELETE /api/v1/routes/{id}
	w = doV1Request(mux, http.MethodDelete, "/api/v1/routes/matrix-route-1", nil, "Bearer test-v1-token")
	if w.Code != http.StatusNoContent {
		t.Fatalf("expected 204 for delete, got %d", w.Code)
	}

	// Verify 404 after delete
	w = doV1Request(mux, http.MethodGet, "/api/v1/routes/matrix-route-1", nil, "Bearer test-v1-token")
	if w.Code != http.StatusNotFound {
		t.Errorf("expected 404 after deletion, got %d", w.Code)
	}
}
