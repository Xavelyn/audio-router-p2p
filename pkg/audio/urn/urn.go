package urn

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"strconv"
	"strings"
)

// Sentinel errors for URN parsing and validation.
var (
	ErrInvalidURN    = errors.New("invalid audio device URN")
	ErrInvalidScope  = errors.New("invalid URN scope")
	ErrInvalidTier   = errors.New("invalid URN hardware tier")
	ErrInvalidFormat = errors.New("malformed URN format")
)

// Scope defines the administrative namespace of an audio device URN.
type Scope string

const (
	ScopeHardware Scope = "hw"
	ScopeVirtual  Scope = "virt"
	ScopeNode     Scope = "node"
)

// Tier defines the resolution hierarchy for physical host audio hardware.
type Tier string

const (
	TierUSB     Tier = "usb"  // Tier 1: USB Vendor, Product, and Serial
	TierPath    Tier = "path" // Tier 2: Sysfs Physical Bus Path
	TierALSA    Tier = "alsa" // Tier 3: ALSA Card ID
	TierName    Tier = "name" // Tier 4: Slug + Hash Fallback
	TierVirtual Tier = "virt" // Virtual loopback
	TierNode    Tier = "node" // Cluster mesh proxy
)

// DeviceURN represents a structured, RFC 8141-compliant audio endpoint identifier.
type DeviceURN struct {
	Raw       string     `json:"raw"`
	Scope     Scope      `json:"scope"`
	Tier      Tier       `json:"tier"`
	VendorID  string     `json:"vendor_id,omitempty"`  // Tier 1 USB VID
	ProductID string     `json:"product_id,omitempty"` // Tier 1 USB PID
	Serial    string     `json:"serial,omitempty"`     // Tier 1 USB Serial
	BusPath   string     `json:"bus_path,omitempty"`   // Tier 2 Bus Path
	CardID    string     `json:"card_id,omitempty"`    // Tier 3 ALSA Card ID
	Slug      string     `json:"slug,omitempty"`       // Tier 4 / Virtual Slug
	Hash      string     `json:"hash,omitempty"`       // Tier 4 Checksum
	Device    int        `json:"device"`               // Subdevice index (defaults to 0)
	NodeID    string     `json:"node_id,omitempty"`    // Cluster Node ID
	LocalURN  *DeviceURN `json:"local_urn,omitempty"`  // Embedded Local URN for Node scope
}

// String returns the raw canonical URN representation.
func (u *DeviceURN) String() string {
	if u == nil {
		return ""
	}
	return u.Raw
}

// Slugify normalizes an arbitrary human-readable name into a lowercased,
// hyphen-delimited alphanumeric slug suitable for URNs.
func Slugify(name string) string {
	name = strings.TrimSpace(strings.ToLower(name))
	if name == "" {
		return "unknown"
	}
	var b strings.Builder
	lastWasDash := false
	for _, r := range name {
		if (r >= 'a' && r <= 'z') || (r >= '0' && r <= '9') {
			b.WriteRune(r)
			lastWasDash = false
		} else if r == '-' || r == '_' || r == ' ' || r == '.' || r == '/' || r == ':' || r == ',' {
			if !lastWasDash && b.Len() > 0 {
				b.WriteByte('-')
				lastWasDash = true
			}
		}
	}
	res := strings.Trim(b.String(), "-")
	if res == "" {
		return "device"
	}
	return res
}

// Checksum returns an 8-character lowercase hexadecimal hash computed over the input.
func Checksum(val string) string {
	h := sha256.Sum256([]byte(val))
	return hex.EncodeToString(h[:4])
}

// NewUSB constructs a Tier 1 USB Serial canonical device URN.
// Syntax: urn:audio:hw:usb:v=<vid>:p=<pid>:s=<serial>:d=<dev>
func NewUSB(vid, pid, serial string, dev int) string {
	return fmt.Sprintf("urn:audio:hw:usb:v=%s:p=%s:s=%s:d=%d",
		strings.ToLower(strings.TrimSpace(vid)),
		strings.ToLower(strings.TrimSpace(pid)),
		strings.TrimSpace(serial),
		dev,
	)
}

