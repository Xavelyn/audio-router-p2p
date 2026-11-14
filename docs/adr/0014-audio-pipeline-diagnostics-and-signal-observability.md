# ADR 0014: Audio Pipeline Diagnostics, Level Monitoring, and Resilient Routing Observability

* **Status:** Accepted
* **Deciders:** Core Systems Architecture Team, Distributed Audio & Network Engineering
* **Date:** 2026-10-01

---

## Context and Problem Statement

In `audio-router-p2p`, real-time audio streams are routed across physical hardware interfaces, virtual audio buses, and peer-to-peer network meshes via the Room and Matrix routing engines (ADR 0001, ADR 0002, ADR 0006, ADR 0013). A fundamental operational requirement is that connecting an audio input device to a room and connecting an audio output device to the same room (intra-room routing) must reliably stream captured audio to the output and register live signal level telemetry (RMS and Peak dBFS) on UI meters.

### The Problem: Silent Failure in Intra-Room Routing & Signal Level Metering

In practice, operators observed that when connecting a verified hardware input and a verified hardware output to the same room, **no sound was emitted from the output and no signal level registered on the input meter** (signal remained at $-96.0\text{ dBFS}$ silence). Critically, the system provided zero error messages, warnings, or diagnostic logs to locate the failure.

Deep architectural analysis identified five compounding root causes across the pipeline:

1. **Silent Fallback to Ghost Audio Streams (`pkg/audio/resolver.go`):**
   When `DeviceResolver.OpenInput` or `OpenOutput` resolves a device reference (URN or legacy alias like `"hw:0"` or `"0"`), any resolution mismatch or PortAudio backend opening error causes the resolver to quarantine the device and silently return a `GhostAudioStream` with a `nil` error. Because `GhostAudioStream` generates real-time paced zeroes (silence), the downstream DSP loudness analyzer computes $-96.0\text{ dBFS}$ (digital silence floor), and the output receives pure silence. No error was returned to the caller and no diagnostic log was emitted.
2. **Local Loopback Destination Resolution Failure (`cmd/client/main.go`):**
   In `router.MatrixRouteToStreamRoute`, intra-room destinations are synthesized with `dests = []string{"127.0.0.1:45678"}`. In `streamManager.StartRoute`, local output streams are only opened if `destIsLocal(d, m.localIPs)` matches. If destination strings or node IDs do not match the collected adapter IP set, `out` remains `nil`. In `Streamer.sendLoop`, if `s.isLocalDest(peer.Addr)` is true but `peer.Output` is `nil`, audio is silently dropped on the floor without any log or error.
3. **Telemetry Tap Lifecycle Race Condition (`cmd/client/main.go`):**
   `streamManager.StartRoute` only registered audio metering taps if `m.hub` was non-nil. In daemon startup, routes were started before `apiSrv.Hub()` was instantiated and passed via `streams.SetHub(hub)`. `SetHub` did not retroactively attach taps to existing active streams. Consequently, streams started at boot or during room pre-configuration never had audio taps attached, causing `st.audioTap` to remain `nil` and UI meters to show nothing.
4. **Suppressed Route Compilation Errors (`pkg/group/room.go`):**
   `Room.recompileRoutesLocked()` synthesized routes and invoked `_ = r.streamMgr.StartRoute(sr)`. Any error returned by `StartRoute` (e.g., hardware device busy, sample rate unsupported, local output failure) was discarded with the blank identifier (`_ = ...`), leaving the room manager and API callers completely unaware of stream failures.
5. **PortAudio Error Suppression and Silent Audio Thread Termination (`pkg/audio/open.go`, `pkg/streamer/streamer.go`):**
   PortAudio C-layer initialization and stream opening suppressed all `stderr` output via `withSuppressedStderr`. In `Streamer.sendLoop`, if `input.Read()` returned an error or `io.EOF`, the goroutine terminated silently and closed `stopCh` without logging the failure cause.

---

## Decision Drivers

* **Zero-Allocation Audio Thread (GC Isolation):** Real-time audio loops run at 50 Hz (20ms frames). Instrumentation on the hot path must execute with $0\text{ B/op}$ heap allocation and zero lock contention.
* **Deterministic Failure Visibility:** The system must never silently substitute physical streams with ghost silence or drop audio without emitting explicit diagnostic logs and status indicators.
* **Throttled Level Diagnostics:** Real-time signal metrics (RMS dBFS, Peak dBFS, DTX states, clipping) must be logged periodically and upon state transitions without causing stdout lock contention or log flooding.
* **Retroactive Observability Binding:** Telemetry taps must attach dynamically to all active streams regardless of initialization order.
* **Standardized Subsystem Tagging:** All diagnostic and debug logs must use structured tags (`[ROOM:ROUTE]`, `[STREAM:MGR]`, `[AUDIO:DEV]`, `[AUDIO:IO]`, `[AUDIO:LEVEL]`, `[AUDIO:METER]`) for rapid automated log filtering.

---

## Considered Options

### Option 1: Unconditional Synchronous Logging in Audio Loops
* **Description:** Add standard `log.Printf` statements on every frame read/write and error condition in `sendLoop` and `OpenInput`.
* **Drawbacks:** At 50 Hz across multiple audio streams, emitting logs per frame causes catastrophic stdout mutex contention, thread preemption, buffer underruns (XRuns), and severe log disk bloat.

### Option 2: Out-of-Band Polling Daemon
* **Description:** Run a separate daemon process that queries ALSA/PortAudio state and HTTP APIs periodically.
* **Drawbacks:** Does not solve internal pipeline races (such as tap binding or suppressed route errors), adds operational complexity, and cannot capture transient audio thread read/write failures occurring between polling intervals.

