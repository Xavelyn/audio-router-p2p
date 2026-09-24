package main

import (
	"github.com/audio-router-p2p/pkg/discovery"
	"github.com/audio-router-p2p/pkg/router"
)

// routesOf returns the distinct remote stream addresses referenced by the
// route table. A route may carry a direct peerAddr, or a peerId that must be
// resolved through discovery (the peer's advertised StreamAddr).
func routesOf(rt *router.Router, disc *discovery.Discovery) []string {
	var out []string
	seen := make(map[string]bool)
	for _, route := range rt.List() {
		addr := route.PeerAddr
		if addr == "" && route.PeerID != "" {
			if p := disc.PeerByID(route.PeerID); p != nil {
				addr = p.StreamAddr
			}
		}
		if addr != "" && !seen[addr] {
			seen[addr] = true
			out = append(out, addr)
		}
	}
	return out
}

// singleDTLSPeer returns the single distinct DTLS peer address, or "" unless
// the route table resolves to exactly one distinct peer. The DTLS/SRTP
// transport supports a single remote peer, so encrypt mode fails fast unless
// exactly one distinct peer address is used.
func singleDTLSPeer(addrs []string) string {
	if len(addrs) != 1 {
		return ""
	}
	return addrs[0]
}
