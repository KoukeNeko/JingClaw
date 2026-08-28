package config_test

import (
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

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

		// A list of tables is the same: the settings are inside its elements,
		// and a server option nobody documents is one an operator can only
		// find by reading the source.
		if field.Type.Kind() == reflect.Slice && field.Type.Elem().Kind() == reflect.Struct {
			keys = append(keys, tag)
			keys = append(keys, koanfKeys(field.Type.Elem())...)
			continue
		}

		keys = append(keys, tag)
	}

	return keys
}

// The defaults have to survive their own validation, or nothing runs.
func TestDefaultsAreValid(t *testing.T) {
	if err := config.Defaults().Validate(); err != nil {
		t.Fatalf("the defaults do not validate: %v", err)
	}
}

// The example is what an operator starts from, so its uncommented state must
// also pass.
func TestExampleIsValid(t *testing.T) {
	cfg, _, err := config.Load(writeConfig(t, config.Example))
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if err := cfg.Validate(); err != nil {
		t.Fatalf("the example does not validate: %v", err)
	}
}

// Durations are written the way people write them, not in nanoseconds.
func TestDurationsAreReadAsText(t *testing.T) {
	path := writeConfig(t, `
[tools]
command_timeout = "45s"

[model.retry]
base_delay = "1500ms"
`)

	cfg, _, err := config.Load(path)
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if cfg.Tools.CommandTimeout != 45*time.Second {
		t.Errorf("command_timeout is %s, want 45s", cfg.Tools.CommandTimeout)
	}
	if cfg.Model.Retry.BaseDelay != 1500*time.Millisecond {
		t.Errorf("base_delay is %s, want 1.5s", cfg.Model.Retry.BaseDelay)
	}
}

// A setting nobody could previously write is now one somebody can, so each new
// one is checked at startup rather than at the moment it matters.
func TestValidateRejectsBadValues(t *testing.T) {
	tests := []struct {
		name    string
		mutate  func(*config.Config)
		mention string
	}{
		{
			// The important one: this API can run programs, so an address that
			// reaches the network is refused however it was written.
			name:    "an address off the loopback interface",
			mutate:  func(c *config.Config) { c.Server.Addr = "0.0.0.0:8080" },
			mention: "loopback",
		},
		{
			name:    "an address that is not host:port",
			mutate:  func(c *config.Config) { c.Server.Addr = "127.0.0.1" },
			mention: "host:port",
		},
		{
			name:    "a permission profile that does not exist",
			mutate:  func(c *config.Config) { c.Agent.PermissionProfile = "trusted" },
			mention: "permission_profile",
		},
		{
			name:    "a log level that does not exist",
			mutate:  func(c *config.Config) { c.Server.LogLevel = "verbose" },
			mention: "log_level",
		},
		{
			name:    "a run that may take no turns at all",
			mutate:  func(c *config.Config) { c.Agent.MaxIterations = 0 },
			mention: "max_iterations",
		},
		{
			name: "a default command timeout above the ceiling on one",
			mutate: func(c *config.Config) {
				c.Tools.CommandTimeout = time.Hour
				c.Tools.MaxCommandTimeout = time.Minute
			},
			mention: "command_timeout",
		},
		{
			name: "a retry that starts slower than it is allowed to get",
			mutate: func(c *config.Config) {
				c.Model.Retry.BaseDelay = time.Minute
				c.Model.Retry.MaxDelay = time.Second
			},
			mention: "base_delay",
		},
		{
			name:    "jitter outside the fraction it is",
			mutate:  func(c *config.Config) { c.Model.Retry.Jitter = 2 },
			mention: "jitter",
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			cfg := config.Defaults()
			test.mutate(&cfg)

			err := cfg.Validate()
			if err == nil {
				t.Fatal("accepted")
			}
			if !strings.Contains(err.Error(), test.mention) {
				t.Errorf("the error does not say which setting is wrong: %v", err)
			}
		})
	}
}

// Loopback is spelled several ways, and refusing the ones an operator actually
// types would push them towards the address that is not safe.
func TestValidateAcceptsEveryLoopbackSpelling(t *testing.T) {
	for _, addr := range []string{"127.0.0.1:0", "localhost:7777", "[::1]:7777", "127.0.0.2:0"} {
		cfg := config.Defaults()
		cfg.Server.Addr = addr

		if err := cfg.Validate(); err != nil {
			t.Errorf("%s was refused: %v", addr, err)
		}
	}
}

