package daemon

import (
	"flag"
	"testing"

	"github.com/KoukeNeko/JingClaw/core/internal/config"
)

func overridesBy(t *testing.T, args []string, cfg config.Config) map[string]string {
	t.Helper()

	flags := flag.NewFlagSet("test", flag.ContinueOnError)
	flags.String("provider", "", "")
	flags.String("model", "", "")
	flags.String("addr", "", "")
	flags.Int("max-iterations", 0, "")
	flags.Bool("dev-fake", false, "")
	if err := flags.Parse(args); err != nil {
		t.Fatal(err)
	}

	got := make(map[string]string)
	for _, held := range settingsHeldFromTheFile(flags, cfg) {
		got[held.Key] = held.Source + ":" + held.Name
	}
	return got
}

// A setting the file cannot decide has to be said, or the page offers an edit
// that does nothing.
func TestSettingsTheEnvironmentAndFlagsDecideAreReported(t *testing.T) {
	t.Setenv("JINGCLAW_PROVIDER_BACKEND", "anthropic")
	t.Setenv("JINGCLAW_SERVER_LOG_LEVEL", "debug")

	cfg := config.Defaults()
	cfg.Provider.Backend = "anthropic"

	got := overridesBy(t, []string{"--model", "m", "--max-iterations", "3"}, cfg)

	want := map[string]string{
		"provider.backend":         "environment:JINGCLAW_PROVIDER_BACKEND",
		"server.log_level":         "environment:JINGCLAW_SERVER_LOG_LEVEL",
		"provider.anthropic.model": "flag:--model",
		"agent.max_iterations":     "flag:--max-iterations",
	}
	for key, name := range want {
		if got[key] != name {
			t.Errorf("%s is %q, want %q", key, got[key], name)
		}
	}
	if len(got) != len(want) {
		t.Errorf("reported %v", got)
	}
}

func TestAFlagIsTheReasonWhenItAndTheEnvironmentBothSetOne(t *testing.T) {
	t.Setenv("JINGCLAW_PROVIDER_BACKEND", "anthropic")

	got := overridesBy(t, []string{"--provider", "ollama"}, config.Defaults())

	if got["provider.backend"] != "flag:--provider" {
		t.Errorf("got %q", got["provider.backend"])
	}
}

// Nothing is held when nothing is set, so the page offers everything.
func TestNothingIsHeldWhenNothingIsSet(t *testing.T) {
	if got := overridesBy(t, nil, config.Defaults()); len(got) != 0 {
		t.Errorf("reported %v", got)
	}
}