### Option 3: End-to-End Hierarchical Subsystem Diagnostics, Zero-Allocation Level Monitoring, and Resilient Pipeline Instrumentation [CHOSEN]
* **Description:**
  1. **Subsystem Logging Standard:** Adopt standardized log prefixes (`[ROOM:ROUTE]`, `[STREAM:MGR]`, `[AUDIO:DEV]`, `[AUDIO:IO]`, `[AUDIO:LEVEL]`, `[AUDIO:METER]`).
  2. **Transparent Device Resolution:** Instrument `DeviceResolver` to explicitly log device queries, alias resolutions, driver errors, and any ghost stream substitutions.
  3. **Retroactive Tap Binding:** Update `streamManager.SetHub` to iterate all active routes and attach audio taps retroactively.
  4. **Route Error Propagation:** Ensure `Room.recompileRoutesLocked` logs compilation details and never discards `streamMgr.StartRoute` errors.
  5. **Throttled Hot-Path Diagnostics:** Implement decimation-based level logging (1 Hz heartbeat or edge transitions) in `Streamer.sendLoop`, log PortAudio XRuns with 1s rate limiting, and log audio read/write terminations.
  6. **Local Destination Fallback:** Ensure loopback addresses and local nodes always open local outputs when destination device URNs are specified.

---

## Decision Outcome

Chosen Option: **Option 3: End-to-End Hierarchical Subsystem Diagnostics, Zero-Allocation Level Monitoring, and Resilient Pipeline Instrumentation**.

---

### Architectural Specifications

#### 1. Canonical Log Prefix Taxonomy

| Subsystem Tag | Domain Scope | Monitored Events |
| :--- | :--- | :--- |
| `[ROOM:ROUTE]` | Room & Matrix Routing | Route compilation, in/out binding mutations, route start/stop, compilation errors. |
| `[STREAM:MGR]` | Stream Lifecycle Manager | Local IP discovery, route provisioning, destination resolution, retroactive tap binding. |
| `[AUDIO:DEV]` | Device Resolver & Hardware | Device lookup, URN alias matching, hardware open results, ghost stream substitution. |
| `[AUDIO:IO]` | PortAudio Driver Wrapper | Device index resolution, stream parameters, buffer overflow/underflow (XRuns). |
| `[AUDIO:LEVEL]`| Real-Time Signal Energy | Throttled RMS (dBFS), Peak (dBFS), DTX speech/silence state transitions, digital clipping. |
| `[AUDIO:METER]`| Telemetry Hub | Stream tap registration, unregistration, subscriber fanout, buffer overflow drops. |

#### 2. Retroactive Telemetry Tap Binding (`cmd/client/main.go`)

`streamManager.SetHub(hub)` must retroactively bind taps for all active streams:

```go
func (m *streamManager) SetHub(hub *apiTelemetry.TelemetryHub) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.hub = hub
	log.Printf("[STREAM:MGR] TelemetryHub attached; binding taps retroactively...")
	for id, as := range m.routes {
		if as.st != nil {
			tap := m.hub.RegisterTap(id, as.route.Channels)
			as.st.SetAudioTap(tap)
			log.Printf("[STREAM:MGR] [AUDIO:METER] retroactively attached tap to route=%s (%d channels)", id, as.route.Channels)
		}
	}
}
```

#### 3. Transparent Device Resolution & Ghost Fallback Logging (`pkg/audio/resolver.go`)

Device resolution must log all state transitions and explicitly report when physical hardware cannot be opened:

```go
// OpenInput logs hardware resolution and highlights GhostAudioStream fallbacks.
// If hardware fails, it logs [AUDIO:DEV] ERROR with the concrete driver error.
```

#### 4. Route Compilation Error Propagation (`pkg/group/room.go`)

`Room.recompileRoutesLocked()` must log route synthesis parameters and handle errors from `streamMgr.StartRoute(sr)`:

```go
// recompileRoutesLocked logs [ROOM:ROUTE] synthesis details and logs CRITICAL errors
// if streamMgr fails to start a route.
```

#### 5. Throttled Hot-Path Level Monitoring (`pkg/streamer/streamer.go`)

`Streamer.sendLoop` evaluates signal metrics on every 20ms frame, updates atomic audio taps lock-free, and emits `[AUDIO:LEVEL]` logs:
1. Every 50 frames (~1.0 second at 20ms frame duration).
2. Immediately upon DTX voice activity state transitions (e.g., silence to active audio).
3. If input reads fail or reach EOF, emitting `[AUDIO:STREAM]` error logs before terminating.

---

## Consequences

### Positive Consequences
* **Immediate Defect Localization:** Operators and engineers can instantly identify whether a silent stream is caused by hardware failure, ghost fallback, missing taps, or routing errors via standardized logs.
* **Resilient Tap Synchronization:** Telemetry taps are guaranteed to connect regardless of whether routes are created before or after HTTP API server initialization.
* **Deterministic Local Playout:** Intra-room and loopback routes properly open local output devices and log playout progress.
* **Zero Performance Degradation:** Telemetry hot paths remain lock-free and allocation-free, preserving the $\le 2.5\text{ ms}$ per-frame real-time audio budget.

### Negative Consequences & Trade-offs
* **Increased Startup Log Output:** Initial device enumeration and route compilation emit informative log entries. This trade-off is necessary for production operational transparency.
* **Log Rate-Limiting Calibration:** Diagnostic level logging is throttled to ~1 Hz to prevent stdout lock contention; microsecond-level transient spikes between logged intervals are captured via atomic peak hold in UI telemetry rather than stdout logs.
