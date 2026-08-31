package tui_test

import (
	"context"
	"os"
	"testing"

	"github.com/KoukeNeko/JingClaw/core/internal/tui"
)

// TestIsThePanel is this binary being the panel, for the checks above.
//
// A test rather than a separate program, so that what is put on a terminal is
// the package as it is built here — a fixture binary would be a second thing
// to keep in step with the first, and the failure it hides is the panel
// changing while the fixture keeps passing.
//
// It does nothing when the environment has not asked, so an ordinary run of
// this package is unaffected.
func TestIsThePanel(t *testing.T) {
	if os.Getenv("JINGCLAW_TUI_SKELETON") == "" {
		t.Skip("not being the panel")
	}

	err := tui.Run(context.Background(), tui.Options{
		PanicForTest: os.Getenv("JINGCLAW_TUI_PANIC") != "",
	})
	if err != nil {
		// Reported by exiting rather than by failing: the caller is a PTY
		// reading bytes, not a test runner reading a report.
		os.Exit(1)
	}
	os.Exit(0)
}
