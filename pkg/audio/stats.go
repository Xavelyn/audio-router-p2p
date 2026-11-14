package audio

// This file defines the optional interfaces and snapshot types that make
// audio artifacts (hardware XRuns, ring-buffer drops) observable per device
// and per route without touching the AudioStream contract (ADR 0034, Pass A).

// XRunHookSetter is optionally implemented by audio streams that can observe
// hardware XRun events at their own error sites (PortAudio input overflow /
// output underflow). The installed hook fires exactly once per XRun, after
// the existing 1/s rate-limited log line, and runs on the audio thread: it
// must be lock-free, allocation-free, and non-blocking. A nil hook clears the
// installed hook.
//
// Implementations: portaudioInput, portaudioOutput (open.go), and
// resampledAudioStream (which forwards to its underlying stream). Ghost,
// emulator, and virtual streams do not implement it and are safely skipped
// by the optional-interface type assertion at the install site.
type XRunHookSetter interface {
	SetXRunHook(hook func())
}

// DropCounter is optionally implemented by streams that expose how much PCM
// data their internal ring buffer has dropped over their lifetime. Bytes are
// accounted exactly per ringBuf.write call; an event is one write that
// dropped at least one byte.
type DropCounter interface {
	// DroppedBytes returns the lifetime count of PCM bytes dropped.
	DroppedBytes() uint64
	// DropEvents returns the lifetime count of drop events (individual
	// ring-buffer overruns).
	DropEvents() uint64
}

// DeviceStreamStats is a point-in-time, read-only snapshot of one active
// directional deviceStreamController (ADR 0034). It carries the controller's
// identity plus the exact, device-lifetime hardware XRun totals accumulated
// by the XRun push hooks. The XRun totals are monotonic across physical
// stream swaps and are never reset while the controller is alive; a missing
// direction (e.g. an input-only device) simply stays at 0.
type DeviceStreamStats struct {
	URN       string
	Direction StreamDirection
	// Ghost reports whether the controller's current physical stream is a
	// GhostAudioStream (device quarantined / no live signal).
	Ghost bool
	// HWXrunsIn / HWXrunsOut are the exact device-lifetime hardware XRun
	// counts for the capture (input overflow) and playback (output
	// underflow) direction of this controller.
	HWXrunsIn  uint64
	HWXrunsOut uint64
}
