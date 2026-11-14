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
//  1. Canonical slash syntax (<node_id>/<device_id>) is classified as:
//     - LocalityLocal with localNodeID if nodeID == localNodeID, nodeID == "local", or nodeID == "".
//     - LocalityRemote with the parsed nodeID if nodeID != localNodeID.
//  2. Cluster mesh proxy endpoints (urn:audio:node:<nodeID>:dev:...) are classified as:
//     - LocalityLocal with localNodeID if nodeID == localNodeID, nodeID == "local", or nodeID == "".
//     - LocalityRemote with the parsed nodeID if nodeID != localNodeID.
//  3. Physical hardware (urn:audio:hw:...) and virtual software endpoints (urn:audio:virt:...)
//     are host-bound and classified as LocalityLocal with localNodeID.
//  4. Legacy / plain identifiers (e.g. "default", "hw:0,0", "mic") are treated as LocalityLocal with localNodeID.
func ClassifyDeviceLocality(rawURN string, localNodeID string) (Locality, string) {
	trimmed := strings.TrimSpace(rawURN)
	if trimmed == "" {
		return LocalityLocal, localNodeID
	}

	// 1. Canonical slash syntax: <node_id>/<device_id>
	if !strings.HasPrefix(trimmed, "urn:") {
		if slash := strings.Index(trimmed, "/"); slash > 0 {
			nodeID := strings.TrimSpace(trimmed[:slash])
			devID := strings.TrimSpace(trimmed[slash+1:])
			if devID != "" && nodeID != "" && nodeID != "local" && (localNodeID == "" || nodeID != localNodeID) {
				return LocalityRemote, nodeID
			}
			return LocalityLocal, localNodeID
		}
	}

	// 2. Cluster mesh proxy URN: urn:audio:node:<nodeID>:dev:<local_urn>
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
