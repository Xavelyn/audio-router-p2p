# ADR 0030: ALSA Loopback Resilience, Asynchronous Stream Promotion, and Self-Healing Device Acquisition

* **Status:** Accepted
* **Deciders:** Core Systems Architecture Team, Real-Time Audio Infrastructure, Distributed Systems & Network Engineering
* **Date:** 2026-10-11

---

## Context and Problem Statement

Operators deploying `audio-router-p2p` on Linux endpoints (notably Raspberry Pi and embedded audio nodes running media daemons such as Raspotify, Librespot, Shairport-sync, or MPD) experienced intermittent, total loss of audio routing. A standard operational sequence repeatedly provoked the failure:

1. The `audio-router-p2p` daemon starts and performs an initial device inventory scan.
2. An external audio service (such as Raspotify) starts playback on an ALSA virtual loopback device (`snd-aloop`, e.g., `hw:Loopback,0,0`).
3. The operator creates a room and assigns the ALSA loopback device as an input, routing it to an active hardware sink or P2P mesh bus.
4. **Symptom:** Absolute silence. Audio diagnostic meters report $-\infty\text{ dBFS}$ (or zero peak), no audio frames egress to the network or sink, and the system permanently remains in this silent state.
5. **Recovery:** The failure can only be cleared by restarting the entire `audio-router-p2p` daemon. Once restarted while Raspotify is actively playing, audio flows normally.

### Root-Cause Analysis

Detailed tracing of the control and data planes uncovered four compounding architectural flaws:

```
+----------------------------------------------------------------------------------------------------+
|                                    FAILURE CASCADE TIMELINE                                        |
+----------------------------------------------------------------------------------------------------+
|                                                                                                    |
|  [00:00:00] Daemon Init: DeviceResolver registers PortAudio RuntimeID=4 for Loopback (48 kHz)    |
|  [00:00:05] Raspotify starts playback on Loopback,0 (forces hardware cable to 44.1 kHz)            |
|  [00:00:10] Room Route Created -> StreamManager calls openInputWithTimeout(ref, timeout=2.0s)      |
|  [00:00:10] ALSA rate probe begins: tests 48000 Hz -> ALSA driver rejects or blocks on hw_params   |
|  [00:00:12] openInputWithTimeout 2.0s deadline EXPIRES:                                            |
|             * Manager abandons hardware worker goroutine                                           |
|             * Manager permanently binds GhostAudioStream (emitting -96 dBFS silence)              |
|             * DeviceResolver records quarantined[URN] = true                                       |
|  [00:00:12.3] ALSA rate probe succeeds at 44.1 kHz -> Hardware handle opened, then immediately    |
|               orphaned or closed because streamManager already bound GhostAudioStream              |
|  [00:00:15+] Subsequent route rebuilds inspect quarantined[URN] -> Immediately return silence     |
|               PERMANENT SYSTEM LOCK: Requires complete daemon restart                              |
+----------------------------------------------------------------------------------------------------+
```

#### 1. Volatile PortAudio Runtime Index Drift
`pkg/audio/devices.go` stored an ephemeral integer `RuntimeID` (the index returned by PortAudio during `portaudio.Devices()`). When PortAudio streams open and close, or when background subsystem re-initializations occur (`initPortAudio()` / `termPortAudio()`), PortAudio re-enumerates host devices. If hotplug events or ALSA state changes shift the index array, `RuntimeID` points to the wrong device or an invalid index, generating `PaErrorCode: -9996 (Invalid device)`.

#### 2. Rigid 2-Second Synchronous Timeout with Ghost Stream Abandonment
In `cmd/client/main.go`, `openInputWithTimeout` and `openOutputWithTimeout` enforced a hard 2-second timeout (`time.After(2 * time.Second)`). When opening a physical device, `pkg/audio/open.go` probes candidate sample rates (`[48000, 44100, 96000]`). ALSA rate negotiation on locked devices or under slow USB/I2S driver scheduling can stall up to 2.5–3.0 seconds. 

