# ADR 0031: Extended DTX Hangover Duration and Acoustic Silence Sensitivity Tuning

* **Status:** Accepted
* **Deciders:** Core Systems Architecture Team, Real-Time Audio Infrastructure Engineering
* **Date:** 2026-10-11

---

## Context and Problem Statement

Discontinuous Transmission (DTX) and Silence Insertion Descriptor (SID) frame signaling were initially introduced in ADR 0003 (`pkg/audio/dsp/dtx.go`, `pkg/streamer/streamer.go`, `pkg/streamer/jitter.go`) to eliminate continuous network egress during unvoiced periods. In typical conversational voice audio, speech activity occupies between 30% and 50% of an open stream, allowing DTX silence suppression to yield up to 96% network egress bandwidth savings over raw, continuous 16-bit linear PCM transmission.

Under real-world multi-room routing and conversational environments, however, the default DTX parameterization exhibited acoustic sensitivity artifacts:
1. **Aggressive Silence Gating and Turn-Taking Clipping**:
   The initial hangover window was set to 400 milliseconds (20 frames at 20 ms packetization). Natural human speech cadence features inter-syllabic and inter-phrase pauses ranging from 200 ms to 1200 ms, while natural conversational turn-taking frequently spans 1 to 3 seconds. Under a 400 ms hangover window, the DTX state machine prematurely dropped into `DTXStateSilence`, cutting off acoustic continuity between words and short conversational pauses.
2. **Reverberation Tail and Room Ambience Truncation**:
   High-quality acoustic environments and studio spaces have room reverberation decay times ($RT_{60}$) extending from 400 ms to over 1.5 seconds. Rapid gating severed reverberation tails, creating an unnatural, "chattering" gating artifact where ambient room noise and reverberation decay cut out abruptly.
3. **Robotic Gating Perception**:
   Frequent toggling between active audio transmission and synthetic comfort noise generation at the receiver jitter buffer created perceptual pumping and audible discontinuities during quiet conversational intervals.

The architecture required an adjustment to the silence detection sensitivity that prevents premature transmission cutoffs during pauses of up to 5 seconds while preserving the bandwidth conservation benefits of DTX during genuine long-term idle intervals.

---

## Decision Drivers

* **Conversational Naturalness and Speech Tail Preservation**: Prevent audio clipping during natural inter-phrase pauses, hesitation intervals, and conversational turn-taking without introducing speech tail truncation.
* **Acoustic Ambience Integrity**: Allow natural acoustic reverberation decay ($RT_{60}$) and low-level room tone to decay smoothly into ambient silence without abrupt squelch gating.
* **Deterministic Hot-Path Execution**: Preserve the $O(1)$ constant-time, zero-heap-allocation frame processing characteristics of `DTXController.ProcessFrame()`.
* **Bounded Bandwidth Overhead**: Quantify and bound the network bandwidth tail incurred during brief pauses while preserving macro-level DTX bandwidth reduction over sustained idle periods.
* **Transport and Clock Stability**: Maintain compatibility with downstream jitter buffer clock synchronization (`PacketJitterBuffer`), preventing false underrun classifications and NAT mapping timeouts via Silence Insertion Descriptor (SID) keepalives.

---

## Considered Options

### Option 1: Dynamic Acoustic Energy Floor Adjustment (Lowering dBFS Threshold to $-60\text{ dBFS}$ or $-65\text{ dBFS}$)
* **Description**: Retain the 400 ms hangover duration but lower the silence detection threshold from $-50\text{ dBFS}$ down to $-60\text{ dBFS}$ or $-65\text{ dBFS}$ to capture lower-energy room noise.
* **Drawbacks**: Does not solve conversational turn-taking or deliberate quiet pauses where speech drops to absolute silence. In quiet environments or with noise-suppressed input microphones, signal level drops below $-65\text{ dBFS}$ within 50 ms of speech termination, causing immediate hangover expiration and clipping. Furthermore, lower thresholds increase vulnerability to microphone preamp self-noise, preventing DTX from ever activating on cheap hardware.

