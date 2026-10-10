package group

import (
	"fmt"
	"strconv"
	"strings"

	"github.com/audio-router-p2p/pkg/audio/urn"
)

// CanonicalEndpoint decomposes an audio endpoint identifier into its constituent routing facets.
type CanonicalEndpoint struct {
	Raw         string         `json:"raw"`
	Clean       string         `json:"clean"`
	Direction   string         `json:"direction,omitempty"` // "in", "out", or ""
	NodeID      string         `json:"node_id,omitempty"`   // parsed cluster node, if any
	DeviceID    string         `json:"device_id"`           // bare or normalized device identifier
	DeviceURN   *urn.DeviceURN `json:"device_urn,omitempty"`// parsed RFC 8141 URN, if applicable
	Slug        string         `json:"slug,omitempty"`      // alphanumeric slug
	Checksum    string         `json:"checksum,omitempty"`  // 8-character hex checksum, if present or computed
	IsLocalNode bool           `json:"is_local_node"`       // true if node is local or unspecified
}

var directionalPrefixes = []struct {
	prefix string
	dir    string
}{
	{"in:", "in"},
	{"out:", "out"},
	{"input:", "in"},
	{"output:", "out"},
	{"input-", "in"},
	{"output-", "out"},
	{"in-", "in"},
	{"out-", "out"},
}

// ParseCanonicalEndpoint decomposes an audio endpoint reference into its canonical components.
// Strips directional canvas tags ("in:", "out:", "input-", "output-"), extracts node qualifications,
// and parses RFC 8141 URNs and checksums.
func ParseCanonicalEndpoint(ref, localNodeID string) CanonicalEndpoint {
	trimmed := strings.TrimSpace(ref)
	if trimmed == "" {
		return CanonicalEndpoint{IsLocalNode: true}
	}

	clean := trimmed
	dir := ""

	// 1. Strip directional tags
	lower := strings.ToLower(clean)
	for _, dp := range directionalPrefixes {
		if strings.HasPrefix(lower, dp.prefix) {
			dir = dp.dir
			clean = strings.TrimSpace(clean[len(dp.prefix):])
			break
		}
	}

	var nodeID string
	var devPart string

	// 2. Extract node qualification
	if strings.HasPrefix(clean, "urn:audio:node:") {
		rest := strings.TrimPrefix(clean, "urn:audio:node:")
		devIdx := strings.Index(rest, ":dev:")
		if devIdx > 0 {
			nodeID = rest[:devIdx]
			devPart = rest[devIdx+len(":dev:"):]
		} else {
			devPart = clean
		}
	} else if !strings.HasPrefix(clean, "urn:") && strings.Contains(clean, "/") {
		slash := strings.Index(clean, "/")
		nodeID = clean[:slash]
		devPart = clean[slash+1:]
	} else {
		devPart = clean
	}

	nodeID = strings.TrimSpace(nodeID)
	devPart = strings.TrimSpace(devPart)

	isLocal := false
	if nodeID == "" || strings.EqualFold(nodeID, "local") || (localNodeID != "" && strings.EqualFold(nodeID, localNodeID)) {
		isLocal = true
	}

	ep := CanonicalEndpoint{
		Raw:         trimmed,
		Clean:       clean,
		Direction:   dir,
		NodeID:      nodeID,
		DeviceID:    devPart,
		IsLocalNode: isLocal,
	}

	// 3. Inspect devPart for RFC 8141 URN or name:checksum
	if strings.HasPrefix(devPart, "urn:audio:") {
		if parsed, err := urn.Parse(devPart); err == nil {
			ep.DeviceURN = parsed
			switch parsed.Scope {
			case urn.ScopeVirtual:
				ep.Slug = parsed.Slug
			case urn.ScopeHardware:
				switch parsed.Tier {
				case urn.TierName:
					ep.Slug = parsed.Slug
					ep.Checksum = parsed.Hash
				case urn.TierALSA:
					ep.Slug = fmt.Sprintf("hw-%s-%d", parsed.CardID, parsed.Device)
				case urn.TierUSB:
					ep.Slug = fmt.Sprintf("usb-%s-%s-%s-%d", parsed.VendorID, parsed.ProductID, parsed.Serial, parsed.Device)
				case urn.TierPath:
					ep.Slug = urn.Slugify(parsed.BusPath)
				}
			case urn.ScopeNode:
				if parsed.LocalURN != nil {
					ep.Slug = parsed.LocalURN.Slug
					ep.Checksum = parsed.LocalURN.Hash
				}
			}
		}
	}

	if ep.Slug == "" {
		// Check for slug:checksum8 format (e.g. "mic-1:a1b2c3d4")
		if colon := strings.LastIndex(devPart, ":"); colon > 0 {
			candidateHash := devPart[colon+1:]
			if isHex8(candidateHash) {
				ep.Slug = urn.Slugify(devPart[:colon])
				ep.Checksum = strings.ToLower(candidateHash)
			} else {
				ep.Slug = urn.Slugify(devPart)
			}
		} else {
			ep.Slug = urn.Slugify(devPart)
		}
	}

	if ep.Checksum == "" && devPart != "" {
		ep.Checksum = urn.Checksum(devPart)
	}

	return ep
}

