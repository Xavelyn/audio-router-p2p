# ADR 0033: Self-Healing Jitter Buffer Playhead Resynchronization and SID Control Plane Separation

* **Status:** Accepted
* **Deciders:** Core Systems Architecture Team, Real-Time Audio Infrastructure Engineering, Distributed Systems & Network Engineering
* **Date:** 2026-10-11

---

## Context and Problem Statement

In distributed peer-to-peer real-time audio systems operating over variable network fabrics and interacting with host audio subsystems (such as ALSA loopback devices `snd-aloop` paired with Spotify/Raspotify, or WebRTC endpoints), streams must withstand dynamic transport interruptions, discontinuous speech activity, and hardware pause/resume cycles.

In `audio-router-p2p`, operators and field diagnostics observed a catastrophic failure mode: **audio streams stalled and entered an unrecoverable, permanent mute state with 0.0 dBFS zero peak**. Once locked in this state, audio never resumed until the stream route was manually torn down and recreated.

Forensic trace analysis isolated this breakdown to three operational regimes:
1. When a remote peer transitioned into Discontinuous Transmission (DTX) silence suppression or expired its hangover period (ADR 0031).
2. When an ALSA loopback device (e.g., `snd-aloop` driven by Raspotify or PulseAudio) paused, underran, or changed track playback.
3. When a remote node rebooted, restarted its streamer pipeline, or experienced a sequence number discontinuity.

```
+-------------------------------------------------------------------------------------------------------+
|                                  PERMANENT STREAM MUTING DEFECT LOOP                                  |
+-------------------------------------------------------------------------------------------------------+
|                                                                                                       |
|  1. Sender pauses or enters DTX silence:                                                              |
|     Packet transmission stops or drops to periodic 0-byte SID keepalives.                             |
|     Sender sequence counter freezes at S_pause.                                                       |
|                                                                                                       |
|  2. Receiver Playout Loop continues running at 50 Hz (20ms ticks):                                    |
|     Streamer.playout() -> JitterBuffer.PopNonBlock() -> Frame Missing!                                |
|     Starvation branch advances playhead: jb.nextSeq++ on EVERY 20ms TICK.                             |
|     After 5 seconds of silence, jb.nextSeq has run away by +250 ticks: nextSeq = S_pause + 250.       |
|                                                                                                       |
|  3. Sender resumes playback (e.g., user hits Play or speech returns):                                 |
|     Sender sends packet with seq = S_pause + 1.                                                       |
|                                                                                                       |
|  4. JitterBuffer.PushWithFlags receives seq:                                                          |
|     distFromNext = int32(seq - nextSeq) = (S_pause + 1) - (S_pause + 250) = -249.                     |
|     Evaluation: distFromNext < 0  ===>  TREATED AS LATE PACKET!                                       |
|     Frame is dropped unconditionally: jb.latePackets++.                                               |
|                                                                                                       |
|  5. DEADLOCK: nextSeq continues advancing at 50 Hz; sender can NEVER catch up.                        |
|     The stream remains completely silent FOREVER.                                                     |
|                                                                                                       |
+-------------------------------------------------------------------------------------------------------+
```

### Root-Cause Analysis

Detailed investigation of `pkg/streamer/jitter.go`, `pkg/streamer/streamer.go`, and `pkg/audio/dsp/dtx.go` revealed three compounding defects:

#### 1. JitterBuffer Playhead Desynchronization on Starvation
In `pkg/streamer/jitter.go`, `PopNonBlock` ticks at 50 Hz (every 20 ms), servicing the hardware output driver. When the ring buffer is starved of incoming packets:
```go
if time.Now().After(expectedPlayout) {
    if !jb.dtxMode {
        jb.underruns++
        jb.atomicUnderruns.Add(1)
        if jb.plc != nil {
            ...
            jb.nextSeq++
            return concealed, jb.nextSeq - 1, nil
        }
    }
    silence := make([]byte, jb.frameSize)
    jb.nextSeq++
    return silence, jb.nextSeq - 1, nil
}
```
`nextSeq` is incremented on every starvation cycle to advance the synthetic timeline. However, when the remote sender stops transmitting (such as during a pause in Spotify or voice silence under DTX), the sender's packet sequence counter freezes.

