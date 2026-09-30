# ADR 0013: Canonical Persistent Audio Device URNs and Ghost Device Resiliency

* **Status:** Accepted
* **Deciders:** Core Systems Architecture Team, Distributed Audio & Network Engineering, Frontend Platform Engineering
* **Date:** 2026-09-30

---

## Context and Problem Statement

`audio-router-p2p` orchestrates low-latency, multi-channel distributed audio streaming across heterogeneous hardware platforms, virtual loopback buses, and peer-to-peer network meshes (ADR 0001, ADR 0002, ADR 0006). In production sound installations, stage-box deployments, and multi-node clusters (ADR 0011, ADR 0012), audio endpoints must be identified, configured, persisted, and routed with absolute determinism.

Historically, the engine relied on transient device identifiers inherited from low-level host audio APIs:
1. **PortAudio Dynamic Array Indices (`0, 1, 2, ...`)**: PortAudio assigns sequential integer indices based on enumeration order at runtime. Whenever any audio peripheral is connected or disconnected, PortAudio re-enumerates all host devices and reshuffles the entire index space.
2. **ALSA Hardware Device Strings (`hw:0,0`, `hw:1,0`)**: On Linux, ALSA card numbers (`card0`, `card1`) are allocated dynamically by the kernel driver probe sequence at boot time. Asynchronous driver loading routinely introduces probe races: a high-end USB audio interface (e.g., Focusrite Scarlett or Behringer X32) and an onboard Intel HD Audio controller frequently swap card numbers (`card0` $\leftrightarrow$ `card1`) across successive reboots.

### The Operational Crisis of Transient Identifiers

Using volatile identifiers (`0`, `hw:0,0`) across persistent application layers caused severe architectural fractures:

* **Persistent Routing Failures and Accidental Misrouting**: Saved room topologies (`rooms.json`), matrix route tables (`matrix_route.go`), and frontend canvas layouts (`flowLayoutStore`, ADR 0012) persisted integer device IDs or ALSA card strings. Following a host reboot or USB reconnect, a room configured to capture an executive microphone on device `0` would inadvertently capture ambient noise or HDMI audio, or attempt to stream from a non-existent index and crash the pipeline.
* **Hotplug Pipeline Halts & Broken Signal Chains**: When a physical USB soundcard was accidentally unplugged or power-cycled during live broadcast, downstream consumers in `pkg/group` and `pkg/router` encountered hard I/O read/write errors. The audio processing loop terminated, and room routing structures were torn down. When the soundcard was plugged back in, it received a different PortAudio index or ALSA card number, preventing automatic recovery and leaving the room permanently silent.
* **Cluster Mesh Ambiguity**: In distributed mesh topologies (ADR 0011), exchanging unqualified device IDs like `"0"` or `"hw:0,0"` across nodes created severe namespace collisions. Remote nodes could not disambiguate local hardware from remote peer interfaces without brittle ad-hoc prefixes.
* **Canvas Spatial Desynchronization**: The SvelteFlow visual workspace (`RoomCanvas.svelte`, ADR 0012) keyed its staged node positions to device IDs (e.g., `input-0`, `output-hw:1`). Any index shift scrambled node configurations, invalidated saved positions in `localStorage`, and forced operators to manually reconstruct complex routing setups.

A unified, persistent, cross-platform identification scheme and a fault-tolerant ghost device handling mechanism are urgently required.

---

## Decision Drivers

* **Absolute Determinism Across Reboots & Hotplugs**: Audio endpoint identifiers must remain strictly stable across operating system reboots, kernel driver probe races, USB hub port reassignments, and peripheral power cycles.
* **RFC 8141 URN Compliance**: Identifiers must follow standard Uniform Resource Name (URN) syntax, providing structured, human-readable, and machine-parseable namespaces.
* **Pure-Go Hardware Discovery (Zero CGO Overhead)**: Linux hardware topology discovery must directly query `/sys/class/sound` and `/proc/asound` in native Go, avoiding CGO locks, ALSA library version mismatches, and driver blocking.
* **Fault-Tolerant Ghost Device Resiliency**: Disconnecting a physical audio device must never terminate room mixers, crash routing pipelines, or drop client connections. The subsystem must seamlessly substitute disconnected hardware with a paced "Ghost" stream generating real-time silence.
* **Zero-Touch Automatic Re-Binding**: When a disconnected peripheral is re-attached, the resolver must match its canonical URN, transparently hot-swap the physical audio stream into the active mixer, and retire the ghost generator without human intervention.
* **Backwards Compatibility**: Existing REST API consumers and configuration files using legacy integer indices (`"0"`) or ALSA strings (`"hw:0,0"`) must resolve transparently via alias mapping.
* **Unified Cluster Namespace**: The URN structure must naturally federate across multi-node meshes, cleanly distinguishing local hardware, remote peer interfaces, and virtual software loopbacks.

