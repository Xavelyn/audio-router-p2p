package discovery

import (
	crand "crypto/rand"
	"encoding/hex"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"strings"

	"github.com/audio-router-p2p/internal/config"
)

// ValidateNodeID ensures the node ID consists of 8-64 alphanumeric characters,
// underscores, or hyphens.
func ValidateNodeID(id string) error {
	if len(id) < 8 || len(id) > 64 {
		return fmt.Errorf("node ID %q length %d must be between 8 and 64 characters", id, len(id))
	}
	for i := 0; i < len(id); i++ {
		c := id[i]
		if (c >= 'a' && c <= 'z') || (c >= 'A' && c <= 'Z') || (c >= '0' && c <= '9') || c == '_' || c == '-' {
			continue
		}
		return fmt.Errorf("node ID %q contains invalid character %q (allowed: alphanumeric, _, -)", id, c)
	}
	return nil
}

func generateNodeID() (string, error) {
	var b [16]byte
	if _, err := crand.Read(b[:]); err != nil {
		return "", err
	}
	return hex.EncodeToString(b[:]), nil
}

// ResolveOrPersistNodeID resolves the persistent node identifier according to the
// priority hierarchy specified in ADR 0025:
// 1. Explicit CLI flag / config value (cfg.NodeID)
// 2. Environment variable (AUDIO_ROUTER_NODE_ID)
// 3. Persistent identity file (cfg.NodeIDFile, or AUDIO_ROUTER_NODE_ID_FILE, or "node.id")
// 4. Cryptographic generation (16 secure bytes / 32 hex chars) and atomic disk persistence.
//
// If disk write fails (e.g. read-only filesystem), it logs a warning and returns the generated
// ID without failing boot. It also mutates cfg.NodeID to the resolved ID if cfg is non-nil.
func ResolveOrPersistNodeID(cfg *config.Config) (string, error) {
	// 1. Check cfg.NodeID
	if cfg != nil && strings.TrimSpace(cfg.NodeID) != "" {
		id := strings.TrimSpace(cfg.NodeID)
		if err := ValidateNodeID(id); err != nil {
			return "", fmt.Errorf("invalid config NodeID: %w", err)
		}
		cfg.NodeID = id
		return id, nil
	}

	// 2. Check environment variable AUDIO_ROUTER_NODE_ID
	if envID := strings.TrimSpace(os.Getenv("AUDIO_ROUTER_NODE_ID")); envID != "" {
		if err := ValidateNodeID(envID); err != nil {
			return "", fmt.Errorf("invalid AUDIO_ROUTER_NODE_ID env: %w", err)
		}
		if cfg != nil {
			cfg.NodeID = envID
		}
		return envID, nil
	}

	// 3. Determine identity file path
	filePath := "node.id"
	if cfg != nil && strings.TrimSpace(cfg.NodeIDFile) != "" {
		filePath = strings.TrimSpace(cfg.NodeIDFile)
	} else if envFile := strings.TrimSpace(os.Getenv("AUDIO_ROUTER_NODE_ID_FILE")); envFile != "" {
		filePath = envFile
	}

	// 4. If file exists, read and validate
	data, err := os.ReadFile(filePath)
	if err == nil {
		id := strings.TrimSpace(string(data))
		if id != "" {
			if err := ValidateNodeID(id); err != nil {
				return "", fmt.Errorf("invalid node ID in file %s: %w", filePath, err)
			}
			if cfg != nil {
				cfg.NodeID = id
			}
			return id, nil
		}
	} else if !os.IsNotExist(err) {
		return "", fmt.Errorf("failed to read node ID file %s: %w", filePath, err)
	}

	// 5. If missing or empty, generate 16 cryptographically secure random bytes hex-encoded
	genID, err := generateNodeID()
	if err != nil {
		return "", fmt.Errorf("failed to generate secure node ID: %w", err)
	}

	// Write atomically using temp file + Sync() + os.Chmod(0600) + os.Rename
	if err := persistNodeIDAtomic(filePath, genID); err != nil {
		log.Printf("[DISCOVERY] Warning: failed to persist node ID to %s: %v", filePath, err)
	}

	if cfg != nil {
		cfg.NodeID = genID
	}
	return genID, nil
}

func persistNodeIDAtomic(filePath, id string) error {
	dir := filepath.Dir(filePath)
	if dir != "" && dir != "." {
		if err := os.MkdirAll(dir, 0755); err != nil {
			return fmt.Errorf("create directory %s: %w", dir, err)
		}
	}

	tmpFile, err := os.CreateTemp(dir, filepath.Base(filePath)+".tmp.*")
	if err != nil {
		return fmt.Errorf("create temp file: %w", err)
	}
	tmpName := tmpFile.Name()
	defer func() {
		_ = os.Remove(tmpName)
	}()

	if _, err := tmpFile.WriteString(id + "\n"); err != nil {
		_ = tmpFile.Close()
		return fmt.Errorf("write node ID: %w", err)
	}

	if err := tmpFile.Sync(); err != nil {
		_ = tmpFile.Close()
		return fmt.Errorf("sync temp file: %w", err)
	}

	if err := tmpFile.Chmod(0600); err != nil {
		_ = tmpFile.Close()
		return fmt.Errorf("chmod 0600 temp file: %w", err)
	}

	if err := tmpFile.Close(); err != nil {
		return fmt.Errorf("close temp file: %w", err)
	}

	if err := os.Rename(tmpName, filePath); err != nil {
		return fmt.Errorf("rename to target %s: %w", filePath, err)
	}

	_ = os.Chmod(filePath, 0600)
	return nil
}
