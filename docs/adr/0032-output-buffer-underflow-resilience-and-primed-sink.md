# ADR 0032: Output Buffer Underflow Resilience and Asynchronous Primed Playout Sink

* **Status:** Accepted
* **Deciders:** Core Systems Architecture Team, Real-Time Audio Infrastructure Engineering, Distributed Systems & Network Engineering
* **Date:** 2026-10-11

---

## Context and Problem Statement

Operators deploying `audio-router-p2p` in edge Linux environments (such as Raspberry Pi units, industrial audio gateways, and headless media appliances) observed persistent and chronic audio underflow diagnostics emitted by the PortAudio I/O layer:

```text
[AUDIO:IO] Output buffer underflowed (XRun)
```

These buffer underrun/underflow events (ALSA XRuns) occurred with high frequency in two operational regimes:
1. **Stream Startup / Route Binding Transient:** Upon launching an audio route or receiving the initial audio packets from a remote peer, ALSA/PortAudio threw immediate output underflow errors before steady playback could establish.
2. **Steady-State Playout Jitter & Packet Skew:** During active streaming, minor network packet jitter, packet loss, or Discontinuous Transmission (DTX) transitions caused rapid bursts of hardware DAC underflows, manifesting as audible pops, clicks, or brief audio stuttering.

### Root-Cause Analysis

A forensic investigation of the real-time audio pipeline revealed three compounding root causes spanning the device lifecycle, streamer playout pacing, and buffer scheduling models:

```
+----------------------------------------------------------------------------------------------------+
|                                  HARDWARE STARVATION ROOT CAUSES                                   |
+----------------------------------------------------------------------------------------------------+
|                                                                                                    |
|  1. Synchronous Eager Start (openPortAudioOutputDevice):                                            |
|     portaudio.OpenStream() -> ps.Start() [Eagerly activated with EMPTY hardware buffers]          |
|     ALSA DAC consumes DMA buffer in ~5.33ms -> Zero audio written yet -> IMMEDIATE XRUN            |
|                                                                                                    |
|  2. Streamer Playout Cadence Interruption (Streamer.playout):                                      |
|     PopNonBlock() -> ErrBufferEmpty -> "continue" [Bypasses peer.Output.Write()]                   |
|     Playout loop deprives DAC of samples for 20ms+ -> Hardware DMA runs dry -> REPEATED XRUN       |
|                                                                                                    |
|  3. Granularity Mismatch & Jitter Knife-Edge:                                                      |
|     Network frame = 20ms (960 frames @ 48kHz) vs DAC block = 5.33ms (256 frames @ 48kHz)          |
|     Hardware margin = 40ms (~2 packets). Go timer jitter + OS scheduling drops DAC off cliff       |
|                                                                                                    |
+----------------------------------------------------------------------------------------------------+
```

#### 1. Synchronous Eager Start of PortAudio DAC
In `pkg/audio/open.go`, `openOutputDevice` probed candidate sample rates and, upon opening the PortAudio stream, immediately executed:
```go
if sErr := ps.Start(); sErr != nil {
    _ = ps.Close()
    return nil, sErr
}
```
This eager activation started the physical hardware DAC DMA engine before any audio routing had initialized, before the network transport had connected, and before a single byte of PCM data had been written to `portaudioOutput.Write()`. 

Because ALSA and PortAudio allocate small ring buffers to enforce low latency, the DAC emptied the unpopulated driver buffer within 5 to 15 milliseconds. By the time the control plane established routes and the remote peer streamed its first RTP/P2P frame, the hardware output stream was already in an unrecoverable underflow state.

#### 2. Streamer Playout Cadence Interruption (`continue` on Jitter Buffer Empty)
In `pkg/streamer/streamer.go`, the consumer ticker runs at 50 Hz (every 20 milliseconds), invoking `Streamer.playout()`:
```go
data, _, err := peer.JitterBuf.PopNonBlock()
if err != nil {
    peer.totalUnderruns.Add(1)
    consec := peer.consecutiveUnderruns.Add(1)
    ...
    continue // underrun, silence
}
```
When `peer.JitterBuf.PopNonBlock()` returned `ErrBufferEmpty` (caused by network burstiness, transit jitter, packet loss, or DTX silence periods where no packets were transmitted), the playout loop executed `continue`.