---

## Considered Options

### Option 1: OS-Level Static ALSA Module Options & Udev Rules
* **Description**: Enforce fixed ALSA card indices by deploying `/etc/modprobe.d/alsa-base.conf` configuration files (specifying `options snd-usb-audio index=0`) and custom `udev` symlink rules on each host operating system.
* **Drawbacks**:
  * Host-intrusive: Requires root/sudo privileges and bespoke OS-level file modifications on every target machine.
  * Incompatible with non-Linux hosts (macOS CoreAudio, Windows WASAPI) and unprivileged Docker/Kubernetes container deployments.
  * Fails to solve PortAudio dynamic array re-indexing upon runtime USB hotplugging.
  * Extremely brittle in environments with multiple identical soundcards sharing the same driver module (`snd-usb-audio`).

### Option 2: Device Name Slug Matching with Runtime CGO Probing
* **Description**: Use friendly device names returned by PortAudio (e.g., `"Focusrite Scarlett 2i2 USB"`) as the primary persistent key. Query ALSA device properties via CGO bindings to `libasound2`.
* **Drawbacks**:
  * Namespace collisions: Two identical audio interfaces connected to the same host produce identical friendly names, causing total ambiguity.
  * String instability: Driver updates or OS language/locale shifts can subtly alter reported device strings (e.g., adding `(hw:0,0)` or changing capitalization).
  * Heavy CGO build burden and risk of thread deadlocks when polling unresponsive audio drivers.

### Option 3: RFC 8141 Canonical Persistent Audio Device URN Subsystem with Ghost Device Resiliency [CHOSEN]
* **Description**:
  1. Implement a four-tier fallback URN grammar conforming to RFC 8141:
     - Tier 1: USB Serial (`urn:audio:hw:usb:v=VID:p=PID:s=SERIAL:d=DEV`)
     - Tier 2: Sysfs Physical Bus Path (`urn:audio:hw:path:BUS_PATH:d=DEV`) for serial-less hardware
     - Tier 3: ALSA Card ID (`urn:audio:hw:alsa:c=CARD_ID:d=DEV`)
     - Tier 4: Clean Name Slug + Hash (`urn:audio:hw:name:SLUG:HASH8`) for macOS, Windows, and emulators
     - Virtual Loopbacks: `urn:audio:virt:SLUG`
     - Federated Cluster Mesh: `urn:audio:node:NODE_ID:dev:LOCAL_URN`
  2. Implement a pure-Go Linux discovery engine in `pkg/audio/discovery` that inspects `/sys/class/sound` and `/proc/asound`.
  3. Introduce a Device Resolver with transparent legacy alias mapping and Ghost Device state management. Disconnected hardware transitions to `quarantined` state, serviced by a monotonic clock-paced silence generator (`GhostAudioStream`), and automatically re-binds upon physical hotplug return.
* **Advantages**: Fully deterministic, container-compatible, zero CGO discovery on Linux, completely non-blocking audio signal chains, and unified cluster-wide addressing.

---

## Decision Outcome

Chosen Option: **Option 3: RFC 8141 Canonical Persistent Audio Device URN Subsystem with Ghost Device Resiliency**.

---

### 1. RFC 8141 URN Namespace Grammar

All audio devices across local daemons and distributed clusters are identified by an immutable, RFC 8141-compliant URN string conforming to the following formal grammar:

```
urn:audio:<scope>:<type-specific-string>
```

Where `<scope>` is one of:
* `hw`: Physical host audio hardware.
* `virt`: Software-defined in-memory loopback or DSP bus.
* `node`: Distributed cluster mesh device proxy.

#### Hardware Hierarchy (Resolution Tiers)

Hardware URNs are generated via a deterministic four-tier waterfall:

