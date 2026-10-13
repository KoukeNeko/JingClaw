package daemon

import (
	"path/filepath"
	"strings"
	"testing"

	"github.com/KoukeNeko/JingClaw/core/internal/config"
)

// A missing interpreter says what to do about it.
//
// The daemon refuses to start, which is right — a deployment that silently
// dropped page reading would look like a model that will not use the tool.
// But "python3 is not on PATH" is the cause, not the remedy, and the operator
// reading it is often somewhere with no way to install anything: a container
// image carries what it carries. The line has to name the setting that runs
// without it.
func TestAMissingBrowserSaysHowToRunWithoutOne(t *testing.T) {
	cfg := config.Defaults()
	cfg.Web.Enabled = true
	cfg.Web.Backend = "browser"
	cfg.Web.Python = filepath.Join(t.TempDir(), "no-such-python3")

	_, err := webFetcher(cfg)
	if err == nil {
		t.Fatal("it accepted an interpreter that is not there")
	}
	for _, want := range []string{"web.enabled", "cloakbrowser"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("the failure does not mention %q: %v", want, err)
		}
	}
}