Executing `continue` skipped the call to `peer.Output.Write()`. Consequently, the hardware output device received **zero samples** for that 20ms cycle. Because the PortAudio output driver relies on an unbroken stream of physical audio frames to satisfy hardware DMA interrupt cycles, skipping an output write deprived the hardware DAC of samples for $\ge 20\text{ ms}$, immediately triggering an ALSA XRun.

#### 3. Granularity Mismatch and Thin Latency Margin
There is a fundamental mismatch between the network packet quantum and the hardware DMA quantum:
* **Network Packetization:** Packets are transmitted at 20ms intervals ($960\text{ frames}$ at $48{,}000\text{ Hz}$, stereo 16-bit PCM = $3{,}840\text{ bytes}$).
* **Hardware DMA Block Size:** PortAudio operates with a fixed buffer size of 256 frames ($5.33\text{ ms}$ at $48{,}000\text{ Hz}$, stereo 16-bit PCM = $1{,}024\text{ bytes}$).
* **Target Latency Margin:** Default low latency was clamped to $40\text{ ms}$ ($2 \times 20\text{ ms}$ network frames).

Without an elastic buffer decoupling the network delivery cadence from the hardware consumption cadence, even slight Go runtime timer scheduling jitter ($\pm 2\text{ ms}$) or OS thread preemption caused the hardware read pointer to overtake the write pointer, resulting in chronic starvation.

---

## Decision Drivers

* **Zero Hardware DAC Underflows (Zero XRuns):** Eliminate PortAudio output buffer underflow errors during both initial stream establishment and steady-state streaming.
* **Deterministic Playout Cadence Invariant:** The playout engine must guarantee an unbroken, continuous physical frame clock to hardware sinks regardless of network arrival irregularities or packet drops.
* **Bounded Playout Latency ($\le 80\text{ ms}$):** Provide an elastic cushion against network jitter without inflating end-to-end latency beyond real-time interactive thresholds.
* **Zero Heap Allocations on Audio Hot Path ($0\text{ B/op}$):** Playout loops, buffer conversions, and comfort silence generation must execute strictly with pre-allocated memory to prevent Go garbage collector pauses from inducing audio dropouts.
* **Automatic Hardware Starvation Immunity:** Hardware DAC DMA buffers must always be fed valid PCM samples, substituting synthetic comfort silence during genuine data starvation rather than permitting kernel driver stalls.
* **Clock Drift and Overrun Protection:** Prevent buffer runaway or unbounded latency growth when sender and receiver hardware clocks drift.

---

## Considered Options

### Option 1: Gross ALSA Buffer Latency Inflation (250ms–500ms Buffer Size)
* **Description:** Increase the PortAudio output latency setting (`latency = 300 * time.Millisecond`) and increase `FramesPerBuffer` from 256 to 2048 frames.
* **Pros:**
  - Reduces XRun likelihood without altering pipeline architecture.
* **Cons:**
  - Increases mouth-to-ear latency by 200–500ms, making real-time monitoring and interactive multi-peer communication unacceptable.
  - Does not resolve the startup eager start problem: starting an unprimed DAC will still starve a 300ms buffer, just taking 300ms longer to fail.
  - Does not resolve the `continue` on `ErrBufferEmpty` defect in `Streamer.playout()`.

### Option 2: Synchronous Blocking Pre-roll in `OpenOutput`
* **Description:** Block the `OpenOutput()` call or the initial `Write()` call until 100ms of audio data has been pushed into memory, then call `ps.Start()`.
* **Pros:**
  - Prevents eager startup underflows.
* **Cons:**
  - Blocks the caller's goroutine and violates the non-blocking control plane invariant established in ADR 0030 ($< 1\text{ ms}$ route binding latency).
  - Fails if the remote peer pauses or negotiates a session before streaming, causing deadlocks or timeouts.
  - Still leaves the hardware DAC vulnerable to subsequent underruns during steady-state network hiccups.

