package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"io/fs"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/audio-router-p2p/pkg/api"
	v1 "github.com/audio-router-p2p/pkg/api/v1"
	"github.com/audio-router-p2p/pkg/cluster"
	"github.com/audio-router-p2p/pkg/router"
)

// ManagedSPAVerifier validates embedded static distribution files, correct MIME headers,
// immutable asset caching, and fallback index.html handling for deep client-side routes.
func ManagedSPAVerifier(t *testing.T, node *ManagedTestNode) {
	t.Helper()
	ctx := context.Background()

	// 1. Direct inspection of embedded filesystem
	distFS := api.EmbeddedDistFS()
	indexData, err := fs.ReadFile(distFS, "dist/index.html")
	if err != nil {
		t.Fatalf("Embedded dist/index.html missing or unreadable: %v", err)
	}

	indexStr := string(indexData)
	if !strings.Contains(indexStr, `<div id="app"></div>`) {
		t.Errorf("Embedded index.html does not contain root app mount element `<div id=\"app\"></div>`")
	}

	entries, err := fs.ReadDir(distFS, "dist/assets")
	if err != nil || len(entries) == 0 {
		t.Fatalf("Embedded dist/assets directory missing or empty: %v", err)
	}

	var sampleJS, sampleCSS string
	for _, entry := range entries {
		if strings.HasSuffix(entry.Name(), ".js") && sampleJS == "" {
			sampleJS = entry.Name()
		}
		if strings.HasSuffix(entry.Name(), ".css") && sampleCSS == "" {
			sampleCSS = entry.Name()
		}
	}
	if sampleJS == "" {
		t.Errorf("No javascript bundle found in dist/assets")
	}
	if sampleCSS == "" {
		t.Errorf("No stylesheet bundle found in dist/assets")
	}

	// 2. HTTP Verification against Live Node File Server
	// (a) Root path '/'
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, node.apiURL+"/", nil)
	if err != nil {
		t.Fatalf("failed to build GET / request: %v", err)
	}
	resp, err := node.client.Do(req)
	if err != nil {
		t.Fatalf("GET / failed: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		t.Errorf("GET / expected 200, got %d", resp.StatusCode)
	}
	contentType := resp.Header.Get("Content-Type")
	if !strings.Contains(contentType, "text/html") {
		t.Errorf("GET / expected text/html, got %q", contentType)
	}
	cacheControl := resp.Header.Get("Cache-Control")
	if !strings.Contains(cacheControl, "no-cache") {
		t.Errorf("GET / expected Cache-Control: no-cache, got %q", cacheControl)
	}

	bodyBytes, _ := io.ReadAll(resp.Body)
	if !strings.Contains(string(bodyBytes), `<div id="app"></div>`) {
		t.Errorf("GET / response missing <div id=\"app\"></div>")
	}

	// (b) Direct '/index.html'
	reqIndex, err := http.NewRequestWithContext(ctx, http.MethodGet, node.apiURL+"/index.html", nil)
	if err != nil {
		t.Fatalf("failed to build GET /index.html request: %v", err)
	}
	respIndex, err := node.client.Do(reqIndex)
	if err != nil {
		t.Fatalf("GET /index.html failed: %v", err)
	}
	defer respIndex.Body.Close()

	if respIndex.StatusCode != http.StatusOK {
		t.Errorf("GET /index.html expected 200, got %d", respIndex.StatusCode)
	}
	if !strings.Contains(respIndex.Header.Get("Cache-Control"), "no-cache") {
		t.Errorf("GET /index.html expected Cache-Control: no-cache, got %q", respIndex.Header.Get("Cache-Control"))
	}

	// (c) Immutable Assets (*.js and *.css)
	if sampleJS != "" {
		reqJS, err := http.NewRequestWithContext(ctx, http.MethodGet, node.apiURL+"/assets/"+sampleJS, nil)
		if err != nil {
			t.Fatalf("failed to build GET asset request: %v", err)
		}
		respJS, err := node.client.Do(reqJS)
		if err != nil {
			t.Fatalf("GET /assets/%s failed: %v", sampleJS, err)
		}
		defer respJS.Body.Close()

		if respJS.StatusCode != http.StatusOK {
			t.Errorf("GET /assets/%s expected 200, got %d", sampleJS, respJS.StatusCode)
		}
		if !strings.Contains(respJS.Header.Get("Content-Type"), "application/javascript") {
			t.Errorf("GET /assets/%s expected application/javascript, got %q", sampleJS, respJS.Header.Get("Content-Type"))
		}
		cc := respJS.Header.Get("Cache-Control")
		if !strings.Contains(cc, "immutable") || !strings.Contains(cc, "max-age=31536000") {
			t.Errorf("GET /assets/%s expected immutable cache header, got %q", sampleJS, cc)
		}
	}

	if sampleCSS != "" {
		reqCSS, err := http.NewRequestWithContext(ctx, http.MethodGet, node.apiURL+"/assets/"+sampleCSS, nil)
		if err != nil {
			t.Fatalf("failed to build GET CSS asset request: %v", err)
		}
		respCSS, err := node.client.Do(reqCSS)
		if err != nil {
			t.Fatalf("GET /assets/%s failed: %v", sampleCSS, err)
		}
		defer respCSS.Body.Close()

		if respCSS.StatusCode != http.StatusOK {
			t.Errorf("GET /assets/%s expected 200, got %d", sampleCSS, respCSS.StatusCode)
		}
		if !strings.Contains(respCSS.Header.Get("Content-Type"), "text/css") {
			t.Errorf("GET /assets/%s expected text/css, got %q", sampleCSS, respCSS.Header.Get("Content-Type"))
		}
		cc := respCSS.Header.Get("Cache-Control")
		if !strings.Contains(cc, "immutable") || !strings.Contains(cc, "max-age=31536000") {
			t.Errorf("GET /assets/%s expected immutable cache header, got %q", sampleCSS, cc)
		}
	}

	// (d) Non-existent asset must return 404 (not index.html)
	reqMissingAsset, err := http.NewRequestWithContext(ctx, http.MethodGet, node.apiURL+"/assets/non-existent-chunk.js", nil)
	if err == nil {
		respMissing, err := node.client.Do(reqMissingAsset)
		if err == nil {
			defer respMissing.Body.Close()
			if respMissing.StatusCode != http.StatusNotFound {
				t.Errorf("GET missing asset expected 404, got %d", respMissing.StatusCode)
			}
		}
	}

	// (e) Deep client-side routes fallback
	deepRoutes := []string{
		"/rooms",
		"/matrix",
		"/telemetry",
		"/devices",
		"/rooms/studio-bus",
	}

	for _, route := range deepRoutes {
		rReq, err := http.NewRequestWithContext(ctx, http.MethodGet, node.apiURL+route, nil)
		if err != nil {
			t.Errorf("failed to build request for %s: %v", route, err)
			continue
		}
		rResp, err := node.client.Do(rReq)
		if err != nil {
			t.Errorf("GET %s failed: %v", route, err)
			continue
		}
		body, _ := io.ReadAll(rResp.Body)
		rResp.Body.Close()

		if rResp.StatusCode != http.StatusOK {
			t.Errorf("Deep route %s returned status %d, expected 200", route, rResp.StatusCode)
		}
		if !strings.Contains(rResp.Header.Get("Content-Type"), "text/html") {
			t.Errorf("Deep route %s returned Content-Type %q, expected text/html", route, rResp.Header.Get("Content-Type"))
		}
		if !strings.Contains(rResp.Header.Get("Cache-Control"), "no-cache") {
			t.Errorf("Deep route %s returned Cache-Control %q, expected no-cache", route, rResp.Header.Get("Cache-Control"))
		}
		if !strings.Contains(string(body), `<div id="app"></div>`) {
			t.Errorf("Deep route %s body does not contain fallback app element", route)
		}
	}
}