// NewPath constructs a Tier 2 Sysfs Physical Bus Path canonical device URN.
// Syntax: urn:audio:hw:path:<sysfs_bus_path>:d=<dev>
func NewPath(busPath string, dev int) string {
	return fmt.Sprintf("urn:audio:hw:path:%s:d=%d", strings.TrimSpace(busPath), dev)
}

// NewALSA constructs a Tier 3 ALSA Card Identifier canonical device URN.
// Syntax: urn:audio:hw:alsa:c=<card_id>:d=<dev>
func NewALSA(cardID string, dev int) string {
	return fmt.Sprintf("urn:audio:hw:alsa:c=%s:d=%d", strings.TrimSpace(cardID), dev)
}

// NewName constructs a Tier 4 Clean Name Slug + Hash fallback canonical device URN.
// Syntax: urn:audio:hw:name:<slug>:<hash8>
func NewName(name string) string {
	slug := Slugify(name)
	hash := Checksum(name)
	return fmt.Sprintf("urn:audio:hw:name:%s:%s", slug, hash)
}

// NewVirtual constructs a Virtual Software Loopback canonical device URN.
// Syntax: urn:audio:virt:<slug>
func NewVirtual(nameOrSlug string) string {
	slug := Slugify(nameOrSlug)
	return fmt.Sprintf("urn:audio:virt:%s", slug)
}

// NewNode constructs a Federated Cluster Mesh canonical device URN.
// Syntax: urn:audio:node:<node_id>:dev:<local_urn>
func NewNode(nodeID string, localURN string) string {
	return fmt.Sprintf("urn:audio:node:%s:dev:%s", strings.TrimSpace(nodeID), strings.TrimSpace(localURN))
}