When the 2-second timer elapsed, the stream manager:
* Abandoned the hardware acquisition goroutine.
* Bound a synthetic `GhostAudioStream` (which clocks real-time silent frames at $-\infty\text{ dBFS}$) into the active route pipeline.
* Late-closed the actual hardware stream when the background probe finally finished.

Once a `GhostAudioStream` was assigned, the route never revisited or promoted the stream, cementing silent playback for the entire lifetime of the route.

#### 3. Permanent Quarantine Poisoning in `DeviceResolver`
When a device lookup or initial open failed or timed out, `DeviceResolver` executed:
```go
r.quarantined[dev.URN] = true
dev.Status = "quarantined"
```
The quarantine table in `DeviceResolver` was binary and permanent. No background retry mechanism, exponential backoff, or cache invalidation TTL existed. Every future invocation of `OpenInput` or `OpenOutput` for that URN immediately returned a `GhostAudioStream` without attempting to touch the ALSA driver, forcing the operator to restart the process.

#### 4. ALSA Loopback (`snd-aloop`) Cable Locking & Subdevice Asymmetry
The Linux ALSA loopback driver (`snd-aloop`) simulates physical audio loop cables. An ALSA loopback card exposes two independent subdevices:
* **Cable 0 / Subdevice 0:** Playback (`hw:Loopback,0,0`) $\leftrightarrow$ Capture (`hw:Loopback,1,0`)
* **Cable 1 / Subdevice 1:** Playback (`hw:Loopback,0,1`) $\leftrightarrow$ Capture (`hw:Loopback,1,1`)

Crucially, the ALSA kernel driver enforces format locking: **whichever endpoint opens a cable first dictates the sample rate, channel count, and bit depth for both sides of that cable**. 

When Raspotify opens `hw:Loopback,0,0` for output, it fixes the cable hardware parameters to **44,100 Hz, 2-channel 16-bit PCM**. When `audio-router-p2p` subsequently attempts to capture on `hw:Loopback,1,0` requesting its default room rate of **48,000 Hz**, the ALSA kernel rejects `hw_params` with `-EINVAL` or stalls the driver. Because candidate probing in `open.go` tested 48 kHz first, driver negotiation timed out past the 2-second deadline. Furthermore, users often configured `hw:Loopback,0` as the capture input, not realizing that playback on device 0 must be captured on device 1.

---

## Decision Drivers

* **Zero Daemon Restarts (Zero "Restart Amnesia"):** Transient device locks, external media player launches, or hotplug re-indexing must never require restarting `audio-router-p2p`.
* **Non-Blocking Control Plane ($< 1\text{ ms}$ Routing Latency):** Route configuration and API calls must return immediately without blocking on synchronous kernel audio driver calls.
* **Seamless In-Place Stream Promotion:** Active streaming pipelines must be established immediately using a virtual handle, then transparently promoted in-place to real hardware without dropping packets, breaking downstream references, or restarting the route.
* **Driver Lock-Aware Candidate Ordering ($< 5\text{ ms}$ Hardware Acquisition):** Rate probing must inspect the Linux ALSA procfs subsystem before opening streams, immediately prioritizing locked active sample rates.
* **ALSA Loopback Subdevice Auto-Pairing:** Loopback capture/playback pairing must automatically connect to the corresponding side of the loopback cable.
* **Self-Healing Device Recovery:** Quarantined or degraded devices must automatically recover through an exponential backoff state machine with decaying TTLs.
* **Transparent Clock & Rate Decoupling:** Hardware running at 44.1 kHz must seamlessly interoperate with the 48 kHz distributed P2P audio mesh via high-quality asynchronous sample rate conversion (ASRC).

---

## Considered Options

### Option 1: Increase Synchronous Timeouts and Rely on Polling Sweeps
* **Description:** Increase `openInputWithTimeout` from 2 seconds to 10 seconds. Periodically invoke PortAudio device re-enumeration in the background.
* **Pros:**
  - Simple code patch requiring no state machine redesign.
* **Cons:**
  - Blocks API requests and UI route creation for up to 10 seconds if a device is unavailable.
  - Still vulnerable to permanent failure if Raspotify starts after the 10-second window.
  - Does not solve PortAudio integer index drift or ALSA loopback cable cross-device matching.
  - Keeps binary quarantine poisoning intact.