// ContractVerifier performs strict JSON deserialization using DisallowUnknownFields
// against all HTTP API endpoints consumed by the WebUI control plane.
func ContractVerifier(t *testing.T, node *ManagedTestNode) {
	t.Helper()
	ctx := context.Background()
	token := node.cfg.AuthToken

	// 1. Provision initial test entities so responses contain rich payloads
	// (a) Create virtual device
	createDevPayload := `{"name": "Contract Virtual Bus", "direction": "duplex", "channels": 2, "sample_rate": 48000}`
	reqDev, err := http.NewRequestWithContext(ctx, http.MethodPost, node.apiURL+"/api/v1/devices/virtual", strings.NewReader(createDevPayload))
	if err != nil {
		t.Fatalf("failed to build POST /api/v1/devices/virtual request: %v", err)
	}
	reqDev.Header.Set("Authorization", "Bearer "+token)
	reqDev.Header.Set("Content-Type", "application/json")
	respDev, err := node.client.Do(reqDev)
	if err != nil {
		t.Fatalf("POST /api/v1/devices/virtual failed: %v", err)
	}
	defer respDev.Body.Close()
	if respDev.StatusCode != http.StatusOK && respDev.StatusCode != http.StatusCreated {
		body, _ := io.ReadAll(respDev.Body)
		t.Fatalf("POST /api/v1/devices/virtual unexpected status %d: %s", respDev.StatusCode, string(body))
	}
	_, _ = io.Copy(io.Discard, respDev.Body)

	// (b) Create room
	createRoomPayload := `{"id": "contract-test-room", "name": "Contract Test Room"}`
	reqRoom, err := http.NewRequestWithContext(ctx, http.MethodPost, node.apiURL+"/api/v1/rooms", strings.NewReader(createRoomPayload))
	if err != nil {
		t.Fatalf("failed to build POST /api/v1/rooms request: %v", err)
	}
	reqRoom.Header.Set("Authorization", "Bearer "+token)
	reqRoom.Header.Set("Content-Type", "application/json")
	respRoom, err := node.client.Do(reqRoom)
	if err != nil {
		t.Fatalf("POST /api/v1/rooms failed: %v", err)
	}
	defer respRoom.Body.Close()
	if respRoom.StatusCode != http.StatusOK && respRoom.StatusCode != http.StatusCreated {
		body, _ := io.ReadAll(respRoom.Body)
		t.Fatalf("POST /api/v1/rooms unexpected status %d: %s", respRoom.StatusCode, string(body))
	}
	_, _ = io.Copy(io.Discard, respRoom.Body)

	// Helper for executing strict unmarshaled GET requests
	strictGet := func(endpoint string, target any) {
		t.Helper()
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, node.apiURL+endpoint, nil)
		if err != nil {
			t.Fatalf("failed to build GET %s request: %v", endpoint, err)
		}
		req.Header.Set("Authorization", "Bearer "+token)
		resp, err := node.client.Do(req)
		if err != nil {
			t.Fatalf("GET %s failed: %v", endpoint, err)
		}
		defer resp.Body.Close()

		if resp.StatusCode != http.StatusOK {
			body, _ := io.ReadAll(resp.Body)
			t.Fatalf("GET %s returned status %d: %s", endpoint, resp.StatusCode, string(body))
		}

		dec := json.NewDecoder(resp.Body)
		dec.DisallowUnknownFields()
		if err := dec.Decode(target); err != nil {
			t.Fatalf("Contract violation on endpoint %s (strict unmarshaling failed): %v", endpoint, err)
		}
	}

	// 2. Strict Schema Verification across WebUI Endpoints

	// (1) /api/v1/status
	var status v1.SystemStatus
	strictGet("/api/v1/status", &status)
	if status.Status == "" || status.Version == "" {
		t.Errorf("SystemStatus missing mandatory fields: %+v", status)
	}

	// (2) /api/v1/devices
	var devices []v1.AudioDevice
	strictGet("/api/v1/devices", &devices)
	if len(devices) == 0 {
		t.Fatalf("Devices list empty on /api/v1/devices")
	}

	// (3) /api/v1/rooms
	var rooms []v1.RoomInfo
	strictGet("/api/v1/rooms", &rooms)
	if len(rooms) == 0 {
		t.Fatalf("Rooms list empty on /api/v1/rooms")
	}

	// (4) Create route and test /api/v1/routes
	var srcDevID, dstDevID string
	for _, d := range devices {
		if d.MaxInput > 0 && srcDevID == "" {
			srcDevID = d.ID
		}
		if d.MaxOutput > 0 && dstDevID == "" {
			dstDevID = d.ID
		}
	}
	if srcDevID == "" || dstDevID == "" {
		t.Fatalf("Could not find suitable source/destination devices: %+v", devices)
	}

	createRoutePayload := fmt.Sprintf(`{
		"id": "contract-test-route",
		"name": "Contract Route",
		"source": {"node_id": "local", "device_id": %q, "channels": [0, 1]},
		"destination": {"node_id": "local", "device_id": %q, "channels": [0, 1]},
		"coefficients": [1.0, 0.0, 0.0, 1.0],
		"slew_ms": 20.0
	}`, srcDevID, dstDevID)

	reqRoute, err := http.NewRequestWithContext(ctx, http.MethodPost, node.apiURL+"/api/v1/routes", strings.NewReader(createRoutePayload))
	if err != nil {
		t.Fatalf("failed to build POST /api/v1/routes request: %v", err)
	}
	reqRoute.Header.Set("Authorization", "Bearer "+token)
	reqRoute.Header.Set("Content-Type", "application/json")
	respRoute, err := node.client.Do(reqRoute)
	if err != nil {
		t.Fatalf("POST /api/v1/routes failed: %v", err)
	}
	defer respRoute.Body.Close()
	if respRoute.StatusCode != http.StatusOK && respRoute.StatusCode != http.StatusCreated {
		body, _ := io.ReadAll(respRoute.Body)
		t.Fatalf("POST /api/v1/routes unexpected status %d: %s", respRoute.StatusCode, string(body))
	}
	_, _ = io.Copy(io.Discard, respRoute.Body)

	var routes []router.MatrixRoute
	strictGet("/api/v1/routes", &routes)

	// (5) /api/v1/cluster/nodes
	var clusterNodes cluster.ClusterNodesResponse
	strictGet("/api/v1/cluster/nodes", &clusterNodes)
	if clusterNodes.TotalNodes <= 0 {
		t.Errorf("ClusterNodesResponse reported zero total nodes")
	}

	// (6) /api/v1/cluster/devices
	var clusterDevices cluster.ClusterDevicesResponse
	strictGet("/api/v1/cluster/devices", &clusterDevices)
}

