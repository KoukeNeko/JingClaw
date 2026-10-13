package config_test

import (
	"reflect"
	"strings"
	"testing"

	"github.com/KoukeNeko/JingClaw/core/internal/config"
)

// The variable a bootstrap key names is the one the environment is actually
// read through. A list that said otherwise would send an operator to a variable
// that does nothing.
func TestEveryBootstrapKeyNamesTheVariableThatReachesIt(t *testing.T) {
	if len(config.BootstrapKeys()) != 2 {
		t.Fatalf("the bootstrap keys are %v", config.BootstrapKeys())
	}

	for _, bootstrap := range config.BootstrapKeys() {
		name, reachable := config.EnvironmentName(bootstrap.Key)
		if !reachable || name != bootstrap.Env {
			t.Errorf("%s names %s; the environment reads it through %q (%v)",
				bootstrap.Key, bootstrap.Env, name, reachable)
		}
	}
}

// Where the settings are kept cannot be one of them.
//
// The saved text is in the database, so a text that said where the database is
// would be obeyed only by a daemon that had already found it. Worse, it would
// be obeyed by the next daemon started against a different one. So a line for
// either key is read and ignored, and everything beside it is still read.
func TestTheSavedTextCannotMoveTheDatabaseOrTheDiscoveryFile(t *testing.T) {
	spellings := map[string]string{
		"a section": "[server]\ndata_dir = \"/from/the/text\"\nruntime_dir = \"/run/from/the/text\"\naddr = \"127.0.0.1:7000\"\n",
		"dotted":    "server.data_dir = \"/from/the/text\"\nserver.runtime_dir = \"/run/from/the/text\"\nserver.addr = \"127.0.0.1:7000\"\n",
		"inline":    "server = { data_dir = \"/from/the/text\", runtime_dir = \"/run/from/the/text\", addr = \"127.0.0.1:7000\" }\n",
	}

	for name, text := range spellings {
		t.Run(name, func(t *testing.T) {
			cfg := candidateOf(t, text)

			if cfg.Server.DataDir != "" || cfg.Server.RuntimeDir != "" {
				t.Errorf("the text moved them: data_dir %q, runtime_dir %q", cfg.Server.DataDir, cfg.Server.RuntimeDir)
			}
			if cfg.Server.Addr != "127.0.0.1:7000" {
				t.Errorf("a setting beside them was lost: addr is %q", cfg.Server.Addr)
			}
		})
	}
}

// What gives them is the environment, and the text does not get in its way.
func TestTheEnvironmentStillGivesTheBootstrapKeys(t *testing.T) {
	t.Setenv("JINGCLAW_SERVER_DATA_DIR", "/from/the/environment")
	t.Setenv("JINGCLAW_SERVER_RUNTIME_DIR", "/run/from/the/environment")

	cfg := candidateOf(t, "[server]\ndata_dir = \"/from/the/text\"\nruntime_dir = \"/run/from/the/text\"\n")

	if cfg.Server.DataDir != "/from/the/environment" || cfg.Server.RuntimeDir != "/run/from/the/environment" {
		t.Errorf("data_dir %q, runtime_dir %q", cfg.Server.DataDir, cfg.Server.RuntimeDir)
	}
}

// The daemon takes where it is running from reading nothing, and from reading
// the saved text. Those have to be the same two values, or a daemon would open
// one database to find its settings and then run against another.
func TestReadingTheTextAndReadingNothingAgreeOnWhereTheDaemonIs(t *testing.T) {
	t.Setenv("JINGCLAW_SERVER_DATA_DIR", "/from/the/environment")

	bootstrap := candidateOf(t, "").Server
	for _, text := range []string{
		config.Example,
		"[server]\ndata_dir = \"/elsewhere\"\nruntime_dir = \"/run/elsewhere\"\n",
		"[agent]\nmax_iterations = 3\n",
	} {
		running := candidateOf(t, text).Server
		if running.DataDir != bootstrap.DataDir || running.RuntimeDir != bootstrap.RuntimeDir {
			t.Errorf("%q: the daemon would run in %q and %q, having started in %q and %q",
				text, running.DataDir, running.RuntimeDir, bootstrap.DataDir, bootstrap.RuntimeDir)
		}
	}
}