```
               +-------------------------------------------+
               | Enumerate Physical Host Audio Endpoints    |
               +-------------------------------------------+
                                     |
                                     v
                 [ Has Valid USB Vendor, Product & Serial? ]
                               /           \
                       YES    /             \    NO
                             v               v
               +--------------------+   [ Has Sysfs Bus Topology Path? ]
               | Tier 1: USB Serial |           /           \
               +--------------------+   YES    /             \    NO
                                              v               v
                               +--------------------+   [ Has ALSA Card String ID? ]
                               | Tier 2: Bus Path   |           /           \
                               +--------------------+   YES    /             \    NO
                                                              v               v
                                                +--------------------+  +--------------------+
                                                | Tier 3: ALSA ID    |  | Tier 4: Name+Hash  |
                                                +--------------------+  +--------------------+
```

1. **Tier 1 — USB Serial Identifier (Highest Stability)**
   * **Syntax**: `urn:audio:hw:usb:v={VID}:p={PID}:s={SERIAL}:d={DEV}`
   * **Attributes**:
     * `v`: 4-character hex USB Vendor ID (e.g., `1235` for Focusrite).
     * `p`: 4-character hex USB Product ID (e.g., `8210` for Scarlett 2i2).
     * `s`: Alphanumeric device hardware serial number extracted from USB descriptors.
     * `d`: Zero-based PCM device index on the physical card (defaults to `0`).
   * **Example**: `urn:audio:hw:usb:v=1235:p=8210:s=10042718:d=0`
   * **Stability**: Retains absolute uniqueness regardless of USB port, hub topology, or system reboots.

2. **Tier 2 — Sysfs Physical Bus Path (Serial-less Hardware)**
   * **Syntax**: `urn:audio:hw:path:{BUS_PATH}:d={DEV}`
   * **Attributes**:
     * `BUS_PATH`: Physical hardware bus address hierarchy extracted from sysfs (e.g., PCI root, hub port tree).
     * `d`: Subdevice index.
   * **Example**: `urn:audio:hw:path:pci-0000:00:14.0-usb-0:2.1:1.0:d=0`
   * **Stability**: Deterministic across reboots provided the device remains connected to the same physical USB/PCI port.

3. **Tier 3 — ALSA Card String Identifier**
   * **Syntax**: `urn:audio:hw:alsa:c={CARD_ID}:d={DEV}`
   * **Attributes**:
     * `c`: Human-readable ALSA string identifier from `/proc/asound/cards` (e.g., `PCH`, `HDMI`, `CODEC`), *never* the volatile numeric card index.
     * `d`: PCM subdevice index.
   * **Example**: `urn:audio:hw:alsa:c=PCH:d=0`
   * **Stability**: Immune to card index probe races (`card0` vs `card1`) for distinct hardware soundcard types.

4. **Tier 4 — Clean Name Slug + Murmur3 Hash (Fallback Tier)**
   * **Syntax**: `urn:audio:hw:name:{SLUG}:{HASH8}`
   * **Attributes**:
     * `SLUG`: Normalized, lowercased alphanumeric string with hyphens (e.g., `built-in-microphone`).
     * `HASH8`: First 8 hex characters of the 64-bit hash computed over the full un-normalized device string, driver class, and channel profile.
   * **Example**: `urn:audio:hw:name:built-in-microphone:a3b89c01`
   * **Usage**: Fallback on non-Linux platforms (macOS CoreAudio, Windows WASAPI, PortAudio emulators) where sysfs is unavailable.

#### Virtual and Cluster Mesh URNs

* **Virtual Loopback Devices**:
  * **Syntax**: `urn:audio:virt:{SLUG}`
  * **Example**: `urn:audio:virt:monitor-bus`, `urn:audio:virt:broadcast-aux`
* **Cluster Mesh Devices (Distributed Federation)**:
  * **Syntax**: `urn:audio:node:{NODE_ID}:dev:{LOCAL_URN}`
  * **Example**: `urn:audio:node:stagebox-01:dev:urn:audio:hw:usb:v=1235:p=8210:s=10042718:d=0`
  * Guarantees total collision freedom across distributed clusters without altering the underlying local device URN.

---

### 2. Pure-Go Linux Hardware Discovery (`pkg/audio/discovery`)

To eliminate CGO build friction, driver blocking, and runtime overhead, device topology discovery on Linux is implemented via direct, pure-Go filesystem inspection:

```
Linux Kernel Virtual Filesystems
  |
  +---> /proc/asound/cards -----------------> Map card index (0, 1) to ALSA String ID & Card Name
  |
  +---> /proc/asound/pcm -------------------> Extract available PCM devices and capture/playback streams
  |
  +---> /sys/class/sound/card{N}/device ----> Traverse symlinks to parent USB/PCI subsystem
          |
          +---> idVendor   (USB VID)
          +---> idProduct  (USB PID)
          +---> serial     (USB Serial string)
          +---> uevent     (PCI bus path & driver identity)
```

