package discovery

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/audio-router-p2p/internal/config"
)

func TestValidateNodeID(t *testing.T) {
	tests := []struct {
		name    string
		id      string
		wantErr bool
	}{
		{"valid standard hex", "0123456789abcdef0123456789abcdef", false},
		{"valid minimum length 8", "node-123", false},
		{"valid maximum length 64", strings.Repeat("a", 64), false},
		{"valid alphanumeric underscore hyphen", "Node_123-abc", false},
		{"too short (7 chars)", "node-12", true},
		{"empty", "", true},
		{"too long (65 chars)", strings.Repeat("a", 65), true},
		{"invalid char space", "node 12345", true},
		{"invalid char slash", "node/12345", true},
		{"invalid char colon", "node:12345", true},
		{"invalid char special", "node$12345", true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := ValidateNodeID(tt.id)
			if (err != nil) != tt.wantErr {
				t.Errorf("ValidateNodeID(%q) error = %v, wantErr = %v", tt.id, err, tt.wantErr)
			}
		})
	}
}

func TestResolveOrPersistNodeID_ConfigOverride(t *testing.T) {
	cfg := &config.Config{
		NodeID:     "explicit-cli-node-id-1",
		NodeIDFile: filepath.Join(t.TempDir(), "node.id"),
	}

	id, err := ResolveOrPersistNodeID(cfg)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if id != "explicit-cli-node-id-1" {
		t.Errorf("expected explicit-cli-node-id-1, got %q", id)
	}
	if cfg.NodeID != "explicit-cli-node-id-1" {
		t.Errorf("expected cfg.NodeID to remain set, got %q", cfg.NodeID)
	}

	// File must not be created since CLI flag was provided
	if _, err := os.Stat(cfg.NodeIDFile); !os.IsNotExist(err) {
		t.Errorf("expected file %s not to exist, stat err: %v", cfg.NodeIDFile, err)
	}
}

func TestResolveOrPersistNodeID_EnvOverride(t *testing.T) {
	origEnv := os.Getenv("AUDIO_ROUTER_NODE_ID")
	defer func() {
		_ = os.Setenv("AUDIO_ROUTER_NODE_ID", origEnv)
	}()

	envVal := "node-from-env-var-99"
	_ = os.Setenv("AUDIO_ROUTER_NODE_ID", envVal)

	cfg := &config.Config{
		NodeIDFile: filepath.Join(t.TempDir(), "node.id"),
	}

	id, err := ResolveOrPersistNodeID(cfg)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if id != envVal {
		t.Errorf("expected %q, got %q", envVal, id)
	}
	if cfg.NodeID != envVal {
		t.Errorf("expected cfg.NodeID %q, got %q", envVal, cfg.NodeID)
	}

	// File should not be created when env var is used
	if _, err := os.Stat(cfg.NodeIDFile); !os.IsNotExist(err) {
		t.Errorf("expected file %s not to exist", cfg.NodeIDFile)
	}

	// CLI flag takes precedence over env var
	cfg.NodeID = "cli-takes-precedence"
	id, err = ResolveOrPersistNodeID(cfg)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if id != "cli-takes-precedence" {
		t.Errorf("expected 'cli-takes-precedence', got %q", id)
	}
}

func TestResolveOrPersistNodeID_FileRead(t *testing.T) {
	origEnv := os.Getenv("AUDIO_ROUTER_NODE_ID")
	_ = os.Unsetenv("AUDIO_ROUTER_NODE_ID")
	defer func() {
		if origEnv != "" {
			_ = os.Setenv("AUDIO_ROUTER_NODE_ID", origEnv)
		}
	}()

	tmpDir := t.TempDir()
	idFile := filepath.Join(tmpDir, "node.id")
	preExistingID := "existing-persisted-node-id-42"
	if err := os.WriteFile(idFile, []byte(preExistingID+"\n"), 0600); err != nil {
		t.Fatalf("write file: %v", err)
	}

	cfg := &config.Config{
		NodeIDFile: idFile,
	}

	id, err := ResolveOrPersistNodeID(cfg)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if id != preExistingID {
		t.Errorf("expected %q, got %q", preExistingID, id)
	}
	if cfg.NodeID != preExistingID {
		t.Errorf("expected cfg.NodeID %q, got %q", preExistingID, cfg.NodeID)
	}
}