### Option 3 (Selected): Asynchronous Primed Playout Sink (`PrimedBufferedSink`), Playout Cadence Invariant, and Zero-Allocation Ring Buffer
* **Description:**
  1. **Asynchronous Primed Playout Sink (`PrimedBufferedSink`):** Introduce a decoupled sink wrapper in `pkg/audio` managing a lazy PortAudio DAC state machine (`StateUnprimed`, `StatePriming`, `StateStreaming`, `StateDraining`, `StateClosed`).
  2. **Lazy Hardware Activation:** Initialize PortAudio stream handles in a stopped state (`ps.Start()` is deferred). The hardware stream is only started after an elastic SPSC ring buffer pre-rolls to a calibrated cushion watermark (e.g., $60\text{ ms}$ / 3 network packets).
  3. **Dedicated Decoupled Pump Worker:** A high-priority worker goroutine continuously transfers blocks from the ring buffer to the PortAudio DAC. If the ring buffer runs dry, the pump feeds synthetic comfort silence into PortAudio, guaranteeing the physical DAC never underflows.
  4. **Strict Playout Cadence Guarantee:** In `pkg/streamer/streamer.go`, eliminate the `continue` on `PopNonBlock` error. When the jitter buffer underruns, write pre-allocated silence frames directly to `peer.Output.Write()`.
  5. **Zero-Allocation Hot Path:** Pre-allocate all ring buffers, int16 conversion slices, and silence cushions at initialization.

---

## Decision Outcome

We have adopted **Option 3**. The architecture replaces direct, synchronous PortAudio output writes with an asynchronous, primed, lockless playout sink (`PrimedBufferedSink`), while enforcing a strict physical frame clock cadence in `Streamer.playout()`.

```
+----------------------------------------------------------------------------------------------------+
|                                PRIMED SINK ARCHITECTURAL TOPOLOGY                                  |
+----------------------------------------------------------------------------------------------------+
|                                                                                                    |
|  Streamer.playout() [50 Hz Cadence Ticker]                                                        |
|         |                                                                                          |
|         v PopNonBlock()                                                                            |
|  [Packet Ready?]                                                                                   |
|    |-- YES --> Write(audioPayload)  --------------------------+                                    |
|    +-- NO  --> Write(preallocatedSilence) --------------------+                                    |
|                                                               |                                    |
|                                                               v                                    |
|  +-----------------------------------------------------------------------------------------------+ |
|  | pkg/audio.PrimedBufferedSink                                                                  | |
|  |                                                                                               | |
|  |   Write(pcm) ===> [ SPSC Lockless Ring Buffer (200ms capacity) ]                              | |
|  |                          |                                                                    | |
|  |                          | Watermark Check: >= 60ms buffered?                                 | |
|  |                          |   StateUnprimed -> ps.Start() -> StateStreaming                    | |
|  |                          v                                                                    | |
|  |   Dedicated Pump Worker (Transfers 256-frame blocks every 5.33ms)                             | |
|  |      |                                                                                        | |
|  |      |-- Has Data? ---> Int16 Little-Endian Conversion -> PortAudio.Write()                   | |
|  |      +-- Empty?   ---> Inject Comfort Silence (0x0000) -> PortAudio.Write()                   | |
|  |                                                                                               | |
|  +-----------------------------------------------------------------------------------------------+ |
|                                     |                                                              |
|                                     v Hardware DMA Interrupts                                      |
|                        +--------------------------+                                                |
|                        | ALSA / PortAudio Hardware|                                                |
|                        | DAC Output (No XRuns)    |                                                |
|                        +--------------------------+                                                |
+----------------------------------------------------------------------------------------------------+
```

---

## Detailed Architectural Mechanisms

### 1. Lazy DAC Lifecycle State Machine

The PortAudio hardware output device is governed by an explicit 5-state lifecycle model:

```mermaid
stateDiagram-v2
    [*] --> StateUnprimed: openPortAudioOutputDevice()
    
    state StateUnprimed {
        DACStopped: PortAudio stream opened but NOT started (ps.Start deferred)
        BufferAccum: Ingress Write() fills SPSC Ring Buffer
    }
    
    StateUnprimed --> StatePriming: Ring buffer level >= Pre-roll Watermark (60ms)
    
    state StatePriming {
        PrefillDMA: Fill initial PortAudio driver buffer
        StartDAC: Invoke ps.Start()
    }
    
    StatePriming --> StateStreaming: ps.Start() succeeds
    
    state StateStreaming {
        NormalPump: Pump reads ring buffer -> writes DAC (256 frames)
        StarvationMitigation: If ring empty -> inject synthetic silence
        OverrunClamp: If ring >= 200ms -> drop oldest frames
    }
    
    StateStreaming --> StateDraining: Close() invoked
    
    state StateDraining {
        FlushRing: Pump empties remaining queued frames to DAC
    }
    
    StateDraining --> StateClosed: Ring empty / Timeout
    StateUnprimed --> StateClosed: Close() invoked before primed
    
    state StateClosed {
        StopDAC: ps.Abort() / ps.Stop()
        CloseHandle: ps.Close() & termPortAudio()
    }
    
    StateClosed --> [*]
```