#### Discovery Workflow
1. **Card & PCM Enumeration**: The discovery engine parses `/proc/asound/cards` to correlate card indices with ALSA string IDs. It then parses `/proc/asound/pcm` to identify available PCM device numbers and channel capabilities (`capture`, `playback`).
2. **Sysfs Traversal**: For each card, the engine inspects `/sys/class/sound/card{N}/device`. It reads the symbolic target to determine the host bus type:
   - For USB audio devices (`snd-usb-audio`), the engine walks up the sysfs tree to locate the parent USB interface and device nodes, reading `idVendor`, `idProduct`, and `serial`.
   - If `serial` is missing or unreadable, the engine extracts the canonical USB bus topology path (e.g., `usb-0000:00:14.0-1.2`).
3. **Correlation with Audio Engine**: The discovered hardware metadata is correlated with PortAudio/ALSA runtime device entries to associate hardware URNs with active stream opening descriptors.

---

### 3. Device Resolver & Ghost Device Resiliency

The device subsystem introduces an explicit state machine and stream proxy to guarantee uninterrupted audio routing:

```
   [ Physical Connect ]                  [ Hardware Unplugged ]
+------------------------+             +------------------------+
|         ACTIVE         | ----------> |       QUARANTINED      |
| Real HW Stream Open    |             | (Ghost Paced Silence)  |
+------------------------+             +------------------------+
            ^                                       |
            |           [ Physical Reconnect ]      |
            +---------------------------------------+
                     (Automatic Re-bind)
```

#### Device Lifecycle States
* **`active`**: Physical or virtual device is open, operational, and processing PCM frames.
* **`quarantined` (Ghost Device)**: Hardware has been disconnected or failed to open. The routing graph retains all connections, while the endpoint is substituted by an internal `GhostAudioStream`.
* **`error`**: Unrecoverable driver failure or invalid configuration parameter.

#### Ghost Audio Stream Implementation (`GhostAudioStream`)
When a device transitions to `quarantined`:
1. The room mixer and matrix crosspoint routes remain active. Routes are **not** torn down.
2. The real hardware audio stream is replaced with a `GhostAudioStream`.
3. **Monotonic Clock Pacing**: The `GhostAudioStream` runs a precision Go `time.Ticker` set to the exact buffer period (e.g., $10\text{ ms} = 480\text{ frames}$ at $48\text{ kHz}$).
4. On read calls (`Read(buf []float32)`), the ghost stream paces execution to the nominal clock rate and fills the buffer with calibrated zero-samples (silence). This prevents CPU spinning, avoids buffer underflow in downstream mixing matrices, and maintains real-time pacing for remote Opus/WebRTC streams.
5. On write calls (`Write(buf []float32)`), the ghost stream consumes samples at nominal clock pacing and safely discards them.

#### Automatic Hotplug Re-Binding
1. A background polling loop (or Linux netlink/uevent listener) monitors audio hardware changes at regular intervals ($1000\text{ ms}$).
2. When a device is detected whose canonical URN matches a `quarantined` endpoint:
   - The engine opens the real hardware audio stream.
   - The mixer executes a thread-safe atomic pointer swap, replacing the `GhostAudioStream` with the physical stream.
   - The device status transitions from `quarantined` back to `active`.
   - A cluster telemetry event (`device.rebound`) is emitted over SSE/WebSocket.

#### Transparent Legacy Alias Mapping
To preserve full backward compatibility with scripts, curl calls, and existing frontends:
* Numeric strings (`"0"`, `"1"`) are mapped to the active PortAudio array index at the time of resolution.
* ALSA strings (`"hw:0,0"`) are resolved via `/proc/asound` to the corresponding canonical URN.
* The API returns both `id` (canonical URN) and `legacy_id` (transient string) in JSON payloads.

---

### 4. Cross-Subsystem Integration

#### Audio Subsystem (`pkg/audio`)
* `audio.Device` struct is updated to include `URN string`, `CanonicalURN string`, `LegacyID string`, and `State string`.
* Implementation of `GhostAudioStream` implementing the `AudioStream` interface with non-blocking, clock-paced silence generation.

