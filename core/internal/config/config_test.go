package config_test

import (
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/KoukeNeko/JingClaw/core/internal/config"
)

func writeConfig(t *testing.T, contents string) string {
	t.Helper()

	path := filepath.Join(t.TempDir(), "config.toml")
	if err := os.WriteFile(path, []byte(contents), 0o600); err != nil {
		t.Fatalf("write config: %v", err)
	}
	return path
}

// Running with no configuration at all has to work, or the first experience of
// the tool is editing TOML.
func TestMissingDefaultFileIsNotAnError(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	t.Setenv("HOME", t.TempDir())

	cfg, used, err := config.Load("")
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if used != "" {
		t.Errorf("reported reading %q when there was no file", used)
	}
	if cfg.Agent.MaxIterations != config.Defaults().Agent.MaxIterations {
		t.Errorf("defaults were not applied: %+v", cfg.Agent)
	}
}

// A file the operator explicitly named and that is not there is a mistake,
// not something to shrug at: they believe those settings are in force.
func TestExplicitMissingFileIsAnError(t *testing.T) {
	_, _, err := config.Load(filepath.Join(t.TempDir(), "absent.toml"))
	if err == nil {
		t.Fatal("a config file that does not exist was accepted")
	}
}

// Silently ignoring a broken file would run with settings nobody chose while
// the operator believes theirs are active.
func TestUnparseableFileIsAnError(t *testing.T) {
	path := writeConfig(t, "[agent\nname = broken")

	if _, _, err := config.Load(path); err == nil {
		t.Fatal("a malformed config file was accepted")
	}
}

func TestFileOverridesDefaults(t *testing.T) {
	path := writeConfig(t, `
[agent]
name = "江委員"
max_iterations = 3

[model]
provider = "gemini"
model = "gemma-4-31b-it"
`)

	cfg, used, err := config.Load(path)
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if used != path {
		t.Errorf("reported reading %q, want %q", used, path)
	}

	if cfg.Agent.Name != "江委員" {
		t.Errorf("name is %q", cfg.Agent.Name)
	}
	if cfg.Agent.MaxIterations != 3 {
		t.Errorf("max_iterations is %d", cfg.Agent.MaxIterations)
	}
	if cfg.Model.Model != "gemma-4-31b-it" {
		t.Errorf("model is %q", cfg.Model.Model)
	}

	// Anything the file did not mention keeps its default.
	if len(cfg.Agent.InstructionFiles) == 0 {
		t.Error("instruction_files lost its default")
	}
	if cfg.Server.Addr != config.Defaults().Server.Addr {
		t.Errorf("addr is %q, want the default", cfg.Server.Addr)
	}
}

// The environment sits between the file and flags, so a shell or a service
// definition can override a checked-in file without editing it.
func TestEnvironmentOverridesFile(t *testing.T) {
	path := writeConfig(t, "[agent]\nname = \"from file\"\n")
	t.Setenv("JINGCLAW_AGENT_NAME", "from env")

	cfg, _, err := config.Load(path)
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if cfg.Agent.Name != "from env" {
		t.Errorf("name is %q, want the environment value", cfg.Agent.Name)
	}
}

// An empty name is a real choice: not claiming one is honest when the account
// the agent speaks through is called something else.
func TestNameCanBeCleared(t *testing.T) {
	path := writeConfig(t, "[agent]\nname = \"\"\n")

	cfg, _, err := config.Load(path)
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if cfg.Agent.Name != "" {
		t.Errorf("name is %q, want it cleared", cfg.Agent.Name)
	}
}

// The example has to be valid, or the first thing an operator copies fails to
// parse.
func TestExampleParsesAndChangesNothing(t *testing.T) {
	path := writeConfig(t, config.Example)

	cfg, _, err := config.Load(path)
	if err != nil {
		t.Fatalf("the example config does not parse: %v", err)
	}

	defaults := config.Defaults()
	if cfg.Agent.Name != defaults.Agent.Name || cfg.Model.Provider != defaults.Model.Provider {
		t.Errorf("the example changes behaviour despite being fully commented:\n%+v", cfg)
	}

}

// Every setting has to appear in the example, or an operator can only discover
// it by reading the source.
//
// The list of settings is taken from the struct rather than written out here.
// A hand-written list has to be updated in two places whenever a field is
// added, and the failure mode is silent: the check passes while the new
// setting is undocumented.
func TestExampleDocumentsEverySetting(t *testing.T) {
	for _, key := range koanfKeys(reflect.TypeOf(config.Config{})) {
		if !strings.Contains(config.Example, key) {
			t.Errorf("the example does not document %q; add it, commented out", key)
		}
	}
}

// koanfKeys collects the koanf tag of every leaf field, walking into nested
// structs.
func koanfKeys(t reflect.Type) []string {
	var keys []string

	for i := range t.NumField() {
		field := t.Field(i)

		tag := field.Tag.Get("koanf")
		if tag == "" || tag == "-" {
			continue
		}

		if field.Type.Kind() == reflect.Struct {
			// A section name is not a setting; its fields are.
			keys = append(keys, koanfKeys(field.Type)...)
			continue
		}

		keys = append(keys, tag)
	}

	return keys
}