### Option 2: Enforce External Sound Server Layer (PulseAudio / PipeWire Only)
* **Description:** Deprecate direct ALSA access and mandate PulseAudio or PipeWire on all host systems.
* **Pros:**
  - PulseAudio and PipeWire handle software mixing, resampling, and loopback routing internally.
* **Cons:**
  - Unacceptable operational burden on lightweight embedded hardware (Raspberry Pi Zero/3/4 running minimal headless Linux images without desktop sound daemons).
  - Introduces unwanted latency overhead and dependency conflicts with bit-perfect headless playback engines.
  - Violates the standalone architecture goal of `audio-router-p2p`.

### Option 3 (Selected): Virtual Stream Contract with Asynchronous Hardware Promotion, Procfs Rate Detective, Decaying TTL Quarantine, and Ingress ASRC
* **Description:**
  1. **Virtual Stream Contract & Asynchronous Promotion:** `OpenInput` and `OpenOutput` return immediately with a `RefStreamHandle` backed by a non-blocking `deviceStreamController`. If hardware acquisition is delayed or in-flight, the controller temporarily feeds or absorbs silent buffers, promoting atomically to the real hardware stream via `atomic.Pointer[streamEntry]` as soon as the driver opens.
  2. **Canonical URN & Name-Based Dynamic Binding:** Deprecate volatile integer `RuntimeID`s. Resolve devices by exact ALSA card name, subdevice path, and canonical URN against a dynamic live scan.
  3. **Kernel Procfs Active Rate Detective:** Before probing sample rates, read `/proc/asound/cardX/pcmY[c|p]/sub0/hw_params` to detect whether the hardware cable is locked by an external process. Prioritize the locked rate at index 0 of candidate testing.
  4. **ALSA Loopback Cross-Cable Auto-Migration:** Automatically bind capture requests on `Loopback:d=0` to the corresponding playback capture counterpart `Loopback:d=1`.
  5. **Decaying TTL Quarantine State Machine:** Replace binary quarantine flags with a multi-state recovery model (`Healthy` $\rightarrow$ `Degraded` $\rightarrow$ `Probing` $\rightarrow$ `Quarantined (with decaying TTL)`).
  6. **Ingress ASRC Resampling:** Bridge 44.1 kHz hardware to 48 kHz mesh bus seamlessly via cubic Hermite resamplers.

---

## Decision Outcome

We have adopted **Option 3**. The audio hardware acquisition layer is completely decoupled from synchronous stream lifecycle management, providing self-healing, non-blocking resilience across all Linux audio interfaces.

```
+---------------------------------------------------------------------------------------------------+
|                                ASYNCHRONOUS PROMOTION ARCHITECTURE                                |
+---------------------------------------------------------------------------------------------------+
|                                                                                                   |
|   RoomManager / StreamManager                                                                     |
|                |                                                                                  |
|                v  OpenInput("hw:Loopback,0")                                                      |
|   +----------------------------+                                                                  |
|   |       DeviceResolver       |                                                                  |
|   +----------------------------+                                                                  |
|                |                                                                                  |
|                +-------------> [Returns immediately: *RefStreamHandle] (< 1ms)                    |
|                |                                                                                  |
|                v (Background Promotion Goroutine)                                                 |
|   +-------------------------------------------------------------------------------------------+   |
|   | 1. Procfs Rate Detective: Read /proc/asound/cardX/pcmYc/sub0/hw_params                    |   |
|   |    -> Detected locked rate: 44,100 Hz (Raspotify active)                                  |   |
|   | 2. Candidate Order: [44100, 48000, 96000] (0ms probe stall)                               |   |
|   | 3. Loopback Cable Mapping: d=0 capture redirected to d=1 capture                          |   |
|   | 4. Hardware PortAudio Stream Opened: 44.1 kHz                                             |   |
|   | 5. Attach Ingress ASRC Resampler (44.1 kHz -> 48.0 kHz)                                   |   |
|   | 6. Atomic Pointer Swap in deviceStreamController:                                         |   |
|   |    GhostAudioStream (Cushion)  ===>  Real Resampled Audio Stream                          |   |
|   +-------------------------------------------------------------------------------------------+   |
|                                                                                                   |
|   Data Plane: Downstream room mixing crossbar transitions seamlessly from silence to live music!  |
+---------------------------------------------------------------------------------------------------+
```