func isHex8(s string) bool {
	if len(s) != 8 {
		return false
	}
	for i := 0; i < len(s); i++ {
		c := s[i]
		if !((c >= '0' && c <= '9') || (c >= 'a' && c <= 'f') || (c >= 'A' && c <= 'F')) {
			return false
		}
	}
	return true
}

// EndpointMatches evaluates symmetric canonical equivalence between a candidate endpoint
// and a target endpoint under ADR 0029 rules.
//
// Matches when:
//  1. Exact literal string equality (case-insensitive).
//  2. Node-qualified endpoints matching local node (e.g. "nodeA/mic-1" matches "mic-1" if local is "nodeA").
//  3. RFC 8141 URN matches ("urn:audio:node:nodeA:dev:mic-1" matches "mic-1" or "nodeA/mic-1").
//  4. Directional tags are stripped ("in:nodeA/mic-1" matches "nodeA/mic-1").
//  5. URN checksum equivalents ("urn:audio:hw:name:mic-1:1a2b3c4d" matches "mic-1").
//  6. ALSA aliases ("hw:0" matches "hw:0,0" or "urn:audio:hw:alsa:c=0:d=0").
//
// Returns false if nodes conflict (e.g. "nodeB/mic-1" does not match "mic-1" when local node is "nodeA").
func EndpointMatches(localNodeID, candidate, target string) bool {
	cStr := strings.TrimSpace(candidate)
	tStr := strings.TrimSpace(target)

	// 1. Exact string equality fast-path
	if cStr == tStr || strings.EqualFold(cStr, tStr) {
		return true
	}
	if cStr == "" || tStr == "" {
		return false
	}

	// 2. Parse canonical endpoints
	c := ParseCanonicalEndpoint(cStr, localNodeID)
	t := ParseCanonicalEndpoint(tStr, localNodeID)

	// Fast-path: cleaned strings match
	if strings.EqualFold(c.Clean, t.Clean) {
		return true
	}

	// 3. Node topological locality verification
	cNode := normalizeNode(c.NodeID, localNodeID)
	tNode := normalizeNode(t.NodeID, localNodeID)

	if localNodeID != "" {
		// Both endpoints resolve to an effective node
		if cNode != "" && tNode != "" && !strings.EqualFold(cNode, tNode) {
			return false
		}
	} else {
		// No local node ID known; if both explicitly specified different nodes, reject
		if c.NodeID != "" && t.NodeID != "" && !strings.EqualFold(c.NodeID, t.NodeID) {
			return false
		}
	}

	// 4. Device matching
	return deviceMatches(c, t)
}

func normalizeNode(nodeID, localNodeID string) string {
	nodeID = strings.TrimSpace(nodeID)
	if nodeID == "" || strings.EqualFold(nodeID, "local") {
		return localNodeID
	}
	return nodeID
}