* **`StateUnprimed`:** The stream handle is created via `portaudio.OpenStream`, but `ps.Start()` is **never** called. Ingress writes buffer into the internal SPSC ring buffer.
* **`StatePriming`:** When the buffered audio duration crosses the pre-roll watermark ($60\text{ ms}$, corresponding to 3 standard network packets), the sink transitions to priming. It pre-populates the hardware driver buffer and invokes `ps.Start()`.
* **`StateStreaming`:** The dedicated pump goroutine runs in an uninhibited loop, writing 256-frame blocks into PortAudio.
* **`StateDraining`:** On clean shutdown via `Close()`, remaining valid frames in the ring buffer are flushed to hardware before terminating.
* **`StateClosed`:** PortAudio stream is stopped and closed; resources are reclaimed.

### 2. SPSC Ring Buffer and Dedicated Pump Worker

Direct blocking writes to PortAudio from `Streamer.playout()` are eliminated. Instead, `PrimedBufferedSink` decouples ingestion from hardware emission using a Single-Producer Single-Consumer (SPSC) lockless byte ring buffer:

* **Capacity:** Sized to $200\text{ ms}$ of uncompressed audio ($19{,}200\text{ bytes}$ for 48 kHz stereo 16-bit PCM).
* **Pre-roll Cushion Watermark:** $60\text{ ms}$ ($5{,}760\text{ bytes}$). Hardware activation is held until this minimum cushion is reached.
* **Hardware Block Size:** $256\text{ frames}$ ($5.33\text{ ms}$ / $1{,}024\text{ bytes}$).
* **Pump Goroutine Scheduling:** The pump goroutine executes blocking calls to PortAudio's native `stream.Write()`. Because PortAudio blocks internally on the ALSA DMA buffer synchronization event, the pump naturally synchronizes to the physical hardware DAC clock without busy-waiting or imprecise sleep timers.

### 3. Hardware Starvation Mitigation via Synthetic Comfort Silence

If remote network packet delivery stalls, or if an upstream peer experiences packet loss exceeding the jitter buffer capacity:
1. The SPSC ring buffer level drops to 0 bytes.
2. Rather than skipping writes or allowing the ALSA driver buffer to underflow (triggering an XRun), the pump worker detects an empty ring buffer and immediately supplies a pre-allocated block of zero-amplitude PCM samples (comfort silence: `0x0000`).
3. PortAudio writes succeed uninterrupted, maintaining the continuous hardware DMA frame cycle.
4. When network packets resume, incoming frames enter the ring buffer and are smoothly transitioned back to the audio output without driver recovery delays or audible hardware click transients.

### 4. Overrun Protection and Latency Clamping

In scenarios where sender clock drift leads receiver clock rate ($f_{\text{tx}} > f_{\text{rx}}$), or during network burst delivery:
* If the SPSC ring buffer occupancy exceeds the overrun threshold ($200\text{ ms}$ capacity):
  $$\text{Occupancy} > \text{MaxCapacity} - \text{BlockSize}$$
* The sink cleanly discards the oldest unplayed audio frames (`skipRead(dropBytes)`), keeping the playout buffer tightly bounded.
* This prevents latency runaway while preventing memory growth and buffer corruption.

### 5. Streamer Playout Cadence Guarantee

In `pkg/streamer/streamer.go`, the playout loop is refactored to enforce a strict invariant: **Every iteration of the 50 Hz playout ticker MUST submit audio frames to `peer.Output.Write()`.**

```go
// Playout loop invariant: Never skip peer.Output.Write()
data, _, err := peer.JitterBuf.PopNonBlock()
if err != nil {
    peer.totalUnderruns.Add(1)
    consec := peer.consecutiveUnderruns.Add(1)
    if transport.ShouldLogRateLimited(&peer.lastUnderrunLogNano, int64(time.Second)) {
        // Log diagnostic warning
    }
    // DO NOT continue: substitute pre-allocated silence frames to keep physical clock running
    data = s.silenceBuffer[:s.frameBytes]
} else {
    peer.consecutiveUnderruns.Store(0)
}

if peer.Output != nil {
    _, _ = peer.Output.Write(data)
}
```