// Every mistake at once, not the first one. Restarting a daemon to find out
// what else is wrong is work the program can do in a single pass.
func TestValidateReportsEveryProblemTogether(t *testing.T) {
	cfg := config.Defaults()
	cfg.Server.Addr = "0.0.0.0:8080"
	cfg.Server.LogLevel = "verbose"
	cfg.Agent.MaxIterations = 0

	var invalid *config.InvalidError
	if err := cfg.Validate(); !errors.As(err, &invalid) {
		t.Fatalf("want an InvalidError, got %v", err)
	}
	if len(invalid.Problems) != 3 {
		t.Fatalf("found %d problems, want 3: %v", len(invalid.Problems), invalid.Problems)
	}
}

// The report is read by somebody who is about to open the file, so it has to
// say which file, which line, and what to write instead.
func TestReportNamesTheFileAndEachSetting(t *testing.T) {
	cfg := config.Defaults()
	cfg.Server.Addr = "0.0.0.0:8080"
	cfg.Model.Provider = "openai"

	var out strings.Builder
	if !config.Report(&out, cfg.Validate(), "/etc/jingclaw/config.toml") {
		t.Fatal("Report did not recognise a configuration problem")
	}

	for _, want := range []string{
		"/etc/jingclaw/config.toml", // which file
		`server.addr = "0.0.0.0:8080"`,
		"loopback",
		`model.provider = "openai"`,
		`Use "gemini"`, // what to write instead
	} {
		if !strings.Contains(out.String(), want) {
			t.Errorf("the report does not mention %q:\n%s", want, out.String())
		}
	}
}

// Anything else has to fall through to the caller's own error handling, or a
// missing file would be swallowed by the reporter for configuration mistakes.
func TestReportIgnoresOtherErrors(t *testing.T) {
	var out strings.Builder
	if config.Report(&out, errors.New("disk on fire"), "") {
		t.Error("Report claimed an unrelated error was a configuration problem")
	}
	if out.Len() != 0 {
		t.Errorf("Report wrote something for an unrelated error: %s", out.String())
	}
}

// Each provider's settings are checked only when it is the one selected. A
// file that keeps several options ready, with one of them half filled in, is
// an ordinary way to work.
func TestOnlyTheChosenProviderIsValidated(t *testing.T) {
	cfg := config.Defaults()
	cfg.Model.Provider = "gemini"
	cfg.Model.OpenAICompat.BaseURL = "" // meaningless here, and not an error
	cfg.Model.OpenAICompat.Profile = "nonsense"

	if err := cfg.Validate(); err != nil {
		t.Fatalf("a section for a provider nobody chose was rejected: %v", err)
	}
}

// Selected, and then it must hold up. An endpoint with no address has no
// default to fall back to.
func TestAnOpenAICompatibleEndpointNeedsAnAddressAndAKnownProfile(t *testing.T) {
	tests := []struct {
		name    string
		mutate  func(*config.Config)
		mention string
	}{
		{
			name:    "no address",
			mutate:  func(c *config.Config) { c.Model.OpenAICompat.BaseURL = "" },
			mention: "base_url",
		},
		{
			// A typo must not silently become the profile that knows nothing
			// about how this server reports being out of credit.
			name: "a misspelled profile",
			mutate: func(c *config.Config) {
				c.Model.OpenAICompat.BaseURL = "http://localhost:8000/v1"
				c.Model.OpenAICompat.Profile = "vlm"
			},
			mention: "profile",
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			cfg := config.Defaults()
			cfg.Model.Provider = "openai_compat"
			cfg.Model.OpenAICompat.BaseURL = "http://localhost:8000/v1"
			test.mutate(&cfg)

			err := cfg.Validate()
			if err == nil {
				t.Fatal("accepted")
			}
			if !strings.Contains(err.Error(), test.mention) {
				t.Errorf("the error does not say which setting is wrong: %v", err)
			}
		})
	}
}

func TestOllamaSettingsAreChecked(t *testing.T) {
	cfg := config.Defaults()
	cfg.Model.Provider = "ollama"
	cfg.Model.Ollama.KeepAlive = "half an hour"

	err := cfg.Validate()
	if err == nil {
		t.Fatal("a keep_alive that is not a duration was accepted")
	}
	if !strings.Contains(err.Error(), "keep_alive") {
		t.Errorf("the error does not name the setting: %v", err)
	}

	// The defaults for a provider must survive being selected.
	valid := config.Defaults()
	valid.Model.Provider = "ollama"
	if err := valid.Validate(); err != nil {
		t.Fatalf("the ollama defaults do not validate: %v", err)
	}
}