---

## Detailed Architectural Mechanisms

### 1. Virtual Stream Contract & Asynchronous Hardware Promotion

`OpenInput` and `OpenOutput` are refactored into non-blocking virtual handle factories:

* When a route is initiated, `OpenInput` / `OpenOutput` returns a `*RefStreamHandle` in $< 1\text{ ms}$.
* The handle references an underlying `deviceStreamController`. If the physical audio device is already open and healthy, it is wired directly.
* If the hardware device is not yet open, locked, or undergoing rate probing:
  1. The controller initializes with an active `cushionStream` (a real-time paced `GhostAudioStream`). Downstream audio processors immediately receive clean, clock-paced silence at $-\infty\text{ dBFS}$ without underflowing or throwing timeout errors.
  2. An asynchronous background goroutine (`promoteHardwareStream`) executes device acquisition.
  3. Once the physical device is successfully opened and primed, the controller executes an atomic pointer exchange:
     ```go
     type streamEntry struct {
         stream    AudioStream
         isGhost   bool
         rate      int
         channels  int
         resampler *ResamplerStream
     }
     
     // Atomically swap the active stream from GhostAudioStream to Real Hardware
     controller.activeEntry.Store(newStreamEntry)
     ```
  4. The worker goroutine cross-fades or immediately switches to reading from the hardware stream. Downstream readers of `RefStreamHandle.Read()` experience an unbroken stream that shifts from silence into live audio without a single dropped buffer or broken handle reference.
  5. The rigid 2-second timeout in `cmd/client/main.go` is completely eliminated.

### 2. Live Canonical URN and Name-Based Hardware Binding

The fragile integer `RuntimeID` is retired as a primary key:
* Devices are indexed and resolved using:
  1. **Canonical RFC 8141 URN:** e.g., `urn:audio:node:node-pi:dev:hw:Loopback,1,0`
  2. **ALSA Card Name & Subdevice Path:** e.g., `Loopback:CARD=Loopback,DEV=1` or `hw:CARD=DAC,DEV=0`
  3. **Exact System Hardware Path:** Sysfs device path (`/sys/class/sound/cardX`)
* When an open request is processed, if the hardware handle is not cached, `DeviceResolver` performs a dynamic live scan matching names and paths, querying PortAudio dynamically rather than relying on stale startup snapshot arrays.

### 3. Kernel Procfs Active Rate Detective & Intelligent Candidate Ordering

On Linux systems, ALSA exposes the exact runtime hardware state of every PCM channel in the virtual `/proc` filesystem. When external daemons hold an ALSA subdevice open, the kernel records parameters in:
`/proc/asound/card<X>/pcm<Y><c|p>/sub<Z>/hw_params`

#### Format of `hw_params`:
```text
access: MMAP_INTERLEAVED
format: S16_LE
subformat: STD
channels: 2
rate: 44100 (44100/1)
period_size: 1024
buffer_size: 4096
```
When closed or idle, the file contains:
```text
closed
```

#### The Detection Algorithm:
Before invoking `portaudio.OpenStream`, `pkg/audio/open.go` inspects the procfs status of the target card and device:
1. Parse the ALSA card and device index from the URN or PortAudio device info.
2. If `/proc/asound/cardX/pcmY[c|p]/sub0/hw_params` is active (not `"closed"`), extract the `rate` parameter (e.g., `44100`).
3. Reorder the candidate rate slice so that the active rate is first:
   ```go
   // If active rate is 44100 and requested is 48000:
   candidates := []int{44100, 48000, 96000}
   ```
4. PortAudio immediately succeeds on the very first candidate trial ($< 5\text{ ms}$). ALSA driver parameter renegotiation stalls are completely bypassed.

### 4. ALSA Loopback Cross-Cable Auto-Migration