Upon playback resumption, the sender transmits sequence $S_{\text{resume}} = S_{\text{pause}} + 1$. In `PushWithFlags`:
```go
distFromNext := int32(seq - jb.nextSeq)
if distFromNext < 0 {
    jb.latePackets++
    return 0, nil
}
```
Because the receiver playhead advanced monotonically at 50 Hz during starvation, `nextSeq` exceeded the sender's sequence number ($S_{\text{resume}} - S_{\text{starved}} \ll 0$). `PushWithFlags` classified the newly resumed packet as an old, out-of-order "late packet" and unconditionally dropped it. Because each dropped packet resulted in another starved `PopNonBlock` tick, `nextSeq` continued to advance at 50 Hz, ensuring that subsequent sender packets were rejected forever.

#### 2. Format Adapter Corruption and Rejection of 0-Byte SID Frames
In ADR 0031 and commit `4c4d6e8`, Silence Insertion Descriptor (SID) keepalive frames were optimized to a 0-byte wire payload to conserve network bandwidth ($99.96\%$ reduction during idle intervals).

In `Streamer.handlePacket`, incoming packets were handled as follows:
```go
if len(payload) == 0 && (hdr.Flags&transport.FlagSID) != 0 {
    payload = make([]byte, s.packetBytes())
}

if peer.IngressAdapter != nil {
    normalizedPayload, err := peer.IngressAdapter.Process(payload, int(hdr.SampleRate), int(hdr.Channels))
    if err != nil {
        log.Printf("streamer: ingress format adapter error: %v", err)
        return
    }
    payload = normalizedPayload
}
```
When receiving an audio stream from a 44,100 Hz sender (such as Raspotify streaming through `snd-aloop`), `hdr.SampleRate` is 44,100 Hz, while the local streamer output runs at 48,000 Hz. `s.packetBytes()` allocates a 1,920-byte buffer (960 samples @ 48 kHz mono). Passing this synthesized 1,920-byte buffer into `IngressAdapter.Process` caused the resampler to treat the 1,920 bytes as 44.1 kHz PCM audio, resampling it up to 48 kHz and producing **2,088 bytes**.

When this 2,088-byte buffer reached `JitterBuffer.PushWithFlags`:
```go
if len(data) != jb.frameSize { // frameSize is 1920
    return 0, errors.New("invalid frame size")
}
```
`PushWithFlags` rejected the SID frame immediately with `"invalid frame size"`. Consequently:
- The receiver never processed the `FlagSID` flag.
- `jb.dtxMode` was **never set to true**.
- The jitter buffer was never informed of DTX silence entry, forcing `PopNonBlock` into active underrun/concealment mode, triggering the runaway `nextSeq++` starvation lockup detailed in Root Cause 1.

#### 3. Inability to Recover from Sequence Number Discontinuities and Node Restarts
If a remote sender rebooted, restarted its process, or experienced a major network partition resulting in sequence jumps ($|\text{seq} - \text{nextSeq}| \ge \text{capacity}$), `PushWithFlags` had no self-healing re-anchoring mechanism. When sequence numbers wrapped backwards or jumped across the ring buffer capacity boundary, packets were either dropped as late or triggered unrecoverable overruns, corrupting downstream DSP state.

---

## Decision Drivers

* **Deterministic Stream Recovery (< 20 ms):** Audio streams must instantly and automatically recover from arbitrary sender pauses, underruns, ALSA loopback halts, and network stalls without human operator intervention or route recreation.
* **Separation of Control Plane Signaling from Ingress DSP:** Control plane signaling frames (such as 0-byte SID keepalives) must never traverse media ingress format adapters (resamplers, channel mixers, FEC decoders) configured for audio payload processing.
* **Zero Late-Packet False Positives on Resumption:** Differentiate between legitimate network reordering of packets during active streaming versus sender resumption after starvation or sequence jumps.
* **Acoustic and DSP Hygiene:** When re-anchoring the jitter buffer playhead, flush stale ring slots and reset stateful audio DSP components (Packet Loss Concealment PSOLA waveform history and Clock Drift Controller PI filters) to prevent audible clicks, pops, or clock skew artifacts.
* **Preservation of Network Reordering Rejection:** Late packets arriving during active, continuous streaming when the jitter buffer holds valid data must continue to be filtered to avoid playout phase distortion.

