package main

import (
	"fmt"
	"time"

	"github.com/audio-router-p2p/pkg/api/v1"
	"github.com/audio-router-p2p/pkg/audio"
	"github.com/audio-router-p2p/pkg/streamer"
)

// routeStatusStale is how long a pipeline stage may go without activity
// before the route is considered not playing.
const routeStatusStale = 2 * time.Second

// routeStatusPeerGone matches the default peer idle expiry: after this long
// without packets from a sender, its receiver state is dropped.
const routeStatusPeerGone = 5 * time.Second

// RouteStatus answers "why isn't this route playing" for a live route by
// combining streamer pipeline activity, hardware buffer counters, and
// discovery state into ordered, human-readable reasons.
func (m *streamManager) RouteStatus(routeID string) (v1.RouteStatus, bool) {
	m.mu.Lock()
	as, ok := m.routes[routeID]
	m.mu.Unlock()
	if !ok {
		return v1.RouteStatus{}, false
	}

	route := as.route
	now := time.Now()
	stats := as.st.Stats()
	pipe := as.st.RouteStatus()

	out := v1.RouteStatus{
		RouteID:          routeID,
		Direction:        routeDirection(as),
		PacketsReceived:  stats.PacketsReceived,
		Underruns:        stats.Underruns,
		Overruns:         stats.Overruns,
		DTXState:         pipe.DTXState,
		CaptureOverflows: audio.CountersOf(as.in).Overflows,
		OutputUnderruns:  audio.CountersOf(as.out).Underflows,
	}
	if !pipe.LastInput.IsZero() {
		t := pipe.LastInput
		out.LastInput = &t
	}
	if !pipe.LastPlayout.IsZero() {
		t := pipe.LastPlayout
		out.LastPlayout = &t
	}

	var lastSend, lastRecv time.Time
	var sendErr string
	for _, p := range pipe.Peers {
		peer := v1.RoutePeerStatus{
			Address:           p.Addr,
			PeerID:            p.PeerID,
			Role:              peerRole(p.Direction),
			PacketsSent:       p.PacketsSent,
			PacketsLost:       p.PacketsLost,
			FECRecovered:      p.FECRecovered,
			JitterDepthFrames: p.JitterDepth,
			Underruns:         p.Underruns,
		}
		if !p.LastSend.IsZero() {
			t := p.LastSend
			peer.LastSend = &t
			if lastSend.IsZero() || t.After(lastSend) {
				lastSend = t
			}
		}
		if !p.LastRecv.IsZero() {
			t := p.LastRecv
			peer.LastRecv = &t
			if lastRecv.IsZero() || t.After(lastRecv) {
				lastRecv = t
			}
		}
		contact := peer.LastSend
		if peer.LastRecv != nil && (contact == nil || peer.LastRecv.After(*contact)) {
			contact = peer.LastRecv
		}
		peer.LastContact = contact
		if p.LastSendError != "" {
			peer.LastSendError = p.LastSendError
			if sendErr == "" {
				sendErr = p.LastSendError
			}
		}
		if p.JitterDepth > out.JitterDepthFrames {
			out.JitterDepthFrames = p.JitterDepth
		}
		out.Peers = append(out.Peers, peer)
	}

	reasons := m.routeReasons(as, now, pipe, lastSend, lastRecv, sendErr, out)
	if len(out.Peers) == 0 {
		out.Peers = nil
	}
	out.Reasons = reasons

	playoutFresh := !pipe.LastPlayout.IsZero() && now.Sub(pipe.LastPlayout) < routeStatusStale
	sendFresh := !lastSend.IsZero() && now.Sub(lastSend) < routeStatusStale
	inputFresh := !pipe.LastInput.IsZero() && now.Sub(pipe.LastInput) < routeStatusStale
	dtxMuted := pipe.DTXState == "muted"

	if route.Direction == "receive" {
		out.Playing = playoutFresh
	} else {
		// A send route is healthy while it delivers, or while capture is
		// live and DTX intentionally suppresses a silent input.
		out.Playing = sendFresh || (dtxMuted && inputFresh)
	}

	if out.Playing {
		out.Summary = "playing"
		if dtxMuted && !playoutFresh {
			out.Summary = "playing (input silent, DTX muted)"
		}
	} else if len(reasons) > 0 {
		out.Summary = reasons[0]
	} else {
		out.Summary = "not playing"
	}
	return out, true
}