#### REST API & OpenAPI Schema (`pkg/api/v1`)
* `AudioDevice` response payload:
  ```json
  {
    "id": "urn:audio:hw:usb:v=1235:p=8210:s=10042718:d=0",
    "legacy_id": "0",
    "name": "Focusrite Scarlett 2i2 USB",
    "max_input": 2,
    "max_output": 2,
    "default_sample_rate": 48000,
    "is_virtual": false,
    "state": "active",
    "volume": 1.0,
    "muted": false,
    "supported_layouts": ["mono", "stereo"]
  }
  ```
* All route creation and room input/output endpoints accept both canonical URNs and legacy aliases.

#### Distributed Mesh & Cluster Summary (`pkg/cluster`)
* `AudioDeviceSummary` utilizes canonical URNs for cluster-wide uniqueness.
* Remote device references adopt the composite federated format `urn:audio:node:{NODE_ID}:dev:{LOCAL_URN}`, allowing any gateway node to target remote hardware unambiguously.

#### Room Manager & Bus Engine (`pkg/group`)
* Room configuration files persist canonical URNs in `inputs` and `outputs` arrays.
* Room mixer loops handle `quarantined` state gracefully, logging health degradation to cluster telemetry while keeping the room online.

#### Frontend Canvas & Layout Store (`web/src/lib/stores/flowLayout.svelte.ts`)
* `flowLayoutStore` stores canonical URNs in `stagedDeviceIds` and node coordinate keys.
* URL/DOM Safe Encoding: Because standard URNs contain colons (`:`), SvelteFlow node IDs use sanitized string prefixes (e.g., `input-node--urn--audio--hw--usb...`) or standard `encodeURIComponent` to prevent DOM selector collisions.
* Visual Ghost Badge: Staged device nodes whose state is `quarantined` render an amber/red "Ghost (Disconnected)" status pill, visually alerting operators while keeping all canvas edges and fader settings intact.

---

## Visual Architecture

```mermaid
flowchart TD
    subgraph Host_Discovery["Linux Host Discovery (pkg/audio/discovery)"]
        SysFS["/sys/class/sound<br/>/proc/asound"] -->|Pure-Go Parser| RawInfo["Hardware Descriptors<br/>(VID, PID, Serial, Bus Path, ALSA ID)"]
        RawInfo --> URNGen["URN Generator Engine"]
        URNGen -->|Tier 1| URN_USB["urn:audio:hw:usb:v=...:p=...:s=...:d=0"]
        URNGen -->|Tier 2| URN_PATH["urn:audio:hw:path:...:d=0"]
        URNGen -->|Tier 3| URN_ALSA["urn:audio:hw:alsa:c=...:d=0"]
        URNGen -->|Tier 4| URN_NAME["urn:audio:hw:name:...:hash"]
    end

    subgraph Core_Resolver["Device Resolver & Resiliency Engine"]
        API_Req["API Request / Stored Route"] --> Resolver{"Device Resolver"}
        Resolver -->|Matches URN| DevReg["Device Registry"]
        Resolver -->|Legacy 'hw:0' or '0'| AliasMap["Alias Translation Map"] --> DevReg
        
        DevReg --> CheckStatus{"Physical Hardware<br/>Present?"}
        CheckStatus -->|YES| OpenHW["Open Physical Stream<br/>(PortAudio / ALSA)"]
        CheckStatus -->|NO| Quarantined["Transition to QUARANTINED<br/>(Ghost Device)"]
        
        Quarantined --> GhostStream["GhostAudioStream<br/>(Clock-Paced Silence Generator)"]
        HotplugWatcher["Hotplug / Uevent Watcher"] -->|Device Re-attached| Rebind["Atomic Re-bind<br/>Swap Ghost -> HW Stream"]
        Rebind --> OpenHW
    end

    subgraph Audio_Pipeline["Real-Time Audio Engine (pkg/group & pkg/router)"]
        OpenHW --> ActiveMixer["Room Audio Bus / Matrix Mixer"]
        GhostStream -->|Paced Silence (No Stall)| ActiveMixer
        ActiveMixer --> DSP["Audio DSP & Opus Network Streaming"]
    end

    subgraph Client_Presentation["WebUI / SvelteFlow (flowLayoutStore)"]
        ActiveMixer -.->|SSE Telemetry| UI_Store["flowLayoutStore / clusterStore"]
        UI_Store --> NodeActive["Active Device Node<br/>(Green Status)"]
        UI_Store --> NodeGhost["Ghost Device Node<br/>(Amber 'Quarantined' Pill)"]
        NodeActive --> Canvas["Interactive Room Canvas<br/>(Persistent Layout Preserved)"]
        NodeGhost --> Canvas
    end

    classDef primary fill:#2563eb,stroke:#1d4ed8,stroke-width:2px,color:#fff;
    classDef warning fill:#d97706,stroke:#b45309,stroke-width:2px,color:#fff;
    classDef success fill:#059669,stroke:#047857,stroke-width:2px,color:#fff;
    classDef neutral fill:#374151,stroke:#1f2937,stroke-width:1px,color:#fff;

    class URNGen,DevReg,ActiveMixer primary;
    class Quarantined,GhostStream,NodeGhost warning;
    class OpenHW,NodeActive,Rebind success;
    class SysFS,RawInfo,AliasMap neutral;
```