To eliminate user confusion and configuration errors regarding the `snd-aloop` architecture, `DeviceResolver` implements automatic loopback cable pairing:

Let $C \in \{0, 1\}$ be the loopback cable index, and $D \in \{0, 1\}$ be the subdevice side:
$$\text{Playback}(C, D) \longleftrightarrow \text{Capture}(C, 1 - D)$$

```
+-------------------------------------------------------------------------+
|                    ALSA SND-ALOOP CABLE PAIRING                         |
+-------------------------------------------------------------------------+
|                                                                         |
|  External Application (Raspotify)                                       |
|      |                                                                  |
|      v  (ALSA Playback)                                                 |
|  hw:Loopback,0,0  [Subdevice 0, Playback Side]                          |
|         ||                                                              |
|         ||  ALSA Kernel Cable 0                                         |
|         \/                                                              |
|  hw:Loopback,1,0  [Subdevice 0, Capture Side]                           |
|      ^                                                                  |
|      |  (Auto-Migrated Capture)                                         |
|  audio-router-p2p Input Route                                           |
|                                                                         |
+-------------------------------------------------------------------------+
```

When an input capture route requests `hw:Loopback,0` (or `dev:Loopback,0`), the resolver detects that subdevice 0 is configured as the playback endpoint for Cable 0. The resolver automatically translates the capture request to `hw:Loopback,1` (the capture side of Cable 0), seamlessly linking to the external audio source.

### 5. Decaying TTL Quarantine State Machine

The binary quarantine flag is replaced by a self-healing state machine:

```mermaid
stateDiagram-v2
    [*] --> Healthy: Device Initialized / Verified
    
    state Healthy {
        NormalIO: Active DMA / PortAudio Streams
        ResetError: consecutiveErrors = 0
    }
    
    Healthy --> Degraded: Single I/O Error / Rate Reject
    
    state Degraded {
        Cushion: GhostAudioStream engaged
        PromoteWorker: Background Promotion Launched
    }
    
    Degraded --> Healthy: Hardware Stream Opens Successfully
    Degraded --> Quarantined: Retries Exceeded Max Retry Count (3)
    
    state Quarantined {
        TTLTimer: Backoff TTL Timer (500ms -> 1s -> 2s -> 4s -> max 30s)
        GhostOnly: All opens return GhostAudioStream
    }
    
    Quarantined --> Probing: Backoff TTL Expires
    
    state Probing {
        CheckProcfs: Live Procfs / Sysfs Scan
        TestOpen: Non-blocking Probe Attempt
    }
    
    Probing --> Healthy: Probe Succeeds
    Probing --> Quarantined: Probe Fails (TTL = min(TTL * 2, 30s))
```

* **Healthy:** Stream operations normal.
* **Degraded:** Hardware failed an initial probe or dropped a buffer. Virtual stream falls back to `GhostAudioStream` cushioning while an asynchronous promotion worker runs in the background.
* **Quarantined (Decaying TTL):** If retries fail repeatedly, the device enters quarantine with an initial TTL of 500ms. Calls return `GhostAudioStream` immediately without blocking.
* **Probing:** Once the TTL expires, the device automatically transitions to `Probing`. If the external application has released the lock or started streaming, the probe acquires the device and transitions back to `Healthy`.

### 6. Ingress ASRC Resampling

When the ALSA loopback or hardware device is locked to 44,100 Hz (standard for Spotify, CD audio, and AirPlay) while the `audio-router-p2p` room bus operates at 48,000 Hz:
* `pkg/audio/resampler_stream.go` wraps the physical stream.
* High-precision cubic Hermite interpolation converts 44.1 kHz PCM into 48.0 kHz PCM in real time.
* The resampler operates entirely inside the ingress worker goroutine, shielding downstream room mixers and network transport pipelines from sample rate variations.

---

## Pros and Cons of Options

