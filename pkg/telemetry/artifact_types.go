package telemetry

import (
	"time"

	"github.com/audio-router-p2p/pkg/audio"
)

// ArtifactSeverity is the 4-tier audibility grade for audio artifacts (ADR
// 0034). It is deliberately distinct from the 5-tier NodeHealthGrade: the two
// answer different questions (artifact audibility vs. link quality) and are
// not cross-mapped.
type ArtifactSeverity string

// The four artifact severity tiers.
const (
	SeverityNone     ArtifactSeverity = "NONE"     // Score >= 90
	SeverityMinor    ArtifactSeverity = "MINOR"    // Score >= 60
	SeverityMajor    ArtifactSeverity = "MAJOR"    // Score >= 30
	SeverityCritical ArtifactSeverity = "CRITICAL" // Score < 30
)

// RouteArtifactSample is a point-in-time, lock-free sample of one route's
// lifetime monotonic artifact counters (ADR 0034, Pass B). The values are
// NOT windowed: the ArtifactAggregator computes the 10 s windowed deltas
// between successive samples.
//
// The streamer fills the route-scoped sums (jitter / FEC / PLC / loss /
// playout gaps and the per-peer breakdown). Route identity and device-bound
// fields (RouteID, device URNs, ghost flags, ring drops) are zero here and
// are filled by the stream manager, which owns the route-to-device binding.
type RouteArtifactSample struct {
	RouteID         string
	SourceDeviceURN string
	DestDeviceURN   string
	SourceGhost     bool
	DestGhost       bool

	CaptureDropBytes  uint64
	CaptureDropEvents uint64
	OutputDropBytes   uint64
	OutputDropEvents  uint64

	JitterUnderruns  uint64
	JitterOverruns   uint64
	LatePackets      uint64
	PLCConcealed     uint64
	FECRecovered     uint64
	FECUnrecoverable uint64
	PacketsReceived  uint64
	PacketsLost      uint64
	PlayoutGaps      uint64

	Peers []PeerArtifactSample
}

// PeerArtifactSample is the windowed per-peer artifact triple carried inside
// a RouteArtifactSample (ADR 0034 §3: underruns / overruns / late_packets).
type PeerArtifactSample struct {
	Addr        string
	Underruns   uint64
	Overruns    uint64
	LatePackets uint64
}

// DeviceArtifactSample is a point-in-time sample of one directional
// deviceStreamController's hardware XRun state (ADR 0034, Pass B). HWXRuns is
// the device-lifetime total for the sample's direction (monotonic across
// stream swaps); the aggregator windows it.
type DeviceArtifactSample struct {
	URN       string
	Direction audio.StreamDirection
	Ghost     bool
	HWXRuns   uint64
}

// ArtifactRates carries per-second rates over the actual ring span (ramp-up
// safe: a route that has existed for 3 s is computed over 3 s, not 10 s).
type ArtifactRates struct {
	UnderrunPerS float64 `json:"underrun_per_s"`
	OverrunPerS  float64 `json:"overrun_per_s"`
	XrunPerS     float64 `json:"xrun_per_s"`
	RingDropPerS float64 `json:"ring_drop_per_s"`
}

// PeerArtifactHealth is the windowed per-peer artifact triple rendered in the
// route health "worst peer" column.
type PeerArtifactHealth struct {
	Underruns   uint64 `json:"underruns"`
	Overruns    uint64 `json:"overruns"`
	LatePackets uint64 `json:"late_packets"`
}

// RouteHealth is the per-route artifact health evaluation over the 10 s
// window (ADR 0034). The score uses ONLY route-scoped coefficients; hardware
// XRuns are surfaced through the separate devices overlay (attribution
// discipline). XrunPerS is the device-shared figure for the route's own
// devices.
type RouteHealth struct {
	Score            float64                       `json:"score"`
	Severity         ArtifactSeverity              `json:"severity"`
	WindowS          int                           `json:"window_s"`
	Underruns        uint64                        `json:"underruns"`
	Overruns         uint64                        `json:"overruns"`
	RingDrops        uint64                        `json:"ring_drops"`
	PLCConcealed     uint64                        `json:"plc_concealed"`
	FECUnrecoverable uint64                        `json:"fec_unrecoverable"`
	PacketsLost      uint64                        `json:"packets_lost"`
	FECRecovered     uint64                        `json:"fec_recovered"`
	PlayoutGaps      uint64                        `json:"playout_gaps"`
	Rates            ArtifactRates                 `json:"rates"`
	Ghost            bool                          `json:"ghost"`
	Peers            map[string]PeerArtifactHealth `json:"peers"`
}

// DeviceDirArtifacts is the per-direction hardware XRun summary on a device.
type DeviceDirArtifacts struct {
	XRunWindow uint64 `json:"xrun_window"`
	XRunTotal  uint64 `json:"xrun_total"`
}

// DeviceHealth is the per-physical-device artifact health (ADR 0034), merging
// up to one input and one output controller per URN. A missing direction
// contributes C = 1.0 to the score and its JSON object is omitted.
type DeviceHealth struct {
	URN      string              `json:"urn"`
	Ghost    bool                `json:"ghost"`
	Score    float64             `json:"score"`
	Severity ArtifactSeverity    `json:"severity"`
	Input    *DeviceDirArtifacts `json:"input,omitempty"`
	Output   *DeviceDirArtifacts `json:"output,omitempty"`
}

// RouteDeviceImpact is the per-route overlay exposing the artifact health of
// the route's source ("input") and destination ("output") devices, so a route
// transiting a degraded device is never shown as falsely clean.
type RouteDeviceImpact struct {
	Input  *DeviceHealth `json:"input,omitempty"`
	Output *DeviceHealth `json:"output,omitempty"`
}

// ArtifactSnapshot is the immutable point-in-time view published by the
// ArtifactAggregator via an atomic pointer swap (O(1) lock-free reads).
type ArtifactSnapshot struct {
	GeneratedAt time.Time               `json:"generated_at"`
	WindowS     int                     `json:"window_s"`
	Routes      map[string]RouteHealth  `json:"routes"`
	Devices     map[string]DeviceHealth `json:"devices"`
}