---

## Consequences

### Positive Consequences

* **Permanent Endpoint Stability**: Audio routing and room associations persist flawlessly across host reboots, kernel driver index swapping, and USB hub re-plugging.
* **Fault-Tolerant Audio Pipelines**: Disconnecting a physical cable or power-cycling a soundcard no longer crashes room mixers or halts audio streaming; downstream network streams receive paced silence rather than terminating with I/O errors.
* **Seamless Zero-Touch Recovery**: When hardware is reconnected, the system automatically detects the canonical URN, swaps out the ghost stream, and resumes audio capture/playback with zero operator intervention.
* **Pure-Go Portability**: Hardware discovery on Linux operates without CGO dependencies or external tool requirements, guaranteeing minimal binary size and rapid initialization in edge/container environments.
* **Global Cluster Uniqueness**: The hierarchical URN syntax (`urn:audio:node:{NODE_ID}:dev:{LOCAL_URN}`) completely eliminates ambiguity when managing audio devices across multi-node peer networks.
* **Preserved Canvas Layouts**: The WebUI SvelteFlow layout remains intact when devices are disconnected; nodes retain their user-curated positions and visual room connections.

### Negative Consequences

* **Discovery Sysfs Parsing Overhead**: Scanning `/sys/class/sound` and `/proc/asound` adds $2\text{–}5\text{ ms}$ of latency during daemon startup or hotplug scans compared to querying raw numeric indices. (Mitigation: Enumeration results are cached and updated incrementally).
* **String Delimiter Sensitivity**: URNs contain colons (`:`) and equals signs (`=`). When used in REST API URL paths (e.g., `/api/v1/devices/{id}`) or DOM element IDs in SvelteFlow, strings must be properly URL-encoded (`encodeURIComponent`) or escaped to prevent routing and CSS selector bugs.
* **Non-Linux Fallback Degradation**: On platforms lacking sysfs (macOS, Windows, or emulators), discovery falls back to Tier 4 (Name Slug + Hash). While stable, Tier 4 cannot differentiate two identical hardware interfaces connected simultaneously without unique platform serial numbers.

### Neutral Consequences

* **Legacy Alias Maintenance**: The device resolver must maintain an in-memory alias index mapping transient integers (`0, 1`) and ALSA strings (`hw:0,0`) to canonical URNs to ensure legacy client scripts continue to operate.
* **Ghost Telemetry Ingestion**: Management clients and monitoring dashboards must handle the `quarantined` state badge, presenting clear visual indicators to avoid confusing operators during hardware outages.

---

## Next Steps

1. **Implement Core Discovery Engine**:
   - Create `pkg/audio/discovery/sysfs_linux.go` and `pkg/audio/discovery/discovery.go` to parse `/sys/class/sound` and `/proc/asound`.
   - Implement the RFC 8141 URN generator with fallback tiers (USB Serial $\to$ Sysfs Path $\to$ ALSA ID $\to$ Name Slug).
2. **Implement Ghost Audio Stream**:
   - Create `GhostAudioStream` in `pkg/audio/ghost.go` featuring a monotonic clock ticker and non-blocking zero-sample generator.
3. **Refactor Device Registry & Resolver**:
   - Update `pkg/api/v1/device_handlers.go` and `pkg/audio/devices.go` to index devices by canonical URN and maintain transparent legacy alias mappings.
   - Implement hotplug re-binding logic and state transitions (`active` $\leftrightarrow$ `quarantined`).
4. **Update Distributed Models & Storage**:
   - Update `pkg/cluster/types.go` and `pkg/group/room.go` to store and exchange canonical device URNs.
5. **Frontend URL-Safe Keying**:
   - Update `web/src/lib/stores/flowLayout.svelte.ts` and `web/src/lib/components/rooms/flow/` to safely encode URNs for SvelteFlow node IDs and render the amber "Ghost" status pill for quarantined devices.