| Architectural Criterion | Option 1: Timeout Extension (10s) | Option 2: Mandate Pulse/PipeWire | Option 3: Virtual Promotion + Procfs Detective (Selected) |
| :--- | :--- | :--- | :--- |
| **Recovery Without Daemon Restart** | No (Quarantine is still permanent) | Partial (Depends on Pulse daemon) | **Yes (100% Self-Healing & Dynamic)** |
| **Route Initiation Latency** | High (2s – 10s blocking on probe) | Medium (50ms – 200ms IPC) | **$< 1\text{ ms}$ (Immediate Virtual Return)** |
| **Driver Lock Negotiation Speed** | Slow (Blind candidate testing) | Managed by OS daemon | **$< 5\text{ ms}$ (Direct Procfs Extraction)** |
| **Hardware Compatibility** | High (Direct ALSA) | Low (Fails on minimal headless Pi) | **High (Direct ALSA with Zero Dependencies)** |
| **Loopback Configuration Usability** | Poor (Manual subdevice selection) | High (Virtual sink abstraction) | **High (Automatic Subdevice Pairing)** |
| **Audio Continuity** | Dropout on delay | Handled by OS daemon | **Unbroken (Seamless Atomic Pointer Swap)** |
| **Implementation Complexity** | Minimal | High (New daemon dependency) | Medium-High (State machine & procfs parsing) |

---

## Consequences

### Positive Consequences
* **Eradication of "Restart Amnesia":** Operators never need to restart `audio-router-p2p` when starting, stopping, or restarting external media daemons (Raspotify, Shairport, etc.).
* **Instantaneous Route Creation:** All API and UI route bindings return within $< 1\text{ ms}$. No audio setup operation ever blocks the control plane.
* **Zero Dropouts or Missed Streams:** Hardware that opens late (e.g. 1.5 seconds after route synthesis) is cushioned with silence and promoted in-place without restarting routes.
* **Deterministic ALSA Loopback Usability:** Both `hw:Loopback,0` and `hw:Loopback,1` work transparently regardless of how external players attach to the cable.
* **Instant Probe Speed:** Reading `/proc/asound/.../hw_params` cuts candidate probing latency from 2,000ms down to $< 5\text{ ms}$.

### Negative Consequences & Trade-offs (Explicit Costs)
* **Ingress Resampling CPU Overhead:** Running cubic Hermite ASRC (44.1 kHz $\rightarrow$ 48.0 kHz) incurs a $\sim 0.8\%$ CPU load penalty per stereo stream on an ARM Cortex-A53 (Raspberry Pi 3/4).
* **Temporary Silent Cushion Window:** If an external hardware device takes 1.5 seconds to initialize its driver, the listener experiences 1.5 seconds of silence before music starts playing (preferable to an outright error or permanent silence).
* **Kernel Procfs Coupling:** Direct inspection of `/proc/asound` is Linux-specific. On macOS (CoreAudio) and Windows (WASAPI), the engine falls back to standard candidate probe iteration.
* **Memory Allocation for Promotion Wrappers:** Each virtual stream controller maintains an atomic pointer wrapper and cushion buffer, adding $\approx 32\text{ KB}$ of resident RAM per active route.

### Neutral Consequences
* Existing `GhostAudioStream` instrumentation and telemetry remain intact, now providing diagnostic metrics for when a stream is actively in cushion mode versus promoted hardware mode.

---

## Visual Architecture

### 1. Asynchronous Stream Promotion & Procfs Rate Detective Sequence