---

## Considered Options

### Option 1: IngressAdapter Bypassing Only (Freeze Playout Sequence During Starvation)
* **Description:** Stop incrementing `nextSeq` in `PopNonBlock` when the jitter buffer is empty or when in DTX mode. Bypass `IngressAdapter` for SID packets.
* **Pros:**
  - Prevents `nextSeq` from advancing while paused.
* **Cons:**
  - **Fails on undetected stalls:** If the sender halts unexpectedly (such as `snd-aloop` starving without sending DTX or SID packets), `PopNonBlock` cannot know the sender stopped. If it freezes `nextSeq`, it stops outputting audio timing entirely.
  - **Does not handle sender reboots or sequence jumps:** If the sender restarts with a random sequence seed, the receiver remains permanently frozen waiting for the old sequence.
  - Leaves the system brittle against packet loss at the start of resumption.

### Option 2: Full Buffer Reset via Control Protocol Signal (Out-of-Band RPC/Signaling)
* **Description:** Implement an out-of-band JSON-RPC or CRDT message (`StreamReset` / `StreamResume`) sent across the control plane whenever ALSA pauses or resumes, commanding the receiver to invoke `JitterBuffer.Reset()`.
* **Pros:**
  - Explicit notification of state transitions.
* **Cons:**
  - High latency: Control plane RPCs traverse HTTP/WebSocket or Gossip hops ($10\text{--}100\text{ ms}$), far too slow for real-time audio resumption ($<20\text{ ms}$).
  - Out-of-band race conditions: Audio packets frequently arrive before the asynchronous control plane message.
  - Fails during ungraceful process crashes or raw ALSA loopback underruns where no RPC can be dispatched.

### Option 3 (Selected): Ingress Control Plane Bypass & Self-Healing Playhead Resynchronization
* **Description:**
  1. **Ingress Control Plane Separation:** SID packets carrying `transport.FlagSID` bypass `FECDecoder` media recovery and `IngressAdapter` format normalization entirely, routing directly to `JitterBuffer.PushWithFlags` with their native 0-byte payload.
  2. **Zero-Byte Payload Acceptance in JitterBuffer:** Allow 0-byte payloads in `PushWithFlags` when `FlagSID` is set.
  3. **Self-Healing Playhead Resynchronization Invariant:** Automatically re-anchor the playhead (`originSeq`, `nextSeq`, `originTime`, `playoutTime`) inside `JitterBuffer.PushWithFlags` upon detecting any of the following conditions:
     - Uninitialized sequence (`!seqInitialized`).
     - Control frame arrival (`isSID`).
     - Resumption from DTX mode (`dtxMode` transition).
     - Sequence discontinuity exceeding buffer capacity: forward jump ($D \ge C$), or backward jump while starved ($D \le -C \land B = 0$).
     - Resumption after buffer starvation: Ring buffer is empty (`buffered == 0`) AND at least one starvation indicator is true:
       * Two or more consecutive underruns (`consecutiveUnderruns >= 2`), OR
       * Inter-packet arrival gap exceeding threshold ($\Delta t_{\text{recv}} \ge 100\text{ ms}$), OR
       * Playout head has drifted past arrival sequence ($\text{distFromNext} \le -5$).
  4. **DSP State Reset on Re-anchor:** Clear stale ring buffer slots, reset Packet Loss Concealment (PLC) history buffers, and clear the clock drift PI integrator accumulator to ensure acoustic transparency.
  5. **Maintained In-Band Late Packet Rejection:** When the ring buffer holds active data (`buffered > 0`) within nominal capacity bounds, legitimate out-of-order late packets continue to be rejected.

---

## Decision Outcome

Chosen Option: **Option 3**.

The architecture establishes a strict separation between media streaming payloads and transport control signaling, combined with an autonomous, self-healing playhead synchronization engine inside `pkg/streamer/jitter.go`.

### Detailed Architectural Specifications

