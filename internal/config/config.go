package config

import (
	"encoding/json"
	"flag"
	"fmt"
	"time"
)

type Config struct {
	ListenAddr  string
	CertFile    string
	KeyFile     string
	AuthToken   string
	UseEmulator bool // true when running with the -use-emulator flag or the noaudio build
	Encrypt     bool   // enable DTLS/SRTP encryption for audio streams
	PSK         string // Pre-shared key for DTLS (base64 encoded)
	RoutesFile   string // Path to the JSON routes file

	MulticastGroup   string
	MulticastPort    int
	DiscoveryPort    int
	SyncPort         int
	StreamPort       string
	AdvHost          string
	ElectionPort     int                // dedicated election socket port
	AnnounceInterval time.Duration
	PeerTTL          time.Duration
	SyncInterval     time.Duration
	ShutdownTimeout  time.Duration

	// StaticPeers for cross-broadcast domain connectivity (manual peer config)
	StaticPeers []StaticPeerConfig
}

type StaticPeerConfig struct {
	ID         string `json:"id"`          // peer ID
	Addr       string `json:"addr"`        // discovery/control address host:port
	StreamAddr string `json:"streamAddr"`  // audio stream address host:port
	SyncAddr   string `json:"syncAddr"`    // clock sync address host:port
}

func Load() (*Config, error) {
	cfg := &Config{}
	flag.StringVar(&cfg.ListenAddr, "listen", ":8443", "HTTP listen address (TLS)")
	flag.StringVar(&cfg.CertFile, "cert", "cert.pem", "TLS certificate file")
	flag.StringVar(&cfg.KeyFile, "key", "key.pem", "TLS key file")
	flag.StringVar(&cfg.AuthToken, "auth-token", "", "Authentication token for the API and peer handshakes")
	flag.BoolVar(&cfg.UseEmulator, "use-emulator", false, "Use the audio emulator instead of real devices")
	flag.BoolVar(&cfg.Encrypt, "encrypt", false, "Enable DTLS/SRTP encryption for audio streams")
	flag.StringVar(&cfg.PSK, "psk", "", "Pre-shared key for DTLS encryption (base64 encoded)")
	flag.StringVar(&cfg.RoutesFile, "routes", "routes.json", "Path to the JSON routes file")

	flag.StringVar(&cfg.MulticastGroup, "multicast-group", "239.255.0.1", "Multicast group for peer discovery")
	flag.IntVar(&cfg.MulticastPort, "multicast-port", 45400, "Multicast port for peer announcements")
	flag.IntVar(&cfg.DiscoveryPort, "discovery-port", 45401, "Node's unicast discovery/control port")
	flag.IntVar(&cfg.SyncPort, "sync-port", 45402, "Node's clock-sync UDP port")
	flag.IntVar(&cfg.ElectionPort, "election-port", 45403, "Node's election UDP port")
	flag.StringVar(&cfg.StreamPort, "stream-port", ":45678", "UDP port for audio streaming [host:]port")
	flag.StringVar(&cfg.AdvHost, "adv-host", "", "Host/IP to advertise to peers (default: auto-detect)")
	flag.DurationVar(&cfg.AnnounceInterval, "announce-interval", 3*time.Second, "Peer discovery announce interval")
	flag.DurationVar(&cfg.PeerTTL, "peer-ttl", 10*time.Second, "How long a peer entry lives without refresh")
	flag.DurationVar(&cfg.SyncInterval, "sync-interval", 2*time.Second, "Heartbeat/clock-sync interval")
	flag.DurationVar(&cfg.ShutdownTimeout, "shutdown-timeout", 5*time.Second, "Graceful shutdown timeout")

	// Static peers (comma-separated JSON array)
	staticPeersJSON := flag.String("static-peers", "", "Static peers JSON array (for cross-broadcast domains)")
	flag.Parse()

	if *staticPeersJSON != "" {
		if err := json.Unmarshal([]byte(*staticPeersJSON), &cfg.StaticPeers); err != nil {
			return nil, fmt.Errorf("parse static-peers: %w", err)
		}
	}

	if cfg.AuthToken == "" {
		return nil, fmt.Errorf("auth-token is required")
	}
	if cfg.AnnounceInterval <= 0 || cfg.PeerTTL <= 0 || cfg.SyncInterval <= 0 {
		return nil, fmt.Errorf("announce-interval, peer-ttl and sync-interval must be positive")
	}
	if cfg.ElectionPort <= 0 {
		cfg.ElectionPort = cfg.DiscoveryPort + 100
	}
	return cfg, nil
}