// VitestNodeOrchestrator spawns an isolated npm test run targeting the live daemon
// using operating system process group isolation (Setpgid) and leak-free teardown (SIGKILL).
func VitestNodeOrchestrator(t *testing.T, node *ManagedTestNode) {
	t.Helper()

	npmPath, err := exec.LookPath("npm")
	if err != nil {
		t.Skip("npm not found in PATH; skipping vitest orchestrator integration")
		return
	}

	webDir, err := findWebDir()
	if err != nil {
		t.Fatalf("failed to locate web directory: %v", err)
	}

	// Skip if dependencies have not been installed
	if _, err := os.Stat(filepath.Join(webDir, "node_modules")); os.IsNotExist(err) {
		t.Skip("web/node_modules not found; skipping vitest orchestrator (run 'npm install' in web/)")
		return
	}

	ctx, cancel := context.WithTimeout(context.Background(), 120*time.Second)
	defer cancel()

	cmd := exec.CommandContext(ctx, npmPath, "test")
	cmd.Dir = webDir
	cmd.Env = append(os.Environ(),
		"VITE_API_URL="+node.apiURL,
		"VITE_AUTH_TOKEN="+node.cfg.AuthToken,
		"NODE_TLS_REJECT_UNAUTHORIZED=0",
	)
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}

	// Go 1.20+ process group cancellation on context timeout
	cmd.Cancel = func() error {
		if cmd.Process != nil && cmd.Process.Pid > 0 {
			return syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
		}
		return nil
	}
	cmd.WaitDelay = 5 * time.Second

	var outBuf bytes.Buffer
	cmd.Stdout = &outBuf
	cmd.Stderr = &outBuf

	var pgid int
	t.Cleanup(func() {
		if pgid > 0 {
			// Kill entire process group to eliminate orphaned vitest workers
			_ = syscall.Kill(-pgid, syscall.SIGTERM)
			time.Sleep(50 * time.Millisecond)
			_ = syscall.Kill(-pgid, syscall.SIGKILL)
		}
	})

	t.Logf("[Orchestrator] Launching npm test against live ephemeral daemon: %s ...", node.apiURL)
	if err := cmd.Start(); err != nil {
		t.Fatalf("failed to start npm test process: %v", err)
	}
	pgid = cmd.Process.Pid

	waitErr := cmd.Wait()
	output := outBuf.String()

	if waitErr != nil {
		t.Fatalf("Vitest execution failed: %v\nLogs:\n%s", waitErr, output)
	}

	if !strings.Contains(output, "passed") {
		t.Fatalf("Vitest did not report passing test suite. Full output:\n%s", output)
	}

	t.Logf("[Orchestrator] Vitest suite completed successfully against daemon.\nSummary Output:\n%s", output)
}

