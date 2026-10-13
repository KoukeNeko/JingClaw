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

	confine, err := confinement(cfg, filepath.Join(root, home.DataName, "jingclaw.db"), slog.New(slog.DiscardHandler))
	if err == nil {
		t.Fatalf("the daemon started with a sandbox that refuses every command: %+v", confine)
	}
	if !strings.Contains(err.Error(), "[sandbox]") {
		t.Errorf("the refusal does not say which setting: %v", err)
	}
	t.Logf("ok   the daemon refuses to start: %v", err)
}

// The same refusal, seen from outside: the daemon is not gone, it is up
// without an agent and says which setting asked for what this machine cannot
// give. It used to be the last line before an exit.
func TestEnablingTheSandboxOnLinuxStartsARepairDaemonThatNamesTheSetting(t *testing.T) {
	root := isolate(t)
	t.Setenv(config.FileEnvVar, "[sandbox]\nenabled = true\nnetwork = true\n")

	d := start(t, root)

	state := d.state()
	if state.Mode != "repair" || len(state.Problems) != 1 {
		t.Fatalf("state is %+v", state)
	}
	if reason := state.Problems[0]; reason.Source != "startup" || !strings.Contains(reason.Why, "[sandbox]") {
		t.Errorf("the reason is %+v, want the refusal that names the setting", reason)
	}
}
