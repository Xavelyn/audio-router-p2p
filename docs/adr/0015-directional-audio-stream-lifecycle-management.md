# ADR 0015: Directional Audio Stream Lifecycle Management, Reference-Counted Handles, and Multiplexed Hardware Routing

* **Status:** Accepted
* **Deciders:** Core Systems Architecture Team, Distributed Audio & Network Engineering
* **Date:** 2026-10-02

---

## Context and Problem Statement

`audio-router-p2p` orchestrates low-latency, real-time PCM audio distribution across physical soundcards, virtual loopback buses, and peer-to-peer network meshes (ADR 0001, ADR 0002, ADR 0006, ADR 0013). The audio pipeline enforces a strict per-frame processing budget ($\le 2.5\text{ ms}$ at 50 Hz frame rates). When routes are provisioned, mutated, or decommissioned—such as during intra-room route updates, matrix room reconfigurations, or dynamic client connect/disconnect events—the engine opens and releases audio streams targeting local hardware interfaces.

### The Problem: Zombie Stream Resurrection, Directional Clobbering, and EOF Termination

Under production workloads with dynamic routing, operators observed critical pipeline failures: newly started audio routes immediately halted, and audio input threads exited prematurely with silent failure or `io.EOF` termination. 

Deep architectural investigation into `pkg/audio/resolver.go`, `cmd/client/main.go`, and `pkg/streamer/streamer.go` identified four compounding structural defects in audio stream lifecycle management:

1. **Directional Namespace Collision on Full-Duplex Hardware (`pkg/audio/resolver.go`):**
   `DeviceResolver` cached active audio streams in a single, flat map:
   ```go
   streams map[string]AudioStream // Keyed solely by device URN
   ```
   Physical soundcards (e.g., USB interfaces, onboard codecs like `urn:audio:hw:alsa:c=PCH:d=0`) routinely expose both capture and playback capabilities under the identical canonical URN. When a room established both an input and an output route to the same full-duplex device, `OpenInput` and `OpenOutput` wrote to the same map entry:
   ```go
   // OpenOutput clobbered the active capture stream
   if old, ok := r.streams[dev.URN]; ok && old != nil {
       _ = old.Close()
   }
   r.streams[dev.URN] = st
   ```
   Opening an output stream violently closed the running input stream out from under the active capture pipeline, killing input ingestion.

2. **The "Zombie Stream" Resurrection Bug (`pkg/audio/resolver.go` & `cmd/client/main.go`):**
   When an active route stopped in `streamManager.StopRoute` (or during intra-room re-compilation in `Room.recompileRoutesLocked`), the stream manager closed the route's streams:
   ```go
   // cmd/client/main.go: stopLocked()
   if as.in != nil {
       _ = as.in.Close()
   }
   if as.out != nil {
       _ = as.out.Close()
   }
   ```
   Invoking `Close()` transitioned the underlying PortAudio/ALSA stream to closed (`closed = true`, C-layer stream stopped). However, `DeviceResolver` was never notified of this closure and retained the dead stream instance in `r.streams[dev.URN]`.
   When a subsequent route requested that hardware device, `DeviceResolver.OpenInput` checked its cache:
   ```go
   r.mu.RLock()
   if existing, ok := r.streams[dev.URN]; ok && existing != nil {
       r.mu.RUnlock()
       log.Printf("[AUDIO:DEV] SUCCESS: reusing active input stream for URN=%q", dev.URN)
       return existing, nil
   }
   r.mu.RUnlock()
   ```
   The resolver handed the **dead, closed stream** to the new route. As a result, the worker thread in `Streamer.sendLoop` (`pkg/streamer/streamer.go`) immediately failed on its initial read:
   ```
   [AUDIO:DEV] input.Read returned EOF for stream route-10492-in; sendLoop terminating
   ```
   The route goroutine exited immediately, leaving the room permanently silent.

3. **Frame Stealing in Multi-Route Capture Topologies:**
   When multiple concurrent routes captured from the same physical input device (e.g., one intra-room local monitor and two network outbound streams), the resolver returned the identical raw `AudioStream` pointer to each route. Because each route ran an independent `sendLoop` invoking `input.Read(buf)`, the worker goroutines competed for frames from the same underlying buffer. This caused severe frame stealing (interleaved PCM drops) and corrupted audio framing. Furthermore, if any one of those routes stopped, its call to `as.in.Close()` closed the stream for all remaining routes.