// Parse inspects, validates, and unpacks an RFC 8141 audio device URN into a DeviceURN.
func Parse(raw string) (*DeviceURN, error) {
	raw = strings.TrimSpace(raw)
	if !strings.HasPrefix(raw, "urn:audio:") {
		return nil, fmt.Errorf("%w: must begin with 'urn:audio:'", ErrInvalidURN)
	}

	rest := strings.TrimPrefix(raw, "urn:audio:")

	// 1. Cluster Mesh: node:<node_id>:dev:<local_urn>
	if strings.HasPrefix(rest, "node:") {
		nodePart := strings.TrimPrefix(rest, "node:")
		devIdx := strings.Index(nodePart, ":dev:")
		if devIdx <= 0 {
			return nil, fmt.Errorf("%w: missing ':dev:' separator in node URN", ErrInvalidFormat)
		}
		nodeID := nodePart[:devIdx]
		localRaw := nodePart[devIdx+len(":dev:"):]
		if nodeID == "" || localRaw == "" {
			return nil, fmt.Errorf("%w: empty node_id or local_urn", ErrInvalidFormat)
		}
		localURN, err := Parse(localRaw)
		if err != nil {
			return nil, fmt.Errorf("%w: invalid nested local URN: %v", ErrInvalidURN, err)
		}
		return &DeviceURN{
			Raw:      raw,
			Scope:    ScopeNode,
			Tier:     TierNode,
			NodeID:   nodeID,
			LocalURN: localURN,
		}, nil
	}

	// 2. Virtual Loopback: virt:<slug>
	if strings.HasPrefix(rest, "virt:") {
		slug := strings.TrimPrefix(rest, "virt:")
		if slug == "" {
			return nil, fmt.Errorf("%w: empty virtual slug", ErrInvalidFormat)
		}
		return &DeviceURN{
			Raw:   raw,
			Scope: ScopeVirtual,
			Tier:  TierVirtual,
			Slug:  slug,
		}, nil
	}

	// 3. Physical Hardware: hw:...
	if strings.HasPrefix(rest, "hw:") {
		hwRest := strings.TrimPrefix(rest, "hw:")

		// Tier 1: usb:v=<vid>:p=<pid>:s=<serial>:d=<dev>
		if strings.HasPrefix(hwRest, "usb:") {
			parts := strings.Split(strings.TrimPrefix(hwRest, "usb:"), ":")
			var vid, pid, serial string
			var dev int
			var hasDev bool

			for _, p := range parts {
				kv := strings.SplitN(p, "=", 2)
				if len(kv) != 2 {
					return nil, fmt.Errorf("%w: invalid usb key-value parameter %q", ErrInvalidFormat, p)
				}
				key := kv[0]
				val := kv[1]
				switch key {
				case "v":
					vid = val
				case "p":
					pid = val
				case "s":
					serial = val
				case "d":
					d, err := strconv.Atoi(val)
					if err != nil {
						return nil, fmt.Errorf("%w: invalid device index %q", ErrInvalidFormat, val)
					}
					dev = d
					hasDev = true
				default:
					return nil, fmt.Errorf("%w: unrecognized usb attribute %q", ErrInvalidFormat, key)
				}
			}

			if vid == "" || pid == "" || serial == "" || !hasDev {
				return nil, fmt.Errorf("%w: usb URN requires v, p, s, and d attributes", ErrInvalidFormat)
			}

			return &DeviceURN{
				Raw:       raw,
				Scope:     ScopeHardware,
				Tier:      TierUSB,
				VendorID:  vid,
				ProductID: pid,
				Serial:    serial,
				Device:    dev,
			}, nil
		}

		// Tier 2: path:<sysfs_bus_path>:d=<dev>
		if strings.HasPrefix(hwRest, "path:") {
			pathPart := strings.TrimPrefix(hwRest, "path:")
			lastD := strings.LastIndex(pathPart, ":d=")
			if lastD <= 0 {
				return nil, fmt.Errorf("%w: path URN requires ':d=<dev>' suffix", ErrInvalidFormat)
			}
			busPath := pathPart[:lastD]
			devStr := pathPart[lastD+len(":d="):]
			dev, err := strconv.Atoi(devStr)
			if err != nil {
				return nil, fmt.Errorf("%w: invalid device index in path URN: %v", ErrInvalidFormat, err)
			}
			if busPath == "" {
				return nil, fmt.Errorf("%w: empty bus path", ErrInvalidFormat)
			}
			return &DeviceURN{
				Raw:     raw,
				Scope:   ScopeHardware,
				Tier:    TierPath,
				BusPath: busPath,
				Device:  dev,
			}, nil
		}

		// Tier 3: alsa:c=<card_id>:d=<dev>
		if strings.HasPrefix(hwRest, "alsa:") {
			alsaPart := strings.TrimPrefix(hwRest, "alsa:")
			parts := strings.Split(alsaPart, ":")
			var cardID string
			var dev int
			var hasDev bool

			for _, p := range parts {
				kv := strings.SplitN(p, "=", 2)
				if len(kv) != 2 {
					return nil, fmt.Errorf("%w: invalid alsa key-value parameter %q", ErrInvalidFormat, p)
				}
				switch kv[0] {
				case "c":
					cardID = kv[1]
				case "d":
					d, err := strconv.Atoi(kv[1])
					if err != nil {
						return nil, fmt.Errorf("%w: invalid device index in alsa URN: %v", ErrInvalidFormat, err)
					}
					dev = d
					hasDev = true
				default:
					return nil, fmt.Errorf("%w: unrecognized alsa attribute %q", ErrInvalidFormat, kv[0])
				}
			}

			if cardID == "" || !hasDev {
				return nil, fmt.Errorf("%w: alsa URN requires c and d attributes", ErrInvalidFormat)
			}

			return &DeviceURN{
				Raw:    raw,
				Scope:  ScopeHardware,
				Tier:   TierALSA,
				CardID: cardID,
				Device: dev,
			}, nil
		}

		// Tier 4: name:<slug>:<hash8>
		if strings.HasPrefix(hwRest, "name:") {
			namePart := strings.TrimPrefix(hwRest, "name:")
			lastColon := strings.LastIndex(namePart, ":")
			if lastColon <= 0 || lastColon == len(namePart)-1 {
				return nil, fmt.Errorf("%w: name URN requires '<slug>:<hash8>'", ErrInvalidFormat)
			}
			slug := namePart[:lastColon]
			hash := namePart[lastColon+1:]
			if len(hash) != 8 {
				return nil, fmt.Errorf("%w: name URN hash must be 8 characters, got %d", ErrInvalidFormat, len(hash))
			}
			return &DeviceURN{
				Raw:   raw,
				Scope: ScopeHardware,
				Tier:  TierName,
				Slug:  slug,
				Hash:  hash,
			}, nil
		}

		return nil, fmt.Errorf("%w: unknown hardware tier under hw:", ErrInvalidTier)
	}

	return nil, fmt.Errorf("%w: unknown scope %q", ErrInvalidScope, rest)
}