```
+-------------------------------------------------------------------------------------------------------------+
|                                    INGRESS PIPELINE ARCHITECTURE (ADR 0033)                                 |
+-------------------------------------------------------------------------------------------------------------+
|                                                                                                             |
|  transport.Packet Arrives                                                                                   |
|           |                                                                                                 |
|           v                                                                                                 |
|  [ hdr.Flags & FlagSID != 0 ? ]                                                                             |
|      |                                                                                                      |
|      |-- YES (Control Plane Frame) ---------------------------------------------------------------------+   |
|      |   - Payload: 0 bytes                                                                             |   |
|      |   - BYPASS IngressAdapter (No resampling, no channel mixing)                                     |   |
|      |   - BYPASS FECDecoder (No parity reconstruction)                                                 |   |
|      |                                                                                                  |   |
|      +-- NO (Media Frame)                                                                               |   |
|           |                                                                                             |   |
|           v                                                                                             |   |
|     FECDecoder.ProcessMedia() (Parity reconstruction)                                                   |   |
|           |                                                                                             |   |
|           v                                                                                             |   |
|     IngressAdapter.Process() (Sample rate conversion e.g. 44.1k -> 48k, Mono/Stereo matrixing)          |   |
|           |                                                                                             |   |
|           +---------------------------------------------------------------------------------------------+   |
|                                                     |                                                       |
|                                                     v                                                       |
|                                        JitterBuffer.PushWithFlags(seq, ts, payload, flags)                  |
|                                                     |                                                       |
|                         +---------------------------+---------------------------+                           |
|                         |                                                       |                           |
|                         v [Self-Healing Evaluation]                             v [Normal Path]             |
|                 Triggers Re-anchor?                                     buffered > 0 &&                     |
|                 - !seqInitialized                                       -capacity < dist < 0                |
|                 - isSID                                                         |                           |
|                 - dtxMode exit                                                  v                           |
|                 - dist >= capacity || (dist <= -capacity && buffered == 0)   Drop as Late Packet                 |
|                 - buffered == 0 && (underruns>=2 || gap>=100ms || dist<=-5)                                 |
|                         |                                                                                   |
|                         v                                                                                   |
|                 RE-ANCHOR PLAYHEAD:                                                                         |
|                 1. originSeq = seq, nextSeq = seq                                                           |
|                 2. originTime = ts, playoutTime = now + targetDelay                                         |
|                 3. Flush stale ring buffer slots (valid = false)                                            |
|                 4. Reset DSP: PLC.Reset(), DriftController.Reset()                                          |
|                                                                                                             |
+-------------------------------------------------------------------------------------------------------------+
```

### 1. Ingress Control Plane Bypass (`pkg/streamer/streamer.go`)
In `Streamer.handlePacket`, packets carrying `transport.FlagSID` bypass all media transformations:
1. `peer.FECDecoder.ProcessMedia` is skipped. SID frames do not carry media data and must not affect FEC parity equations.
2. `peer.IngressAdapter.Process` is bypassed. 0-byte SID frames are delivered directly to `PushWithFlags` without memory reallocation or sample rate conversion.
3. Media packets with audio payload continue through `IngressAdapter` normalization as required.

### 2. JitterBuffer Re-anchoring and Self-Healing (`pkg/streamer/jitter.go`)

#### Frame Size Validation
`PushWithFlags` permits 0-byte payloads if and only if `isSID` is true:
```go
if isSID {
    // 0-byte payload is valid for SID control frames
    if len(data) != 0 && len(data) != jb.frameSize {
        return 0, errors.New("invalid frame size")
    }
} else if len(data) != jb.frameSize {
    return 0, errors.New("invalid frame size")
}
```

#### Re-synchronization State Machine Invariant
Let:
- $S$: Sequence number of the arriving packet (`seq`).
- $S_{\text{next}}$: Playout sequence cursor (`jb.nextSeq`).
- $D = \text{int32}(S - S_{\text{next}})$: Signed distance under RFC 1982 serial arithmetic.
- $C$: Ring buffer capacity (`jb.capacity`).
- $B$: Currently buffered valid frames count (`jb.buffered`).
- $U_{\text{consec}}$: Consecutive playout underruns count (`consecutiveUnderruns`).
- $\Delta t_{\text{recv}} = t_{\text{now}} - t_{\text{lastRecv}}$: Elapsed time since last received packet.

