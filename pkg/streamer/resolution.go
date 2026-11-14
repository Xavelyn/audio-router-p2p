package streamer

import (
	"fmt"
	"net"
	"strings"

	"github.com/audio-router-p2p/pkg/discovery"
	"github.com/audio-router-p2p/pkg/router"
)

// ResolvePeerDestination maps a route destination into a validated UDP/DTLS host:port address.
func ResolvePeerDestination(sr router.StreamRoute, disc *discovery.Discovery, defaultPort string) (string, error) {
	if sr.PeerAddr != "" {
		if _, _, err := net.SplitHostPort(sr.PeerAddr); err == nil {
			return sr.PeerAddr, nil
		}
	}

	peerID := sr.PeerID
	if peerID == "" || peerID == "local" {
		return "", fmt.Errorf("local route does not require peer address resolution")
	}

	if disc == nil {
		return "", fmt.Errorf("discovery subsystem required for cross-node resolution")
	}

	peer := disc.PeerByID(peerID)
	if peer == nil {
		return "", fmt.Errorf("peer %q not found in active discovery table", peerID)
	}

	if peer.StreamAddr != "" {
		if _, _, err := net.SplitHostPort(peer.StreamAddr); err == nil {
			return peer.StreamAddr, nil
		}
	}

	// Fallback: derive stream address from discovery unicast address and default stream port
	host, _, err := net.SplitHostPort(peer.Addr)
	if err != nil {
		host = peer.Addr
	}
	if host != "" {
		port := strings.TrimPrefix(defaultPort, ":")
		if port == "" {
			port = "45678"
		}
		res := net.JoinHostPort(host, port)
		if _, _, err := net.SplitHostPort(res); err == nil {
			return res, nil
		}
	}
	return "", fmt.Errorf("unable to resolve stream address for peer %q", peerID)
}
