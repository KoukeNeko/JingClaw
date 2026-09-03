package daemon

import (
	"os"
	"path/filepath"
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

// A deployment has the files it is meant to be edited, without --init.
//
// They were created only by --init, so a daemon started any other way — a
// container, a service, anyone who ran "jingclaw" and never read about the
// flag — ran with neither, and the reason they are created rather than
// documented ("a file that exists is a file somebody edits; one they have to
// know to create is one that stays absent") applied to nobody but the person
// who already knew.
func TestStartingCreatesTheFilesMeantToBeEdited(t *testing.T) {
	root := t.TempDir()

	if err := writeInstructionFiles(root); err != nil {
		t.Fatalf("write: %v", err)
	}

	for _, name := range config.InstructionFiles() {
		content, err := os.ReadFile(filepath.Join(root, name))
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		if !strings.Contains(string(content), instructionPurpose[name]) {
			t.Errorf("%s does not say what it is for: %q", name, string(content))
		}
	}
}

// And never over what is in them.
//
// The daemon runs this on every start, so overwriting would discard an
// edited persona on the restart after somebody edited it — and a persona
// seeded from the environment would be replaced by an empty heading before
// it was ever read.
func TestAnInstructionFileThatExistsIsLeftAlone(t *testing.T) {
	root := t.TempDir()

	theirs := "# Who you are\n\nAnswer briefly.\n"
	if err := os.WriteFile(filepath.Join(root, config.PersonaFile), []byte(theirs), 0o600); err != nil {
		t.Fatalf("staging: %v", err)
	}

	if err := writeInstructionFiles(root); err != nil {
		t.Fatalf("write: %v", err)
	}

	back, _ := os.ReadFile(filepath.Join(root, config.PersonaFile))
	if string(back) != theirs {
		t.Errorf("it replaced a persona that was already there: %q", string(back))
	}
}

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