func deviceMatches(c, t CanonicalEndpoint) bool {
	// Literal device match
	if strings.EqualFold(c.DeviceID, t.DeviceID) {
		return true
	}

	// Slug matching
	if c.Slug != "" && t.Slug != "" && strings.EqualFold(c.Slug, t.Slug) {
		return true
	}

	// One slugified equals the other's slug
	if c.Slug != "" && strings.EqualFold(c.Slug, urn.Slugify(t.DeviceID)) {
		return true
	}
	if t.Slug != "" && strings.EqualFold(t.Slug, urn.Slugify(c.DeviceID)) {
		return true
	}

	// Checksum match:
	// If both have explicit non-empty checksums (from URN or name:hash), compare them
	if c.Checksum != "" && t.Checksum != "" && strings.EqualFold(c.Checksum, t.Checksum) {
		return true
	}
	// Or if one is bare name whose SHA256 checksum matches the other's URN hash
	if c.Checksum != "" && strings.EqualFold(c.Checksum, urn.Checksum(t.DeviceID)) {
		return true
	}
	if t.Checksum != "" && strings.EqualFold(t.Checksum, urn.Checksum(c.DeviceID)) {
		return true
	}

	// Both are parsed URNs
	if c.DeviceURN != nil && t.DeviceURN != nil {
		if c.DeviceURN.Raw == t.DeviceURN.Raw {
			return true
		}
		if c.DeviceURN.Slug != "" && c.DeviceURN.Slug == t.DeviceURN.Slug {
			return true
		}
		if c.DeviceURN.Tier == urn.TierALSA && t.DeviceURN.Tier == urn.TierALSA {
			if c.DeviceURN.CardID == t.DeviceURN.CardID && c.DeviceURN.Device == t.DeviceURN.Device {
				return true
			}
		}
	}

	// One is a URN and the other is a bare alias or slug
	if c.DeviceURN != nil {
		if matchURNWithBare(c.DeviceURN, t.DeviceID) {
			return true
		}
	}
	if t.DeviceURN != nil {
		if matchURNWithBare(t.DeviceURN, c.DeviceID) {
			return true
		}
	}

	// ALSA hardware shorthand aliases: "hw:0" vs "hw:0,0" vs "0"
	if alsaMatches(c.DeviceID, t.DeviceID) {
		return true
	}

	// Stripped URN prefix matching
	cStripped := stripURNPrefixes(c.DeviceID)
	tStripped := stripURNPrefixes(t.DeviceID)
	if cStripped != "" && tStripped != "" {
		if strings.EqualFold(cStripped, tStripped) || strings.EqualFold(urn.Slugify(cStripped), urn.Slugify(tStripped)) {
			return true
		}
	}

	return false
}

func matchURNWithBare(u *urn.DeviceURN, bare string) bool {
	bare = strings.TrimSpace(bare)
	if bare == "" {
		return false
	}

	if u.Slug != "" && (strings.EqualFold(u.Slug, bare) || strings.EqualFold(u.Slug, urn.Slugify(bare))) {
		return true
	}

	if u.Tier == urn.TierALSA {
		if bare == fmt.Sprintf("hw:%s", u.CardID) ||
			bare == fmt.Sprintf("hw:%s,%d", u.CardID, u.Device) ||
			(u.Device == 0 && bare == u.CardID) {
			return true
		}
	}

	if u.Tier == urn.TierVirtual {
		slug := strings.TrimPrefix(u.Raw, "urn:audio:virt:")
		if strings.EqualFold(slug, bare) || strings.EqualFold(urn.Slugify(slug), urn.Slugify(bare)) {
			return true
		}
	}

	if u.Tier == urn.TierName {
		if u.Hash != "" && (strings.EqualFold(u.Hash, bare) || strings.EqualFold(u.Hash, urn.Checksum(bare))) {
			return true
		}
	}

	return false
}

func alsaMatches(a, b string) bool {
	cardA, devA, okA := parseALSA(a)
	cardB, devB, okB := parseALSA(b)
	if okA && okB {
		return cardA == cardB && devA == devB
	}
	return false
}

func parseALSA(s string) (card string, dev int, ok bool) {
	s = strings.TrimSpace(s)
	if strings.HasPrefix(s, "hw:") {
		rest := s[3:]
		comma := strings.Index(rest, ",")
		if comma > 0 {
			c := rest[:comma]
			dStr := rest[comma+1:]
			d, err := strconv.Atoi(dStr)
			if err == nil {
				return c, d, true
			}
			return c, 0, true
		}
		return rest, 0, true
	}
	// Single integer like "0" or "1"
	if id, err := strconv.Atoi(s); err == nil {
		return strconv.Itoa(id), 0, true
	}
	return "", 0, false
}

func stripURNPrefixes(s string) string {
	s = strings.TrimSpace(s)
	prefixes := []string{
		"urn:audio:virt:",
		"urn:audio:hw:name:",
		"urn:audio:hw:alsa:",
		"urn:audio:hw:usb:",
		"urn:audio:hw:path:",
		"urn:audio:hw:",
		"urn:audio:",
	}
	for _, p := range prefixes {
		if strings.HasPrefix(s, p) {
			s = strings.TrimPrefix(s, p)
			break
		}
	}
	return s
}