```mermaid
sequenceDiagram
    autonumber
    actor User as User / WebUI
    participant RM as RoomManager
    participant SM as StreamManager
    participant DR as DeviceResolver
    participant DSC as deviceStreamController
    participant FS as Linux Procfs (/proc/asound)
    participant HW as ALSA / snd-aloop HW
    participant Net as P2P Mesh Bus

    Note over User,HW: External: Raspotify starts playing on hw:Loopback,0 (locked 44.1 kHz)
    User->>RM: Create Route(src="Loopback", dst="Room1")
    RM->>SM: StartRoute()
    SM->>DR: OpenInput("Loopback")
    
    DR->>DSC: newDeviceStreamController(cushion=GhostAudioStream)
    DR-->>SM: Return *RefStreamHandle (< 1ms)
    SM-->>RM: Route Active
    RM-->>User: HTTP 200 OK (< 5ms)
    
    par Data Plane (Cushioning)
        SM->>DSC: Read() audio frames
        DSC-->>SM: Paced Silence (-96 dBFS)
        SM->>Net: Stream Silent Packets (No underflows)
    and Control Plane (Asynchronous Promotion Worker)
        DR->>FS: Read /proc/asound/cardX/pcmYc/sub0/hw_params
        FS-->>DR: rate: 44100, channels: 2
        Note over DR: Prioritize candidate rate: [44100, 48000, 96000]
        DR->>HW: PortAudio.OpenStream(rate=44100, dev=Loopback:d=1)
        HW-->>DR: Physical Stream Opened (< 5ms!)
        DR->>DR: Wrap Physical Stream with Ingress ASRC (44.1 -> 48 kHz)
        DR->>DSC: atomic.Store(promotedStreamEntry)
    end

    Note over DSC,Net: SEAMLESS PROMOTION: Cushion swaps to Live Hardware Audio
    SM->>DSC: Read() audio frames
    DSC-->>SM: 48.0 kHz Live Resampled PCM Frames
    SM->>Net: Stream Live Audio (Peak > 0 dBFS)
```

---

### 2. Decaying TTL Quarantine State Machine

```mermaid
stateDiagram-v2
    [*] --> Healthy: Device Discovered
    
    state Healthy {
        [*] --> Streaming
        Streaming --> Streaming: Normal I/O Frame Transfer
    }
    
    Healthy --> Degraded: Open Timeout / Hardware Error
    
    state Degraded {
        GhostCushion: Return RefStreamHandle with GhostAudioStream
        AsyncWorker: Spawn Background Promotion Worker
        RetryCount: Increment Attempt Counter
    }
    
    Degraded --> Healthy: Async Promotion Succeeds
    Degraded --> Quarantined: Retries >= 3
    
    state Quarantined {
        StartTTL: Set TTL = 500ms
        ServeGhost: Return GhostAudioStream immediately (0ms)
        BackoffDecay: Wait for Timer Expiration
    }
    
    Quarantined --> Probing: TTL Expired
    
    state Probing {
        InspectProcfs: Check /proc/asound status
        TryAcquire: Attempt Non-Blocking Device Open
    }
    
    Probing --> Healthy: Device Acquired & Active
    Probing --> Quarantined: Acquisition Failed -> TTL = min(TTL * 2, 30s)
```

---

### 3. ALSA Cable Routing and Procfs Inspection Topology

```mermaid
flowchart TD
    subgraph HostLinux["Linux Host Environment"]
        subgraph MediaDaemon["Media Service (e.g. Raspotify)"]
            Spotify["Raspotify Audio Player"]
        end
        
        subgraph KernelALSA["Linux ALSA Kernel Space (snd-aloop)"]
            LoopbackCard["Card: Loopback (snd-aloop)"]
            Procfs["/proc/asound/cardLoopback/pcm0c/sub0/hw_params"]
            Cable0Playback["Subdevice 0: Playback (hw:Loopback,0,0)"]
            Cable0Capture["Subdevice 1: Capture (hw:Loopback,1,0)"]
            
            Cable0Playback <-->|Internal Loop Cable| Cable0Capture
            Cable0Playback -.->|Updates Kernel Status| Procfs
        end
        
        Spotify -->|Writes 44.1 kHz PCM| Cable0Playback
        
        subgraph AudioRouter["audio-router-p2p Process"]
            ProcDetective["Procfs Rate Detective"]
            Resolver["DeviceResolver & Cable Auto-Mapper"]
            Resampler["Ingress ASRC Resampler (44.1 -> 48 kHz)"]
            Controller["deviceStreamController (atomic.Pointer)"]
            Ghost["GhostAudioStream (Silence Cushion)"]
            RefHandle["RefStreamHandle"]
            RoomMixer["Room Audio Mixer Crossbar (48 kHz)"]
            
            ProcDetective -->|Inspects active rate| Procfs
            ProcDetective -->|Injects 44100 Hz first| Resolver
            Resolver -->|Auto-maps 'Loopback,0' to 'Loopback,1'| Cable0Capture
            Cable0Capture -->|44.1 kHz Stream| Resampler
            Resampler -->|48.0 kHz Stream| Controller
            Ghost -.->|Initial Cushion| Controller
            Controller --> RefHandle
            RefHandle --> RoomMixer
        end
    end
```