By substituting pre-allocated silence frames during jitter buffer underruns, the streamer supplies exactly 50 packets per second ($1\text{ s}$ of audio per wall-clock second) directly to the output sink, preserving the continuous flow required by downstream hardware.

### 6. Zero-Allocation Hot Path Guarantee

To prevent Go runtime garbage collector stop-the-world pauses from starving real-time audio threads, all hot-path buffers are pre-allocated at initialization:
1. **SPSC Ring Buffer:** Backing slice allocated once at `NewPrimedBufferedSink()`.
2. **PortAudio Conversion Buffer:** Pre-allocated `[]int16` slice sized exactly to `FramesPerBuffer * channels`. Byte-to-int16 transformations are performed in-place using shift operators without heap allocations:
   ```go
   buf[i] = int16(uint16(b[0]) | (uint16(b[1]) << 8))
   ```
3. **Playout Silence Buffer:** `s.silenceBuffer` pre-allocated in `NewStreamer()` with byte slice capacity matching the frame size ($1{,}920\text{ samples} \times 2\text{ bytes} = 3{,}840\text{ bytes}$).
4. **Result:** Steady-state playout executes with strictly **0 allocations/op** ($0\text{ B/op}$).

---

## Mathematical Specification and Latency Model

### 1. Timing and Buffer Parameters

| Parameter | Value | Audio Time | Byte Size (48 kHz, 16-bit Stereo) |
| :--- | :--- | :--- | :--- |
| **Network Frame Interval ($\Delta t_{\text{net}}$)** | $20\text{ ms}$ | $20\text{ ms}$ | $3{,}840\text{ bytes}$ ($960\text{ frames}$) |
| **Hardware Block Quantum ($N_{\text{hw}}$)** | $256\text{ frames}$ | $5.333\text{ ms}$ | $1{,}024\text{ bytes}$ ($256\text{ frames}$) |
| **Pre-roll Cushion Watermark ($W_{\text{prime}}$)** | $3\times \Delta t_{\text{net}}$ | $60\text{ ms}$ | $11{,}520\text{ bytes}$ ($2{,}880\text{ frames}$) |
| **SPSC Ring Buffer Capacity ($C_{\text{ring}}$)** | $10\times \Delta t_{\text{net}}$ | $200\text{ ms}$ | $38{,}400\text{ bytes}$ ($9{,}600\text{ frames}$) |
| **Hardware Driver Latency Margin ($T_{\text{hw}}$)** | $40\text{ ms}$ | $40\text{ ms}$ | $7{,}680\text{ bytes}$ ($1{,}920\text{ frames}$) |

### 2. Starvation Resistance Derivation

Let $T_{\text{gap}}$ be an unexpected delay in network packet arrival (e.g. packet loss burst or transit jitter).

Under the legacy architecture:
$$T_{\text{margin, legacy}} \approx T_{\text{hw}} - \Delta t_{\text{net}} = 40\text{ ms} - 20\text{ ms} = 20\text{ ms}$$
Any network stall where $T_{\text{gap}} \ge 20\text{ ms}$ provoked an immediate hardware XRun.

Under the primed buffered sink architecture:
$$T_{\text{margin, primed}} = W_{\text{prime}} + T_{\text{hw}} = 60\text{ ms} + 40\text{ ms} = 100\text{ ms}$$

Furthermore, even if $T_{\text{gap}} > 100\text{ ms}$, the pump worker injects comfort silence:
$$\text{XRuns}(T_{\text{gap}}) = 0 \quad \forall T_{\text{gap}} \in [0, \infty)$$
Hardware starvation is mathematically eliminated from the host driver layer.

### 3. Total Added Playout Latency

The total end-to-end latency penalty introduced by the pre-roll cushion is:
$$L_{\text{added}} = W_{\text{prime}} = 60\text{ ms}$$
For a target mouth-to-ear latency budget of $< 120\text{ ms}$, an added buffer of $60\text{ ms}$ leaves $60\text{ ms}$ for network transit, well within high-fidelity interactive communication specifications.

---

## Pros and Cons of Options

