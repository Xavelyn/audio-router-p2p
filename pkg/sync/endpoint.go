package sync

import (
	"net"
	"strconv"
	"sync"
	"time"

	"github.com/audio-router-p2p/pkg/discovery"
)

type cachedResolvedAddr struct {
	ip        net.IP
	port      int
	expiresAt time.Time
}

var (
	dnsCacheMu sync.RWMutex
	dnsCache   = make(map[string]cachedResolvedAddr)
)

const dnsCacheTTL = 30 * time.Second

// parseEndpoint attempts fast, allocation-light decomposition of "host:port" endpoints.
// If the host is a numeric IP, it avoids DNS resolution altogether.
func parseEndpoint(endpoint string) (net.IP, int, error) {
	host, portStr, err := net.SplitHostPort(endpoint)
	if err != nil {
		return nil, 0, err
	}

	port, err := strconv.Atoi(portStr)
	if err != nil || port <= 0 || port > 65535 {
		return nil, 0, net.InvalidAddrError("invalid port")
	}

	// Fast path: numeric IP
	if ip := net.ParseIP(host); ip != nil {
		return ip, port, nil
	}

	// Check resolution cache for hostnames
	dnsCacheMu.RLock()
	cached, found := dnsCache[endpoint]
	dnsCacheMu.RUnlock()

	now := time.Now()
	if found && now.Before(cached.expiresAt) {
		return cached.ip, cached.port, nil
	}

	// Fallback to DNS resolution for hostnames
	resolved, err := net.ResolveUDPAddr("udp4", endpoint)
	if err != nil {
		return nil, 0, err
	}

	dnsCacheMu.Lock()
	dnsCache[endpoint] = cachedResolvedAddr{
		ip:        resolved.IP,
		port:      resolved.Port,
		expiresAt: now.Add(dnsCacheTTL),
	}
	dnsCacheMu.Unlock()

	return resolved.IP, resolved.Port, nil
}

// syncAddrMatches compares an incoming source UDP socket with an advertised peer endpoint.
// It avoids DNS lookups for numeric IPv4/IPv6 addresses and caches hostname resolutions.
func syncAddrMatches(advertised string, source *net.UDPAddr) bool {
	if source == nil || advertised == "" {
		return false
	}

	ip, port, err := parseEndpoint(advertised)
	if err != nil {
		return false
	}

	return port == source.Port && ip.Equal(source.IP)
}

// resolvePeerID resolves a datagram source address to the ID of a discovered peer.
// The local node ID is excluded to prevent reflection loops.
func resolvePeerID(peers []discovery.Peer, localID string, addr *net.UDPAddr) (string, bool) {
	if addr == nil {
		return "", false
	}
	for _, p := range peers {
		if p.ID == "" || p.ID == localID || p.SyncAddr == "" {
			continue
		}
		if syncAddrMatches(p.SyncAddr, addr) {
			return p.ID, true
		}
	}
	return "", false
}