func TestResolveOrPersistNodeID_GenerationAndAtomicPersistence(t *testing.T) {
	origEnv := os.Getenv("AUDIO_ROUTER_NODE_ID")
	_ = os.Unsetenv("AUDIO_ROUTER_NODE_ID")
	defer func() {
		if origEnv != "" {
			_ = os.Setenv("AUDIO_ROUTER_NODE_ID", origEnv)
		}
	}()

	tmpDir := t.TempDir()
	idFile := filepath.Join(tmpDir, "sub", "dir", "node.id")

	cfg := &config.Config{
		NodeIDFile: idFile,
	}

	// 1. Initial generation
	id1, err := ResolveOrPersistNodeID(cfg)
	if err != nil {
		t.Fatalf("first ResolveOrPersistNodeID failed: %v", err)
	}
	if len(id1) != 32 {
		t.Fatalf("expected 32 hex chars, got %d (%q)", len(id1), id1)
	}
	if err := ValidateNodeID(id1); err != nil {
		t.Fatalf("generated ID is invalid: %v", err)
	}

	// Verify file was written and has 0600 permissions
	fi, err := os.Stat(idFile)
	if err != nil {
		t.Fatalf("stat idFile failed: %v", err)
	}
	perm := fi.Mode().Perm()
	if perm != 0600 {
		t.Errorf("expected 0600 permissions, got %#o", perm)
	}

	fileContent, err := os.ReadFile(idFile)
	if err != nil {
		t.Fatalf("read idFile failed: %v", err)
	}
	if strings.TrimSpace(string(fileContent)) != id1 {
		t.Errorf("file content %q does not match returned id %q", strings.TrimSpace(string(fileContent)), id1)
	}

	// 2. Second boot simulation (new cfg, pointing to same file)
	cfg2 := &config.Config{
		NodeIDFile: idFile,
	}
	id2, err := ResolveOrPersistNodeID(cfg2)
	if err != nil {
		t.Fatalf("second ResolveOrPersistNodeID failed: %v", err)
	}
	if id2 != id1 {
		t.Fatalf("expected persistent id across boots (%q), got %q", id1, id2)
	}
	if cfg2.NodeID != id1 {
		t.Errorf("expected cfg2.NodeID = %q, got %q", id1, cfg2.NodeID)
	}
}

func TestResolveOrPersistNodeID_ReadOnlyFilesystemFallback(t *testing.T) {
	origEnv := os.Getenv("AUDIO_ROUTER_NODE_ID")
	_ = os.Unsetenv("AUDIO_ROUTER_NODE_ID")
	defer func() {
		if origEnv != "" {
			_ = os.Setenv("AUDIO_ROUTER_NODE_ID", origEnv)
		}
	}()

	// Point to an impossible directory path (e.g., under a non-existent root or dev null)
	impossibleFile := "/proc/sys/fs/cannot-write-here-node.id"
	cfg := &config.Config{
		NodeIDFile: impossibleFile,
	}

	// Should NOT return error, but generate a valid ID and log a warning
	id, err := ResolveOrPersistNodeID(cfg)
	if err != nil {
		t.Fatalf("expected graceful fallback on write failure, got error: %v", err)
	}
	if len(id) != 32 {
		t.Errorf("expected 32 hex chars, got %d (%q)", len(id), id)
	}
	if cfg.NodeID != id {
		t.Errorf("expected cfg.NodeID %q, got %q", id, cfg.NodeID)
	}
}

func TestResolveOrPersistNodeID_InvalidFileContent(t *testing.T) {
	origEnv := os.Getenv("AUDIO_ROUTER_NODE_ID")
	_ = os.Unsetenv("AUDIO_ROUTER_NODE_ID")
	defer func() {
		if origEnv != "" {
			_ = os.Setenv("AUDIO_ROUTER_NODE_ID", origEnv)
		}
	}()

	tmpDir := t.TempDir()
	idFile := filepath.Join(tmpDir, "node.id")
	_ = os.WriteFile(idFile, []byte("bad node id with spaces!\n"), 0600)

	cfg := &config.Config{
		NodeIDFile: idFile,
	}
	_, err := ResolveOrPersistNodeID(cfg)
	if err == nil {
		t.Fatal("expected error on invalid file content, got nil")
	}
}