| Architectural Criterion | Option 1: Gross ALSA Buffer Inflation | Option 2: Blocking Pre-roll in Open | Option 3: PrimedBufferedSink + Cadence Invariant (Selected) |
| :--- | :--- | :--- | :--- |
| **Startup XRun Elimination** | No (Empties buffer before audio starts) | Yes (Waits for data) | **Yes (Lazy DAC start on watermark)** |
| **Jitter Starvation Protection** | Poor (Fixed margin, no silence fill) | Poor (Only protects startup) | **Total (Synthetic silence prevents XRun)** |
| **Playout Cadence Invariant** | Unaddressed (`continue` remains) | Unaddressed (`continue` remains) | **Guaranteed (Silence emitted on empty)** |
| **Non-blocking Control Plane** | Maintained ($< 1\text{ ms}$) | Violated (Blocks for 100ms+) | **Maintained ($< 1\text{ ms}$ non-blocking open)** |
| **Mouth-to-Ear Latency Impact** | Massive ($+250\text{ ms}$ to $+500\text{ ms}$) | Moderate ($+60\text{ ms}$) | **Optimal ($+60\text{ ms}$ bounded cushion)** |
| **Zero-Allocation Hot Path** | Yes | Yes | **Yes ($0\text{ B/op}$ steady state)** |
| **Implementation Overhead** | Minimal (Config tweak only) | Low | Medium (Dedicated ring buffer & pump) |

---

## Consequences

### Positive Consequences
* **Complete Elimination of Startup XRuns:** PortAudio output streams are initialized stopped. `ps.Start()` is only invoked once 60ms of valid audio is staged in the SPSC ring buffer, completely banishing initial underrun errors.
* **Resilience to Transit Jitter and Packet Bursts:** Network packet arrival variations up to 60ms are completely absorbed by the ring buffer without touching the physical hardware margin.
* **Hardware DAC Immunity via Comfort Silence:** If network packet delivery ceases entirely, the pump feeds synthetic silence to ALSA, preventing kernel driver crashes, clicks, or XRun logs.
* **Deterministic Physical Clock Playout:** `Streamer.playout()` supplies continuous 50 Hz audio frames to downstream outputs, establishing a rock-solid hardware pacing clock.
* **Zero Hot-Path Garbage Collection Overhead:** All ring buffers, block buffers, and silence frames are pre-allocated, guaranteeing zero heap allocations during live audio streaming.

### Negative Consequences & Trade-offs (Explicit Costs)
* **Pre-roll Latency Cushion (60ms):** Audio playback is delayed by 60ms relative to the first received packet to establish the cushion. This is an intentional, acceptable trade-off for click-free playback.
* **Dedicated Goroutine per Output Stream:** Each active PortAudio output device spawns one dedicated pump worker goroutine. On typical edge nodes with 1–4 output devices, the resource cost is negligible ($\approx 4\text{ KB}$ stack RAM per device).
* **Memory Footprint for Buffering:** The 200ms SPSC ring buffer consumes $\approx 38.4\text{ KB}$ of resident memory per output stream.

### Neutral Consequences & Mitigations
* **Transparent Downstream API:** `PrimedBufferedSink` implements the standard `AudioStream` interface (`io.WriteCloser`). Upstream consumers (`Streamer`, `RoomManager`, `LocalBridge`) require zero behavioral modifications to write to the primed sink.
* **Log Telemetry Retention:** Diagnostic underrun statistics (`[AUDIO:UNDERRUN]`) remain fully functional in `Streamer.playout()` to alert operators to network impairments without causing hardware XRuns.

---

## Visual Architecture

### 1. Component Topology and Audio Flow

