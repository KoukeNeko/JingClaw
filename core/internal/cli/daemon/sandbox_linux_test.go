//go:build linux

package daemon

import (
	"log/slog"
	"path/filepath"
	"strings"
	"testing"

	"github.com/KoukeNeko/JingClaw/core/internal/config"
	"github.com/KoukeNeko/JingClaw/core/internal/home"
)

// Turning the sandbox on must stop the daemon at startup on Linux, not leave
// it running with an exec_command that refuses everything.
//
// The deployment directory is always hidden from a confined command, and
// landlock cannot hide a directory. Network is allowed here so that, on a
// machine that has landlock, that is the only thing left to refuse; on one
// without it the daemon is refused for having no backend, which is also the
// startup refusal this is about.
func TestEnablingTheSandboxOnLinuxIsRefusedAtStartup(t *testing.T) {
	root := filepath.Join(t.TempDir(), home.DirName)
	if _, err := home.Create(root); err != nil {
		t.Fatalf("create: %v", err)
	}
	t.Setenv(home.EnvVar, root)

	cfg := config.Config{Sandbox: config.Sandbox{Enabled: true, Network: true}}

	confine, err := confinement(cfg, slog.New(slog.DiscardHandler))
	if err == nil {
		t.Fatalf("the daemon started with a sandbox that refuses every command: %+v", confine)
	}
	if !strings.Contains(err.Error(), "[sandbox]") {
		t.Errorf("the refusal does not say which setting: %v", err)
	}
	t.Logf("ok   the daemon refuses to start: %v", err)
}