// findWebDir discovers the web/ root directory regardless of invocation working directory.
func findWebDir() (string, error) {
	candidates := []string{
		"../../web",
		"../web",
		"./web",
	}
	for _, c := range candidates {
		pkgJson := filepath.Join(c, "package.json")
		if _, err := os.Stat(pkgJson); err == nil {
			return filepath.Abs(c)
		}
	}

	wd, err := os.Getwd()
	if err == nil {
		dir := wd
		for i := 0; i < 5; i++ {
			pkgJson := filepath.Join(dir, "web", "package.json")
			if _, err := os.Stat(pkgJson); err == nil {
				return filepath.Abs(filepath.Join(dir, "web"))
			}
			parent := filepath.Dir(dir)
			if parent == dir {
				break
			}
			dir = parent
		}
	}
	return "", fmt.Errorf("unable to locate web/ directory containing package.json")
}

// TestE2E_WebUI_ManagedSPA runs hermetic verification of embedded static assets and SPA routes.
func TestE2E_WebUI_ManagedSPA(t *testing.T) {
	node := spawnManagedTestNode(t, "node-webui-spa")
	ManagedSPAVerifier(t, node)
}

// TestE2E_WebUI_ContractVerification runs strict schema parity verification.
func TestE2E_WebUI_ContractVerification(t *testing.T) {
	node := spawnManagedTestNode(t, "node-webui-contract")
	ContractVerifier(t, node)
}

// TestE2E_WebUI_VitestOrchestrator executes the frontend test suite against a live ephemeral daemon.
func TestE2E_WebUI_VitestOrchestrator(t *testing.T) {
	node := spawnManagedTestNode(t, "node-webui-vitest")
	VitestNodeOrchestrator(t, node)
}