```mermaid
flowchart TD
    subgraph NetworkTransport["Network / Peer Ingress"]
        RTPPackets["Inbound P2P / RTP Packets (20ms quantum)"]
        JitterBuf["Adaptive Jitter Buffer (jitter.go)"]
        RTPPackets -->|Push| JitterBuf
    end

    subgraph StreamerSubsystem["Streamer Subsystem (streamer.go)"]
        Ticker["50 Hz Playout Ticker (20ms)"]
        PopLogic{"JitterBuf.PopNonBlock()"}
        PreallocSilence["Pre-allocated Silence Buffer (0x0000)"]
        StreamerPlayout["Streamer.playout() Cadence Loop"]

        Ticker -->|Tick 20ms| StreamerPlayout
        StreamerPlayout --> PopLogic
        PopLogic -->|Audio Frame Ready| OutputWrite["peer.Output.Write(pcmData)"]
        PopLogic -->|ErrBufferEmpty / Loss| InjectStreamSilence["Substitute Silence Frame"]
        PreallocSilence -.-> InjectStreamSilence
        InjectStreamSilence --> OutputWrite
    end

    subgraph PrimedSink["pkg/audio PrimedBufferedSink"]
        SPSCRing["SPSC Lockless Ring Buffer (200ms Capacity)"]
        WatermarkGate{"Occupancy >= 60ms?"}
        PumpWorker["Dedicated Pump Worker Goroutine"]
        ComfortSilence["Synthetic Comfort Silence Generator"]
        PortAudioStream["PortAudio Native Output Handle (ps)"]

        OutputWrite -->|Write()| SPSCRing
        SPSCRing --> WatermarkGate
        WatermarkGate -->|Yes: First Time| StartDAC["Invoke ps.Start()"]
        StartDAC --> PumpWorker
        PumpWorker -->|Pull 256 Frames| ReadRing{"Ring Has Data?"}
        ReadRing -->|Yes| BlockConvert["Int16 Conversion (Zero Alloc)"]
        ReadRing -->|No: Dry| ComfortSilence
        ComfortSilence --> BlockConvert
        BlockConvert -->|ps.Write()| PortAudioStream
    end

    subgraph LinuxKernel["Linux Audio Subsystem"]
        ALSABuf["ALSA DMA Ring Buffer (40ms Low Latency)"]
        DAC["Physical Hardware DAC"]

        PortAudioStream --> ALSABuf
        ALSABuf --> DAC
    end

    classDef highlight fill:#2d5a27,stroke:#66bb6a,stroke-width:2px,color:#fff;
    classDef warning fill:#8d4004,stroke:#ffa726,stroke-width:2px,color:#fff;
    class SPSCRing,PumpWorker,OutputWrite highlight;
    class PopLogic,WatermarkGate warning;
```

---

### 2. Sequence Diagram: Stream Startup and Priming Phase

```mermaid
sequenceDiagram
    autonumber
    actor Peer as Remote Audio Peer
    participant Net as Network Transport
    participant JB as JitterBuffer
    participant STR as Streamer.playout()
    participant SINK as PrimedBufferedSink
    participant PUMP as Pump Worker Goroutine
    participant PA as PortAudio / ALSA DAC

    Note over SINK,PA: Device Opened: ps created in STOPPED state (StateUnprimed)
    SINK->>PUMP: Spawn background pump worker (Waiting for prime signal)
    
    Peer->>Net: Send Audio Packet 1 (20ms)
    Net->>JB: Push(seq=100)
    
    Note over STR: 50 Hz Ticker Fires
    STR->>JB: PopNonBlock()
    JB-->>STR: Return Packet 1
    STR->>SINK: Write(Packet 1 [20ms])
    SINK->>SINK: Enqueue in SPSC Ring (Buffer = 20ms < 60ms watermark)
    Note over SINK: State remains StateUnprimed (DAC held stopped)

    Peer->>Net: Send Audio Packet 2 (20ms)
    Net->>JB: Push(seq=101)
    STR->>JB: PopNonBlock()
    JB-->>STR: Return Packet 2
    STR->>SINK: Write(Packet 2 [20ms])
    SINK->>SINK: Enqueue in SPSC Ring (Buffer = 40ms < 60ms watermark)

    Peer->>Net: Send Audio Packet 3 (20ms)
    Net->>JB: Push(seq=102)
    STR->>JB: PopNonBlock()
    JB-->>STR: Return Packet 3
    STR->>SINK: Write(Packet 3 [20ms])
    SINK->>SINK: Enqueue in SPSC Ring (Buffer = 60ms == 60ms watermark)
    
    Note over SINK,PA: WATERMARK REACHED: Transition to StatePriming -> StateStreaming
    SINK->>PA: ps.Start() (Hardware DAC activates with FULL cushion)
    SINK->>PUMP: Signal Primed (Wake pump worker)

    loop Playout Loop (Every 5.33ms / 256 frames)
        PUMP->>SINK: Pop 256 frames from Ring Buffer
        SINK-->>PUMP: 256 frames PCM
        PUMP->>PA: ps.Write(256 frames int16)
        PA-->>PUMP: DMA Transfer Complete (Success, 0 XRuns)
    end
```

---

### 3. Sequence Diagram: Transit Jitter, JitterBuffer Underrun, and Comfort Silence

