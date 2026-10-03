package urn

import (
	"strings"
)

// Locality represents the topological locality of an audio endpoint relative to a node.
type Locality string

const (
	LocalityLocal  Locality = "local"
	LocalityRemote Locality = "remote"
)

// ClassifyDeviceLocality inspects a canonical RFC 8141 device URN or legacy identifier
// and determines whether it resides on the local node or on a remote cluster peer.
//
// Rules:
//  1. Physical hardware (urn:audio:hw:...) and virtual software endpoints (urn:audio:virt:...)
//     are host-bound and classified as LocalityLocal with localNodeID.
//  2. Legacy / plain identifiers (e.g. "default", "hw:0,0", "mic") are treated as LocalityLocal with localNodeID.
//  3. Cluster mesh proxy endpoints (urn:audio:node:<nodeID>:dev:...) are classified as:
//     - LocalityLocal with localNodeID if nodeID == localNodeID, nodeID == "local", or nodeID == "".
//     - LocalityRemote with the parsed nodeID if nodeID != localNodeID.
func ClassifyDeviceLocality(rawURN string, localNodeID string) (Locality, string) {
	trimmed := strings.TrimSpace(rawURN)
	if trimmed == "" {
		return LocalityLocal, localNodeID
	}

	// 1. Cluster mesh proxy URN: urn:audio:node:<nodeID>:dev:<local_urn>
	if strings.HasPrefix(trimmed, "urn:audio:node:") {
		rest := strings.TrimPrefix(trimmed, "urn:audio:node:")
		devIdx := strings.Index(rest, ":dev:")
		var nodeID string
		if devIdx >= 0 {
			nodeID = rest[:devIdx]
		} else {
			// Malformed or incomplete node URN: take remainder up to next colon or end
			colonIdx := strings.Index(rest, ":")
			if colonIdx >= 0 {
				nodeID = rest[:colonIdx]
			} else {
				nodeID = rest
			}
		}

		nodeID = strings.TrimSpace(nodeID)
		if nodeID == "" || nodeID == "local" || nodeID == localNodeID {
			return LocalityLocal, localNodeID
		}
		return LocalityRemote, nodeID
	}

	// 2. Physical hardware or virtual endpoints
	if strings.HasPrefix(trimmed, "urn:audio:hw:") || strings.HasPrefix(trimmed, "urn:audio:virt:") {
		return LocalityLocal, localNodeID
	}

	// 3. Legacy or plain device identifier
	return LocalityLocal, localNodeID
}

// IsLocalEndpoint reports whether the given device identifier is local to localNodeID.
func IsLocalEndpoint(rawURN string, localNodeID string) bool {
	loc, _ := ClassifyDeviceLocality(rawURN, localNodeID)
	return loc == LocalityLocal
}