### Option 2: Total Deactivation of Discontinuous Transmission (DTX Disabled)
* **Description**: Disable DTX across all ingress streams, permanently streaming continuous 16-bit linear PCM at 50 packets per second.
* **Drawbacks**: Imposes an unremitting network bandwidth penalty of 192 kB/s (1.536 Mbps) per stereo 48 kHz stream regardless of channel activity. In multi-peer mesh deployments and edge environments, this wastes considerable bandwidth, accelerates mobile client battery depletion, and exacerbates network queue contention on congested uplink connections.

### Option 3: Extended Deterministic Hangover Window (5.0 Seconds / 250 Frames) with Preserved Energy Threshold and Periodic SID Keepalives
* **Description**: Extend the default hangover window in `DefaultDTXConfig()` and fallback initialization in `NewDTXController()` from 400 ms to 5000 ms (5.0 seconds). Retain the verified silence energy threshold of $-50.0\text{ dBFS}$, the periodic SID frame interval of 500 ms, and the 20 ms frame duration.
* **Advantages**: Covers human conversational turn-taking, pauses, and reverberation decays up to 5 seconds seamlessly as continuous active audio. Sustains long-term bandwidth savings during genuine idle periods (e.g., unattended streams, inactive microphones, paused playback). Operates with zero heap allocations and $O(1)$ deterministic integer arithmetic.

---

## Decision Outcome

Chosen Option: **Option 3**.

The architecture establishes an extended hangover duration of 5.0 seconds (250 frames at 20 ms packetization) as the standard default configuration for discontinuous transmission across `pkg/audio/dsp/dtx.go` and `pkg/streamer/streamer.go`.

### Technical Specifications and Parameter Baseline

| Parameter | Legacy Value | New Accepted Value | Invariant Justification |
| :--- | :--- | :--- | :--- |
| `HangoverDuration` | $400\text{ ms}$ (20 frames) | **$5000\text{ ms}$ (250 frames)** | Bridges pauses up to 5 s; eliminates conversational turn clipping. |
| `SilenceThreshold` | $-50.0\text{ dBFS}$ | **$-50.0\text{ dBFS}$** | Robust separation between active speech/program audio and ambient noise floor. |
| `SIDInterval` | $500\text{ ms}$ (25 frames) | **$500\text{ ms}$ (25 frames)** | Guarantees NAT pinhole traversal and jitter buffer sequence synchronization. |
| `FrameDuration` | $20\text{ ms}$ | **$20\text{ ms}$** | Standard packetization quantum (50 packets per second). |

### Mathematical Evaluation of Bandwidth and Frame Countdown

#### 1. Frame Countdown Derivation
For an audio packetization quantum of $\Delta t = 20\text{ ms}$ and an extended hangover window of $T_{\text{hangover}} = 5000\text{ ms}$:

$$N_{\text{hangover}} = \left\lceil \frac{T_{\text{hangover}}}{\Delta t} \right\rceil = \left\lceil \frac{5000\text{ ms}}{20\text{ ms}} \right\rceil = 250\text{ frames}$$

Similarly, the periodic Silence Insertion Descriptor (SID) frame interval is:

$$N_{\text{sid}} = \left\lceil \frac{T_{\text{sid}}}{\Delta t} \right\rceil = \left\lceil \frac{500\text{ ms}}{20\text{ ms}} \right\rceil = 25\text{ frames}$$

#### 2. Frame Processing Complexity
The state machine in `pkg/audio/dsp/dtx.go` executes entirely via scalar comparison and integer decrement:
* When $\text{RMS}_{\text{dBFS}} \ge -50.0\text{ dBFS}$:
  $$\text{state} \leftarrow \text{Active},\quad C_{\text{hangover}} \leftarrow 250,\quad C_{\text{sid}} \leftarrow 0$$
