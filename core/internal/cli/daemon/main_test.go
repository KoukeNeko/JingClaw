package daemon

import (
	"strings"
	"testing"

	"github.com/KoukeNeko/JingClaw/core/internal/config"
)

// A file written from the environment is not "all defaults".
//
// The line says where the settings came from, and on a container platform the
// answer is a variable somebody set on the service. Calling that defaults
// sends them looking for why their configuration was ignored, on the one run
// where it was applied for the first time.
func TestAConfigFromTheEnvironmentIsNotCalledDefaults(t *testing.T) {
	said := describeConfigFile("/data/config.toml", false, true)
	if strings.Contains(said, "defaults") {
		t.Errorf("a file written from %s is described as %q", config.FileEnvVar, said)
	}
	if !strings.Contains(said, config.FileEnvVar) {
		t.Errorf("it does not say where the settings came from: %q", said)
	}

	// And a file nobody supplied still reads the way it did.
	if said := describeConfigFile("/data/config.toml", true, false); !strings.Contains(said, "defaults") {
		t.Errorf("a file nobody supplied is described as %q", said)
	}
}

// The variable stays set on every later run, and only the first run created
// anything. A line that reported the variable rather than what this run did
// would claim, on every restart, to have just written a file it did not
// touch — while the file being read might be one somebody edited since.
func TestALaterRunDoesNotClaimToHaveWrittenTheFile(t *testing.T) {
	t.Setenv(config.FileEnvVar, "[provider]\nbackend = \"ollama\"\n")

	if said := describeConfigFile("/data/config.toml", false, false); said != "/data/config.toml" {
		t.Errorf("a run that created nothing says %q", said)
	}
}
