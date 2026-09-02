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
	t.Setenv(config.FileEnvVar, "[provider]\nbackend = \"ollama\"\n")

	said := describeConfigFile("/data/config.toml", true)
	if strings.Contains(said, "defaults") {
		t.Errorf("a file written from %s is described as %q", config.FileEnvVar, said)
	}
	if !strings.Contains(said, config.FileEnvVar) {
		t.Errorf("it does not say where the settings came from: %q", said)
	}

	// And without the variable, the sentence it had is still right.
	t.Setenv(config.FileEnvVar, "")
	if said := describeConfigFile("/data/config.toml", true); !strings.Contains(said, "defaults") {
		t.Errorf("a file nobody supplied is described as %q", said)
	}
}