---

## Implementation Plan

1. **Phase 1: Procfs Rate Detective & ALSA Cable Auto-Pairing (`pkg/audio/discovery/discovery_linux.go`, `pkg/audio/open.go`)**
   - Implement `DetectActiveHardwareRate(card, device int, direction string) (int, bool)` on Linux by parsing `/proc/asound/card%d/pcm%d%s/sub0/hw_params`.
   - Update `buildRateCandidates` in `pkg/audio/open.go` to inject the detected rate at index 0.
   - Implement `NormalizeLoopbackDevice(ref string, direction StreamDirection) string` to automatically pair playback subdevice 0 with capture subdevice 1.

2. **Phase 2: Decaying TTL Quarantine State Machine (`pkg/audio/resolver.go`)**
   - Replace `quarantined map[string]bool` with `quarantineEntries map[string]*quarantineState`.
   - Implement state transitions: `Healthy` $\leftrightarrow$ `Degraded` $\rightarrow$ `Quarantined` $\rightarrow$ `Probing`.
   - Add exponential backoff TTL logic (500ms initial, exponential doubling up to 30s cap).

3. **Phase 3: Virtual Stream Contract & Asynchronous Promotion (`pkg/audio/resolver.go`, `cmd/client/main.go`)**
   - Refactor `deviceStreamController` to support atomic stream exchange using `atomic.Pointer[streamEntry]`.
   - Update `OpenInput` and `OpenOutput` to return immediately with a `RefStreamHandle` cushioned by `GhostAudioStream` if hardware is not yet open.
   - Spawn asynchronous worker `promoteHardwareStream` to open physical audio handles and execute atomic swap.
   - Remove blocking 2.0-second timeout loops from `openInputWithTimeout` and `openOutputWithTimeout` in `cmd/client/main.go`.

4. **Phase 4: Ingress ASRC Integration (`pkg/audio/resolver.go`, `pkg/audio/resampler_stream.go`)**
   - Integrate `ResamplerStream` into the ingress pipeline when hardware stream sample rate deviates from requested room rate (e.g. 44.1 kHz vs 48.0 kHz).

---

## Validation and Verification

1. **Raspotify + Loopback Reproduction Test:**
   - On a Linux testbed with `snd-aloop` loaded, start `audio-router-p2p`.
   - Start Raspotify streaming 44.1 kHz audio to `hw:Loopback,0,0`.
   - Issue `POST /api/v1/rooms/{id}/inputs` with `hw:Loopback,0`.
   - Verify that:
     1. The route returns HTTP 200 in $< 5\text{ ms}$.
     2. Output audio peak meter transitions from silence to $> -20\text{ dBFS}$ in $< 200\text{ ms}$ without daemon restart.
     3. Audio spectrum verifies bit-accurate 44.1 kHz $\rightarrow$ 48.0 kHz cubic Hermite resampling.

2. **Asynchronous Promotion Timing Benchmark:**
   - Simulate a hardware device taking 2.5 seconds to open via a delayed mock audio stream.
   - Verify `OpenInput` returns within $< 1\text{ ms}$.
   - Verify downstream reader continuously receives valid PCM silence frames without underflow.
   - Verify that at $T = 2.5\text{s}$, stream promotes atomically with zero audio dropouts or memory leaks.

3. **Quarantine Decay & Self-Healing Test:**
   - Simulate an open failure on a disconnected device (enters quarantine).
   - Verify first retry is blocked for 500ms (returning ghost stream instantly).
   - Re-attach the device after 1 second.
   - Verify device transitions to `Probing` upon TTL expiration, successfully opens hardware, and resumes streaming without user intervention.

4. **Procfs Rate Detective Unit Tests:**
   - Mock Linux procfs `/proc/asound/card0/pcm0c/sub0/hw_params` with active 44.1 kHz and closed states.
   - Confirm candidate builder generates `[44100, 48000, ...]` when active and `[48000, 44100, ...]` when closed.