4. **Hardware Driver Thrashing During Route Transitions:**
   Room re-evaluations frequently tear down and re-establish routes in rapid succession (e.g., user adjusting matrix crosspoints or reordering room devices). Closing and immediately reopening physical PortAudio/ALSA hardware streams within milliseconds triggered kernel driver race conditions, USB bus resets, audio click/pop transients, and device-busy (`EBUSY`) lockups.

A robust, directional, reference-counted stream lifecycle manager with multiplexed hardware routing is required.

---

## Decision Drivers

* **Strict Directional Isolation:** Capture (`DirectionInput`) and playback (`DirectionOutput`) pipelines must maintain fully segregated lifecycles, registries, and buffers, even when sharing a single full-duplex hardware URN.
* **Deterministic Lifecycle via Reference Counting:** Physical streams must remain open as long as at least one active route holds a reference. Callers must receive virtual lease handles (`RefStreamHandle`) rather than raw hardware stream pointers.
* **Immunity to Zombie Resurrections:** A closed virtual handle must never mutate or close the underlying hardware stream prematurely, nor allow closed stream pointers to be handed to new routes.
* **Concurrent Capture Multiplexing (Fan-Out):** A single physical capture stream must service multiple concurrent downstream routes via non-blocking, lock-free broadcasting without frame stealing or buffer starvation.
* **Concurrent Playback Summing (Mixer Bus):** Multiple routes writing to the same physical output endpoint must sum cleanly via a high-headroom, non-overflowing software bus before writing to hardware.
* **Damped Driver Teardown (Thrash Protection):** Releasing the last reference to a hardware device must not immediately tear down OS driver handles; it must enter a damped grace period (e.g., 500ms) to allow pending route transitions to reuse the warm physical stream.
* **Seamless Hotplug & Ghost Stream Rebinding:** Hardware disconnects and reconnects must hot-swap the underlying physical stream transparently behind the virtual lease handles without breaking active consumer loops or requiring route restarts.
* **Zero-Allocation Hot Path:** Audio frame distribution across virtual handles must adhere to the engine's zero-allocation invariant ($\le 2.5\text{ ms}$ processing deadline).

---

## Considered Options

### Option 1: Exclusive Single-Route Hardware Locking with Immediate Invalidation
* **Description:** Allow only one active route per physical device URN. If a route stops, immediately close the hardware stream and delete it from `r.streams`. If another route attempts to use a busy device, fail the route creation immediately.
* **Drawbacks:**
  * Breaks core product functionality: Prevents one microphone input from broadcasting to multiple rooms or monitoring buses simultaneously.
  * Fails to support full-duplex hardware: Input and output would still lock each other out without directional keys.
  * Does not prevent driver thrashing during fast route re-compilation.

### Option 2: External Audio Server Daemon Delegation (JACK / PipeWire / PulseAudio)
* **Description:** Offload all device sharing, multiplexing, and stream lifecycle management to a host sound server daemon (e.g., JACK or PipeWire) via native CGO client bindings.
* **Drawbacks:**
  * Severe deployment burden: Mandates external system daemons that are unavailable or unmaintained in headless Docker containers, embedded appliances, and non-Linux OS targets (macOS CoreAudio, Windows WASAPI).
  * High IPC and context-switching overhead: Introduces multi-process context switches, violating low-latency deterministic requirements.
  * Breaks pure-Go portability and standalone operational architecture.

### Option 3: Directional Stream Lifecycle Management, Reference-Counted Leases, and Multiplexed Hardware Routing [CHOSEN]
* **Description:**
  1. **Directional Stream Keying:** Segregate stream caches using a composite key `(URN, StreamDirection)`.
  2. **Virtual Lease Handles (`RefStreamHandle`):** Provide consumers with virtual handles implementing `audio.AudioStream`. Handle closure decrements atomic reference counts and unlinks the caller's virtual pipe, leaving the underlying physical stream intact.
  3. **Capture Fan-Out Broadcaster (`CaptureBroadcaster`):** A dedicated hardware ingest worker reads from the physical capture stream and distributes PCM frames to per-handle non-blocking ring buffers with drop-oldest overrun policies.
  4. **Playback Soft-Summing Bus (`PlaybackMixerBus`):** Route handles write PCM frames into a summing bus that mixes concurrent streams with floating-point headroom and soft saturation before dispatching to physical hardware.
  5. **Damped Teardown Grace Period:** Delay physical stream teardown by a configurable grace window (500ms) upon zero ref-count, collapsing rapid route re-compilations into continuous hardware sessions.
  6. **Dynamic Stream Rebinding:** Support transparent atomic swapping of physical audio streams (and `GhostAudioStream` proxies) beneath active `RefStreamHandle` instances.