The playhead is re-anchored ($\text{ReAnchor}(S, t)$) if and only if:

$$\text{Trigger} = \neg\text{seqInitialized} \lor \text{isSID} \lor \text{dtxExit} \lor (D \ge C) \lor \left( B = 0 \land \left( D \le -C \lor U_{\text{consec}} \ge 2 \lor \Delta t_{\text{recv}} \ge 100\text{ ms} \lor D \le -5 \right) \right)$$

#### Re-anchor State Reset Routine
When $\text{Trigger}$ evaluates to true:
1. **Playhead Alignment:**
   $$\text{originSeq} \leftarrow S, \quad \text{nextSeq} \leftarrow S, \quad \text{originTime} \leftarrow t_{\text{packet}}, \quad \text{playoutTime} \leftarrow t_{\text{now}} + T_{\text{targetDelay}}$$
2. **Buffer Flush:** Every slot in the internal ring buffer is marked invalid ($f.\text{valid} \leftarrow \text{false}$), eliminating stale audio fragments.
3. **Playout State Alignment:** If `isSID`, $\text{dtxMode} \leftarrow \text{true}$; otherwise, $\text{dtxMode} \leftarrow \text{false}$.
4. **DSP State Reset:**
   - **Packet Loss Concealment (PLC):** Invalidate concealment history buffer. Concealed waveform extrapolations from prior to the stall must not be spliced onto newly arriving audio.
   - **Clock Drift Controller:** Reset PI filter integral error accumulator to zero. Timing gaps caused by pauses must not be misconstrued as physical DAC clock drift.

---

## Consequences

### Positive Consequences
* **Permanent Mute Defect Eliminated:** Streams recover immediately ($\le 20\text{ ms}$) after pauses in Spotify/Raspotify, ALSA loopback underruns, long conversational DTX pauses, or sender restarts.
* **Control Plane Integrity:** 0-byte SID frames bypass audio DSP resampling, eliminating buffer corruption and size mismatch errors.
* **Acoustic Cleanliness:** Zero clicks, pops, or screeching artifacts on stream resumption due to mandatory PLC history purging and stale ring slot flushing.
* **Robust Clock Synchronization:** Clock drift PI filter integrator is protected against spurious adjustments during pauses.
* **Deterministic Hot-Path Performance:** All re-synchronization decisions use $O(1)$ scalar arithmetic and pre-allocated ring buffers with zero dynamic heap allocations.

### Negative Consequences and Trade-offs
* **Sequence Continuity Discontinuity:** Playout timeline resets drop the strict mathematical guarantee of continuous linear RTP sequence progression during an underrun, which downstream logging or strict RTP diagnostic monitors must account for.
* **Late Packet Tolerance Boundary:** If a burst of late packets arrives immediately following a starvation event while $B=0$, the first late packet will trigger a re-anchor rather than being discarded. This trade-off is required: restoring live audio after starvation strictly supersedes filtering late packets into silence.

---

## Performance and Verification Baseline

| Test Scenario | Legacy Behavior | Post-ADR 0033 Behavior |
| :--- | :--- | :--- |
| **5-second DTX Silence Resumption** | Permanent silence ($0.0\text{ dBFS}$), $100\%$ late packet drop | Seamless resumption at $t \le 20\text{ ms}$ |
| **ALSA Loopback (`snd-aloop`) Pause/Resume** | Stream stalls permanently; requires route recreation | Self-heals within 1 frame after playback restarts |
| **0-byte SID Frame Delivery @ 44.1 kHz Ingress** | Resampled to 2088 bytes; rejected with `invalid frame size` | Bypasses resampler; activates receiver `dtxMode` cleanly |
| **Remote Node Reboot / Sequence Reset** | Stalls permanently if $|D| \ge \text{capacity}$ | Triggers playhead re-anchor; restores audio immediately |
| **Active Network Packet Reordering** | Correctly drops out-of-order late packets | Unchanged: continues dropping late packets when $B > 0$ |
