package discovery

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"runtime"

	"github.com/KoukeNeko/JingClaw/core/internal/home"
)

// ProtocolVersion identifies the RPC contract. A client that does not
// recognize it should refuse to talk rather than guess.
const ProtocolVersion = "jingclaw.control.v1"

// Discovery is what the daemon writes on startup so local clients can find it.
// The daemon listens on an ephemeral port, so this file — not a well-known
// port — is the rendezvous point.
type File struct {
	PID     int    `json:"pid"`
	BaseURL string `json:"base_url"`
	Token   string `json:"token"`

	// GatewayToken reaches the ingress and nothing else. It is written here so
	// a gateway started by the same operator can find it, and it is separate so
	// that handing it out never hands out the full control credential.
	GatewayToken    string `json:"gateway_token,omitempty"`
	ProtocolVersion string `json:"protocol_version"`
}

// discoveryFile is what the file is called wherever it is put.
const discoveryFile = "daemon.json"

// PathIn returns the discovery file's location, preferring an explicit
// directory.
//
// A configured directory matters for running more than one daemon on a machine:
// two instances sharing a discovery file would have the second silently steal
// every client from the first.
func PathIn(dir string) (string, error) {
	if dir != "" {
		return filepath.Join(dir, discoveryFile), nil
	}
	return Path()
}

// Path returns the location of the discovery file.
func Path() (string, error) {
	if dir := os.Getenv("JINGCLAW_RUNTIME_DIR"); dir != "" {
		return filepath.Join(dir, discoveryFile), nil
	}

	// A .JingClaw directory decides this on every platform: a client and the
	// daemon it is meant to find have to agree, and agreeing per-platform is
	// how they stop agreeing.
	if dir, found := home.FromWorkingDirectory(); found {
		return filepath.Join(dir.Run(), discoveryFile), nil
	}

	if runtime.GOOS != "darwin" {
		if dir := os.Getenv("XDG_RUNTIME_DIR"); dir != "" {
			return filepath.Join(dir, "jingclaw", discoveryFile), nil
		}
	}

	base, err := os.UserConfigDir()
	if err != nil {
		return "", fmt.Errorf("discovery: locate runtime dir: %w", err)
	}
	return filepath.Join(base, "JingClaw", "run", discoveryFile), nil
}

// WriteDiscovery persists the file with owner-only permissions. It holds the
// control token, so it must never be group or world readable.
func Write(path string, d File) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return fmt.Errorf("discovery: create runtime dir: %w", err)
	}

	payload, err := json.MarshalIndent(d, "", "  ")
	if err != nil {
		return fmt.Errorf("discovery: encode discovery: %w", err)
	}

	// Write then rename, so a client never reads a half-written file.
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, payload, 0o600); err != nil {
		return fmt.Errorf("discovery: write discovery: %w", err)
	}
	if err := os.Rename(tmp, path); err != nil {
		_ = os.Remove(tmp)
		return fmt.Errorf("discovery: install discovery: %w", err)
	}

	return nil
}

func Read(path string) (File, error) {
	var d File

	payload, err := os.ReadFile(path)
	if err != nil {
		return d, fmt.Errorf("discovery: read discovery: %w", err)
	}
	if err := json.Unmarshal(payload, &d); err != nil {
		return d, fmt.Errorf("discovery: decode discovery: %w", err)
	}
	if d.ProtocolVersion != ProtocolVersion {
		return d, fmt.Errorf("discovery: daemon speaks %q, client speaks %q", d.ProtocolVersion, ProtocolVersion)
	}

	return d, nil
}

// RemoveIfOwnedBy deletes the discovery file, unless it now describes some
// other process.
//
// A daemon shutting down must clean up after itself, but a daemon shutting
// down after a replacement has already started must not delete the
// replacement's file. Getting that wrong leaves a running daemon that no
// client can find, which looks from the outside like the daemon is down.
func RemoveIfOwnedBy(path string, pid int) error {
	found, err := Read(path)
	if err != nil {
		// Already gone, or unreadable: either way there is nothing here that
		// this process should be removing.
		return nil
	}
	if found.PID != pid {
		return nil
	}

	if err := os.Remove(path); err != nil && !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("discovery: remove %s: %w", path, err)
	}
	return nil
}