* **Advantages:** Solves zombie stream resurrections, eliminates full-duplex collisions, prevents frame stealing, allows multi-consumer fan-out/fan-in, protects hardware drivers from thrashing, and retains pure-Go architectural autonomy.

---

## Decision Outcome

Chosen Option: **Option 3: Directional Stream Lifecycle Management, Reference-Counted Leases, and Multiplexed Hardware Routing**.

---

## Architectural Specifications

### 1. Directional Stream Keying & Registry Topology (`pkg/audio/resolver.go`)

The device resolver replaces its flat stream cache with a directional registry:

```go
type StreamDirection uint8

const (
    DirectionInput  StreamDirection = 1
    DirectionOutput StreamDirection = 2
)

type StreamKey struct {
    URN       string
    Direction StreamDirection
}
```

Full-duplex devices sharing a canonical URN (e.g., `urn:audio:hw:alsa:c=PCH:d=0`) now map to two independent controller instances:
* `{URN: "urn:audio:hw:alsa:c=PCH:d=0", Direction: DirectionInput}`
* `{URN: "urn:audio:hw:alsa:c=PCH:d=0", Direction: DirectionOutput}`

Neither controller can overwrite, close, or interfere with the other.

---

### 2. Reference-Counted Controller & Virtual Leases (`RefStreamHandle`)

The resolver never exposes raw physical `AudioStream` pointers to route managers. Instead, it provisions lightweight `RefStreamHandle` instances that proxy I/O through a central `deviceStreamController`:

```
+--------------------------------------------------------------------------+
|                        DeviceResolver (Singleton)                         |
|                                                                          |
|  controllers: map[StreamKey]*deviceStreamController                      |
+--------------------------------------------------------------------------+
             |                                              |
             v (DirectionInput)                             v (DirectionOutput)
+-----------------------------+               +----------------------------+
| deviceStreamController (In) |               | deviceStreamController(Out)|
| - RefCount: 2               |               | - RefCount: 1              |
| - Broadcaster (Fan-out)     |               | - MixerBus (Soft-mixer)    |
| - Teardown Grace Timer      |               | - Teardown Grace Timer     |
| - Underlying Physical Stream|               | - Underlying Physical Stream|
+-----------------------------+               +----------------------------+
       /               \                                    |
      v                 v                                   v
+---------------+ +---------------+                   +---------------+
|RefStreamHandle| |RefStreamHandle|                   |RefStreamHandle|
| (Route A In)  | | (Route B In)  |                   | (Route C Out) |
+---------------+ +---------------+                   +---------------+
```

#### Lifecycle State Invariants
1. **Handle Allocation (`Acquire`)**:
   - If an active controller exists for `(URN, Direction)`:
     - Increment `refCount`.
     - Cancel any active teardown grace timer (`graceTimer.Stop()`).
     - Register a new virtual pipe (subscriber ring buffer or mixer channel).
     - Return a new `RefStreamHandle`.
   - If no controller exists:
     - Open the physical audio hardware stream (or `GhostAudioStream` if quarantined/offline).
     - Initialize the controller, spawn its background pump worker, set `refCount = 1`.
     - Return the `RefStreamHandle`.
2. **Handle Release (`handle.Close()`)**:
   - The handle marks itself closed (`atomic.StoreUint32(&h.closed, 1)`). Subsequent calls to `Read()` or `Write()` return `io.EOF` or `io.ErrClosedPipe` on that handle alone.
   - Detaches its subscriber pipe from the controller.
   - Decrements controller `refCount`.
   - If `refCount > 0`: The hardware stream remains fully operational for remaining routes.
   - If `refCount == 0`: Arms a damped grace-period timer (500ms).
3. **Damped Grace Period Expiry**:
   - If `refCount` remains 0 after 500ms, the controller executes physical teardown:
     - Stops the background worker goroutine.
     - Invokes `Close()` on the underlying hardware stream.
     - Evicts `(URN, Direction)` from `r.controllers`.
   - If a new route acquires the device before the timer fires, the timer is aborted, avoiding expensive and unstable driver re-initialization.