// routeReasons explains why the route is (or may be) not playing, ordered
// from the most likely first-thing-to-check to the least.
func (m *streamManager) routeReasons(
	as *activeStream,
	now time.Time,
	pipe streamer.RouteStatus,
	lastSend, lastRecv time.Time,
	sendErr string,
	out v1.RouteStatus,
) []string {
	var reasons []string
	add := func(format string, args ...any) {
		reasons = append(reasons, fmt.Sprintf(format, args...))
	}

	route := as.route
	hasInput := as.in != nil
	recvPath := route.Direction == "receive"

	// Capture side.
	if hasInput {
		if out.CaptureOverflows > 0 {
			add("capture device overflows: %d (audio dropped at the input; lower input latency or channel count)", out.CaptureOverflows)
		}
		if pipe.LastInput.IsZero() {
			add("capture has not delivered any audio yet")
		} else if now.Sub(pipe.LastInput) >= routeStatusStale {
			add("no audio from capture for %s", now.Sub(pipe.LastInput).Round(time.Millisecond))
		}
	}

	// Network / peer side.
	if recvPath {
		if len(pipe.Peers) == 0 {
			add("no audio received from any peer yet")
		} else if lastRecv.IsZero() {
			add("peer is connected but no audio packets have arrived yet")
		} else if now.Sub(lastRecv) >= routeStatusPeerGone {
			add("no audio from peer for %s (peer offline or route mismatched)", now.Sub(lastRecv).Round(time.Second))
		} else if now.Sub(lastRecv) >= routeStatusStale {
			add("no audio from peer for %s", now.Sub(lastRecv).Round(time.Millisecond))
		}
		if out.Underruns > 0 && !lastRecv.IsZero() && now.Sub(lastRecv) < routeStatusPeerGone {
			add("jitter buffer underruns: %d (packet loss or jitter exceeded the buffer)", out.Underruns)
		}
		if out.OutputUnderruns > 0 {
			add("output device underflows: %d (playback starved; check the output device)", out.OutputUnderruns)
		}
	}

	// Send side.
	if hasInput && !recvPath {
		if route.PeerID != "" && route.PeerID != "local" && m.disc != nil {
			if m.disc.PeerByID(route.PeerID) == nil {
				add("peer %q is not discovered (it must join the mesh and advertise a stream address)", route.PeerID)
			}
		}
		if len(pipe.Peers) == 0 {
			add("no active send destination (peer has not advertised a stream address)")
		} else if lastSend.IsZero() {
			add("no audio has been sent yet")
		} else if now.Sub(lastSend) >= routeStatusStale {
			if sendErr != "" {
				add("send failing: %s", sendErr)
			} else if pipe.DTXState == "muted" {
				add("input is silent: DTX is suppressing packets (not a fault)")
			} else {
				add("no audio sent for %s", now.Sub(lastSend).Round(time.Millisecond))
			}
		} else if sendErr != "" {
			add("intermittent send errors: %s", sendErr)
		}
	}

	return reasons
}

func routeDirection(as *activeStream) string {
	if as.route.Direction == "receive" {
		return "receive"
	}
	if as.out != nil {
		return "sendrecv" // sends over the network and plays a local copy
	}
	return "send"
}

func peerRole(d streamer.StreamDirection) string {
	switch d {
	case streamer.DirRecvOnly:
		return "receive"
	case streamer.DirSendOnly:
		return "send"
	default:
		return "sendrecv"
	}
}