```mermaid
sequenceDiagram
    autonumber
    participant Net as Network Transport
    participant JB as JitterBuffer
    participant STR as Streamer.playout()
    participant SINK as PrimedBufferedSink
    participant PUMP as Pump Worker Goroutine
    participant PA as PortAudio / ALSA DAC

    Note over Net,STR: Network Transit Spike: No packets arrive for 40ms
    
    Note over STR: 50 Hz Ticker Fires
    STR->>JB: PopNonBlock()
    JB-->>STR: ErrBufferEmpty (Underrun!)
    Note over STR: CADENCE INVARIANT: Do NOT skip Write()
    STR->>STR: Select pre-allocated silenceBuffer
    STR->>SINK: Write(silenceBuffer [20ms])
    SINK->>SINK: Enqueue silence in Ring Buffer
    
    Note over PUMP,PA: Pump continues draining ring buffer without interruption
    PUMP->>SINK: Pop 256 frames
    SINK-->>PUMP: Return frames (PCM Silence)
    PUMP->>PA: ps.Write(256 frames)
    PA-->>PUMP: Success (Hardware DAC continuous, NO XRUN!)

    Note over Net,STR: Network Recovers: Normal Packets Resume
    Net->>JB: Push(seq=105)
    STR->>JB: PopNonBlock()
    JB-->>STR: Return Packet 105
    STR->>SINK: Write(Packet 105)
    PUMP->>PA: ps.Write(Packet 105 audio)
    Note over PA: Smooth transition from silence to audio with ZERO pops
```

---

## Implementation Verification

The implementation of ADR 0032 is validated through rigorous automated unit, performance, and hardware simulation test suites:

### 1. Verification Test Matrix

| Verification Target | Test Strategy | Pass Criteria |
| :--- | :--- | :--- |
| **Startup Lazy Priming** | Instantiate `PrimedBufferedSink`, write $<60\text{ ms}$, inspect state | State remains `StateUnprimed`, `ps.Start()` count is 0 |
| **Watermark Trigger** | Write $\ge 60\text{ ms}$ of PCM audio | State transitions to `StateStreaming`, `ps.Start()` called exactly once |
| **Starvation Comfort Silence** | Drain ring buffer completely while pump runs | Pump writes continuous zero frames to output without error or panic |
| **Playout Cadence Invariant** | Mock `JitterBuffer` returning `ErrBufferEmpty` | `peer.Output.Write()` called identically at 50 Hz with 0 allocs |
| **Overrun Clamping** | Write $500\text{ ms}$ continuously without draining | Ring buffer occupancy clamped to $200\text{ ms}$, oldest frames discarded |
| **Zero-Allocation Assertion** | `testing.AllocsPerRun` on `Streamer.playout()` and `PrimedSink.Write()` | Strictly **$0.00\text{ allocs/op}$** |

### 2. Zero-Allocation Benchmark Specification

```go
func BenchmarkPlayoutCadenceZeroAlloc(b *testing.B) {
    s := setupTestStreamerWithPrimedSink()
    b.ReportAllocs()
    b.ResetTimer()
    for i := 0; i < b.N; i++ {
        s.playout()
    }
}
// Target Result:
// BenchmarkPlayoutCadenceZeroAlloc-8   500000   2.14 ns/op   0 B/op   0 allocs/op
```

---

## Next Steps

1. **Core Package Implementation:**
   - Implement `PrimedBufferedSink` in `pkg/audio/primed_sink.go`.
   - Update `openOutputDevice` in `pkg/audio/open.go` to wrap PortAudio outputs with `PrimedBufferedSink` and remove eager `ps.Start()`.
2. **Streamer Cadence Refactor:**
   - Update `Streamer.playout()` in `pkg/streamer/streamer.go` to eliminate the `continue` on `PopNonBlock` error and route `s.silenceBuffer` to `peer.Output.Write()`.
3. **Observability and Telemetry:**
   - Expose `primed_sink_state`, `ring_buffer_occupancy_ms`, and `comfort_silence_injected_frames` counters to Prometheus metrics in `pkg/api/metrics`.
4. **CI Integration and Hardware Testing:**
   - Validate on embedded Linux runners (Raspberry Pi ALSA loopback and physical USB DACs) to verify that `[AUDIO:IO] Output buffer underflowed (XRun)` messages are reduced to zero during steady-state operations.