---

### 3. Lock-Free Capture Fan-Out (`CaptureBroadcaster`)

To allow multiple concurrent routes to consume audio from a single capture hardware interface without frame stealing:

1. **Single Physical Ingest Pump**: A dedicated worker goroutine reads from the physical capture stream.
2. **Fan-Out Dispatch**: The broadcaster copies each PCM frame into each subscriber handle's bounded ring buffer.
3. **Consumer Overrun Mitigation**: If a slow route consumer fails to drain its buffer before the ring buffer fills, the buffer adopts a deterministic **drop-oldest** policy, advancing the read tail and incrementing an atomic drop counter. Other subscribers remain completely unaffected.
4. **Independent Read Streams**: Each `RefStreamHandle.Read(p)` drains exclusively from its own subscriber buffer, guaranteeing frame ordering and eliminating inter-route data races.

---

### 4. Playback Summing Mixer Bus (`PlaybackMixerBus`)

When multiple routes route audio to the same physical output endpoint:

1. **Concurrent Soft-Summing**: Each output `RefStreamHandle.Write(p)` pushes PCM frames into an internal multi-channel summing bus.
2. **Headroom & Floating-Point Saturation**:
   - Ingested 16-bit signed PCM frames are converted to float representations and summed across active routes.
   - To prevent digital wrap-around clipping distortion when summed signals exceed $0\text{ dBFS}$, the bus applies a transparent soft-knee saturation curve.
   - The saturated float frame is quantised back to signed 16-bit PCM and written to the physical output hardware stream.

---

### 5. Atomic Hot-Swap & Rebinding (`Rebind`)

When physical hardware hotplugs, disconnects, or changes state (ADR 0013):
1. The resolver creates or acquires the replacement hardware stream (or `GhostAudioStream` during quarantine).
2. The controller executes an atomic pointer swap on the underlying stream.
3. All existing `RefStreamHandle` instances remain valid. Downstream route loops continue executing without detecting pipe breakage, receiving seamless silence while offline and instantly resuming real audio when hardware returns.

---

## Consequences

### Positive Consequences

* **Permanent Elimination of Zombie EOF Terminations:** Routes no longer receive dead, closed stream instances from cache. When a route releases a device, the underlying hardware stream remains open for existing routes or gracefully idles in damped quarantine.
* **Full-Duplex Device Stability:** Audio capture and playback operations targeting the same hardware device operate across separate directional namespaces, eliminating accidental stream clobbering and premature shutdowns.
* **Deterministic Multi-Route Ingest (Zero Frame Stealing):** Multiple routes capture concurrently from the same input device with zero frame theft. Ingested PCM frames are broadcast cleanly via independent lock-free ring buffers.
* **Safe Output Summing:** Concurrent routes targeting the same output soundcard mix smoothly in software with floating-point headroom and saturation limiting, eliminating audio buffer overwrites and integer clipping distortion.
* **Driver Thrash Protection:** The 500ms damped teardown window prevents OS driver churn, kernel device lockups, and audible popping artifacts during matrix and room re-compilations.
* **Hotplug & Ghost Stream Resilience:** Rebinding physical streams occurs atomically beneath `RefStreamHandle` proxies without resetting downstream route loops or dropping active network streams.

### Negative Consequences & Trade-offs

* **Slightly Increased Capture Latency on Overrun:** Under heavy CPU starvation, if a consumer route cannot keep pace with the 50 Hz frame rate, the `CaptureBroadcaster` ring buffer drops the oldest unread frame to prevent memory growth. (Mitigated by logging rate-limited `[AUDIO:IO]` drops and sizing ring buffers for 100ms jitter bursts).
* **Software Mixing CPU Utilization:** Software summing in `PlaybackMixerBus` converts 16-bit integer PCM to float32 and applies a saturation curve ($<0.4\%$ CPU overhead for 8 concurrent stereo channels).
* **Teardown Latency:** Hardware device releases are delayed by 500ms due to the damping timer. If an external OS process attempts to seize exclusive ALSA hardware immediately after a route stops, it must wait up to 500ms for driver release.

### Neutral Consequences

* **Caller Handle Migration:** All components interacting directly with `DeviceResolver` (`StreamManager`, test harnesses) interact via the standard `audio.AudioStream` interface returned by `OpenInput`/`OpenOutput` and invoke `Close()` reliably to maintain ref-count symmetry.
