package config

import (
	"encoding/json"
	"flag"
	"fmt"
	"net"
	"os"
	"strconv"
	"strings"
	"time"
)

type Config struct {
	ListenAddr  string
	CertFile    string
	KeyFile     string
	NoTLS       bool   // disable TLS and serve plain HTTP
	AuthToken   string
	UseEmulator bool   // true when running with the -use-emulator flag or the noaudio build
	Encrypt     bool   // enable DTLS/SRTP encryption for audio streams
	PSK         string // Pre-shared key for DTLS (base64 encoded)
	RoutesFile  string // Path to the JSON routes file

	MulticastGroup   string
	MulticastPort    int
	DiscoveryPort    int
	SyncPort         int
	StreamPort       string
	AdvHost          string
	ElectionPort     int // dedicated election socket port
	AnnounceInterval time.Duration
	PeerTTL          time.Duration
	SyncInterval     time.Duration
	ShutdownTimeout  time.Duration

	// Room orchestration flags
	RoomID       string // Room ID to create/join
	RoomName     string // Human-readable room name
	RoomAutoJoin bool   // Auto-join the room on startup

	// StaticPeers for cross-broadcast domain connectivity (manual peer config)
	StaticPeers []StaticPeerConfig
}

type StaticPeerConfig struct {
	ID         string `json:"id"`         // peer ID
	Addr       string `json:"addr"`       // discovery/control address host:port
	StreamAddr string `json:"streamAddr"` // audio stream address host:port
	SyncAddr   string `json:"syncAddr"`   // clock sync address host:port
}

// Load loads configuration from command-line arguments.
func Load() (*Config, error) {
	return LoadFromArgs(os.Args[1:])
}

// LoadFromArgs parses configuration from a slice of command-line arguments.
func LoadFromArgs(args []string) (*Config, error) {
	cfg := &Config{}
	fs := flag.NewFlagSet("audio-router", flag.ContinueOnError)

	fs.StringVar(&cfg.ListenAddr, "listen", ":8443", "HTTP listen address (TLS or plaintext)")
	fs.StringVar(&cfg.CertFile, "cert", "cert.pem", "TLS certificate file")
	fs.StringVar(&cfg.KeyFile, "key", "key.pem", "TLS key file")
	fs.BoolVar(&cfg.NoTLS, "no-tls", false, "Disable TLS and serve plain HTTP")
	fs.StringVar(&cfg.AuthToken, "auth-token", "", "Authentication token for the API and peer handshakes")
	fs.BoolVar(&cfg.UseEmulator, "use-emulator", false, "Use the audio emulator instead of real devices")
	fs.BoolVar(&cfg.Encrypt, "encrypt", false, "Enable DTLS/SRTP encryption for audio streams")
	fs.StringVar(&cfg.PSK, "psk", "", "Pre-shared key for DTLS encryption (base64 encoded)")
	fs.StringVar(&cfg.RoutesFile, "routes", "routes.json", "Path to the JSON routes file")

	fs.StringVar(&cfg.MulticastGroup, "multicast-group", "239.255.0.1", "Multicast group for peer discovery")
	fs.IntVar(&cfg.MulticastPort, "multicast-port", 45400, "Multicast port for peer announcements")
	fs.IntVar(&cfg.DiscoveryPort, "discovery-port", 45401, "Node's unicast discovery/control port")
	fs.IntVar(&cfg.SyncPort, "sync-port", 45402, "Node's clock-sync UDP port")
	fs.IntVar(&cfg.ElectionPort, "election-port", 45403, "Node's election UDP port")
	fs.StringVar(&cfg.StreamPort, "stream-port", ":45678", "UDP port for audio streaming [host:]port")
	fs.StringVar(&cfg.AdvHost, "adv-host", "", "Host/IP to advertise to peers (default: auto-detect)")
	fs.DurationVar(&cfg.AnnounceInterval, "announce-interval", 3*time.Second, "Peer discovery announce interval")
	fs.DurationVar(&cfg.PeerTTL, "peer-ttl", 10*time.Second, "How long a peer entry lives without refresh")
	fs.DurationVar(&cfg.SyncInterval, "sync-interval", 2*time.Second, "Heartbeat/clock-sync interval")
	fs.DurationVar(&cfg.ShutdownTimeout, "shutdown-timeout", 5*time.Second, "Graceful shutdown timeout")

	fs.StringVar(&cfg.RoomID, "room-id", "", "Room ID to create/join")
	fs.StringVar(&cfg.RoomName, "room-name", "", "Human-readable name of the room")
	fs.BoolVar(&cfg.RoomAutoJoin, "room-auto-join", false, "Auto-join room on daemon startup")

	// Static peers (comma-separated JSON array or comma-separated addresses)
	staticPeersJSON := fs.String("static-peers", "", "Static peers JSON array (for cross-broadcast domains)")
	peersFlag := fs.String("peers", "", "Comma-separated list of peer discovery addresses (e.g. 10.1.10.160:45401 or 10.1.10.160) for direct unicast peering")

	if err := fs.Parse(args); err != nil {
		return nil, err
	}

	if *staticPeersJSON != "" {
		if err := json.Unmarshal([]byte(*staticPeersJSON), &cfg.StaticPeers); err != nil {
			return nil, fmt.Errorf("parse static-peers: %w", err)
		}
	}

	if *peersFlag != "" {
		for _, raw := range strings.Split(*peersFlag, ",") {
			raw = strings.TrimSpace(raw)
			if raw == "" {
				continue
			}
			host, port, err := net.SplitHostPort(raw)
			if err != nil {
				host = raw
				port = strconv.Itoa(cfg.DiscoveryPort)
			}
			addr := net.JoinHostPort(host, port)
			cfg.StaticPeers = append(cfg.StaticPeers, StaticPeerConfig{
				Addr: addr,
			})
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