// An edit to one is refused with the way to give it instead, whether it sets it
// or takes it back.
func TestAnEditToABootstrapKeyIsAProblemThatSaysWhatToUseInstead(t *testing.T) {
	problem, refused := config.BootstrapProblem(config.Edit{Key: "server.data_dir", Value: "/data"})
	if !refused {
		t.Fatal("an edit to server.data_dir was not refused")
	}
	if problem.Key != "server.data_dir" || problem.Value != `"/data"` {
		t.Errorf("the problem points at %s = %s", problem.Key, problem.Value)
	}
	for _, want := range []string{"JINGCLAW_SERVER_DATA_DIR", "--data-dir"} {
		if !strings.Contains(problem.Fix, want) {
			t.Errorf("the fix does not mention %s: %q", want, problem.Fix)
		}
	}
	if problem.Why == "" {
		t.Error("a problem that does not say why")
	}

	problem, refused = config.BootstrapProblem(config.Edit{Key: "server.runtime_dir", Remove: true})
	if !refused || !strings.Contains(problem.Fix, "JINGCLAW_SERVER_RUNTIME_DIR") {
		t.Errorf("taking back runtime_dir: %+v %v", problem, refused)
	}
	if strings.Contains(problem.Fix, "--") {
		t.Errorf("runtime_dir has no flag, and the fix names one: %q", problem.Fix)
	}

	if _, refused := config.BootstrapProblem(config.Edit{Key: "server.addr", Value: "127.0.0.1:0"}); refused {
		t.Error("an edit to a key beside them was refused")
	}
}

// Text that sets one is refused as a whole, by name.
func TestTextThatSetsABootstrapKeyIsAProblemForEachOne(t *testing.T) {
	problems, err := config.BootstrapProblems([]byte("[server]\ndata_dir = \"/a\"\nruntime_dir = \"/b\"\n"))
	if err != nil {
		t.Fatalf("problems: %v", err)
	}
	if len(problems) != 2 || problems[0].Key != "server.data_dir" || problems[1].Key != "server.runtime_dir" {
		t.Errorf("problems: %+v", problems)
	}

	for _, clean := range []string{config.Example, "", "[server]\naddr = \"127.0.0.1:0\"\n"} {
		if problems, err := config.BootstrapProblems([]byte(clean)); err != nil || len(problems) != 0 {
			t.Errorf("%q: %+v %v", clean, problems, err)
		}
	}

	if _, err := config.BootstrapProblems([]byte("[server\n")); err == nil {
		t.Error("text that does not parse was inspected")
	}
}

// The daemon compares what an imported file says with where it is running, and
// needs the value, not only that there is one.
func TestWhatATextSaysAboutTheBootstrapKeysIsReported(t *testing.T) {
	set, err := config.BootstrapSettings([]byte("[server]\ndata_dir = \"/a\"\n"))
	if err != nil {
		t.Fatalf("settings: %v", err)
	}
	if !reflect.DeepEqual(set, map[string]string{"server.data_dir": "/a"}) {
		t.Errorf("settings: %v", set)
	}
}

// The settings a text sets are the ones that can be changed. One that is
// ignored on reading is not offered as set.
func TestTheSettingsATextSetsLeaveOutTheBootstrapKeys(t *testing.T) {
	set, err := config.FileSettings([]byte("[server]\ndata_dir = \"/a\"\nlog_level = \"debug\"\n"))
	if err != nil {
		t.Fatalf("file settings: %v", err)
	}
	if _, present := set["server.data_dir"]; present {
		t.Error("a setting that is ignored was reported as set")
	}
	if set["server.log_level"] != "debug" {
		t.Errorf("a setting beside it was lost: %v", set)
	}
}

// Check is the daemon's own test of a text, so it answers the way starting would.
func TestCheckAnswersAsStartingWouldAndIgnoresTheBootstrapKeys(t *testing.T) {
	problems, err := config.Check([]byte("[agent]\nmax_iterations = 0\n"))
	if err != nil || len(problems) != 1 || problems[0].Key != "agent.max_iterations" {
		t.Errorf("a text that would not start: %+v %v", problems, err)
	}

	if problems, err := config.Check([]byte(config.Example)); err != nil || len(problems) != 0 {
		t.Errorf("the example: %+v %v", problems, err)
	}

	// Ignored when read, so not something starting would complain about.
	if problems, err := config.Check([]byte("[server]\ndata_dir = \"/a\"\n")); err != nil || len(problems) != 0 {
		t.Errorf("a bootstrap key: %+v %v", problems, err)
	}

	if _, err := config.Check([]byte("[agent\n")); err == nil {
		t.Error("text that does not parse was checked")
	}
	if _, err := config.Check([]byte("[agent]\nmax_iterations = \"twelve\"\n")); err == nil {
		t.Error("text that cannot be read as settings was checked")
	}
}

// The report says "no saved settings" only when none were read.
func TestReportSaysWhenNothingSavedWasRead(t *testing.T) {
	cfg := config.Defaults()
	cfg.Agent.MaxIterations = 0

	var out strings.Builder
	if !config.Report(&out, cfg.Validate(), "") {
		t.Fatal("Report did not recognise a configuration problem")
	}
	if !strings.Contains(out.String(), "no saved settings were read") {
		t.Errorf("the report does not say where the values came from:\n%s", out.String())
	}
}