* When $\text{RMS}_{\text{dBFS}} < -50.0\text{ dBFS}$ and $\text{state} \in \{\text{Active}, \text{Hangover}\}$:
  $$\text{state} \leftarrow \text{Hangover},\quad C_{\text{hangover}} \leftarrow C_{\text{hangover}} - 1$$
* Transition to `Silence` occurs when $C_{\text{hangover}} \le 0$, triggering immediate dispatch of the boundary SID descriptor frame.

Complexity remains strictly $O(1)$ time and $O(1)$ space with zero dynamic heap allocations per frame.

#### 3. Egress Bandwidth Overhead Analysis
For standard uncompressed dual-channel (stereo) 16-bit linear PCM audio sampled at $f_s = 48{,}000\text{ Hz}$:
* Samples per channel per 20 ms frame: $48{,}000 \times 0.020 = 960\text{ samples}$
* Total samples per frame (stereo): $960 \times 2 = 1{,}920\text{ samples}$
* PCM payload per frame: $1{,}920 \times 2\text{ bytes} = 3{,}840\text{ bytes}$
* Linear transmission rate at 50 packets/sec:

$$R_{\text{active}} = 3{,}840\text{ bytes} \times 50\text{ pps} = 192{,}000\text{ bytes/s} = 192.0\text{ kB/s} \quad (1.536\text{ Mbps})$$

During an unvoiced pause:
* **Under legacy 400 ms hangover**:
  $$\text{Data}_{\text{tail}} = 192\text{ kB/s} \times 0.4\text{ s} = 76.8\text{ kB} \quad (20\text{ frames})$$
* **Under extended 5000 ms hangover**:
  $$\text{Data}_{\text{tail}} = 192\text{ kB/s} \times 5.0\text{ s} = 960.0\text{ kB} \quad (250\text{ frames})$$
* **Incremental bandwidth delta per silence transition**:
  $$\Delta \text{Data} = 960.0\text{ kB} - 76.8\text{ kB} = 883.2\text{ kB}$$

Once the 5-second hangover elapses, the stream transitions to `DTXStateSilence`, dispatching only SID keepalive frames (34-byte wire header + 0-byte audio payload) at 2 packets per second:

$$R_{\text{silence}} = 34\text{ bytes} \times 2\text{ pps} = 68\text{ bytes/s} \quad (0.000544\text{ Mbps})$$

This achieves a $99.96\%$ bandwidth reduction during steady-state silence compared to active transmission.

---

## Consequences

### Positive Consequences
* **Acoustic Naturalness**: Eliminates word clipping, swallowed consonants, and artificial squelch gating during pauses of up to 5 seconds.
* **Turn-Taking Resilience**: Conversational handoffs between participants no longer trigger repetitive state transitions into comfort noise, maintaining a continuous, transparent acoustic background.
* **Reverberation Tail Preservation**: Studio acoustic spaces and long decay tails ($RT_{60}$) dissipate smoothly without mechanical truncation.
* **Deterministic Hot-Path Performance**: Preserves $O(1)$ zero-allocation frame processing characteristics without increasing CPU overhead or garbage collection pressure.
* **Maintained Idle Conservation**: Audio streams that are completely inactive or paused continue to yield near-total bandwidth suppression ($>99.9\%$) after the initial 5-second window.

### Negative Consequences
* **Increased Bandwidth Tail on Brief Inactivity**: Pauses shorter than 5 seconds are transmitted in their entirety as active linear PCM, producing an egress payload tail of up to $960\text{ kB}$ per pause event.
* **Delayed Silence State Convergence**: Sinks and diagnostic telemetry will reflect `DTXStateHangover` for 5 seconds after input audio stops before transitioning to `DTXStateSilence`.

### Neutral Consequences
* **No Wire Format Changes**: The 34-byte wire protocol packet structure established in ADR 0003 remains unchanged.
* **Jitter Buffer Compatibility**: Receiver jitter buffers (`PacketJitterBuffer`) continue to handle DTX transitions and comfort noise synthesis identically without modification.
* **Configurability**: Endpoints requiring customized thresholds or shorter hangovers retain programmatic control via `DTXConfig`.
