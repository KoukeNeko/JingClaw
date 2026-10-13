package config_test

import (
	"reflect"
	"strings"
	"testing"

	"github.com/KoukeNeko/JingClaw/core/internal/config"
)

// differing lists the lines of two texts that are not the same line in both,
// by position. Enough to say "exactly this one changed" for an edit that keeps
// the line count.
func differing(t *testing.T, before, after string) []int {
	t.Helper()

	a, b := strings.Split(before, "\n"), strings.Split(after, "\n")
	if len(a) != len(b) {
		t.Fatalf("the edit changed the number of lines, %d to %d:\n%s", len(a), len(b), after)
	}

	var at []int
	for i := range a {
		if a[i] != b[i] {
			at = append(at, i)
		}
	}
	return at
}

func patch(t *testing.T, src string, edits ...config.Edit) config.Patched {
	t.Helper()

	patched, err := config.Patch([]byte(src), edits)
	if err != nil {
		t.Fatalf("patch: %v", err)
	}
	return patched
}

func candidate(t *testing.T, patched config.Patched) config.Config {
	t.Helper()

	cfg, err := config.Candidate(patched.Text)
	if err != nil {
		t.Fatalf("the patched file does not load: %v", err)
	}
	return cfg
}

// Every setting is in the example, commented out. Turning that line on is the
// whole change: a file read in an editor afterwards has the setting where it
// was, and nothing else moved.
func TestAnEditSwitchesOnTheCommentedDefault(t *testing.T) {
	patched := patch(t, config.Example, config.Edit{Key: "provider.retry.max_attempts", Value: float64(6)})

	at := differing(t, config.Example, string(patched.Text))
	if len(at) != 1 {
		t.Fatalf("expected one line to change, got %v", at)
	}
	if got := strings.Split(string(patched.Text), "\n")[at[0]]; got != "max_attempts = 6" {
		t.Errorf("the line is %q", got)
	}

	if got := candidate(t, patched).Provider.Retry.MaxAttempts; got != 6 {
		t.Errorf("the file says %d", got)
	}
}

// A setting that is already written is replaced where it stands.
func TestAnEditReplacesTheLineThatSetsIt(t *testing.T) {
	src := "[agent]\n# Tool calls one turn may make.\nmax_iterations = 20\npermission_profile = \"local\"\n"

	patched := patch(t, src, config.Edit{Key: "agent.max_iterations", Value: float64(30)})

	want := "[agent]\n# Tool calls one turn may make.\nmax_iterations = 30\npermission_profile = \"local\"\n"
	if string(patched.Text) != want {
		t.Errorf("got:\n%s\nwant:\n%s", patched.Text, want)
	}
}

// A file written with dotted keys sets the same setting as one written with
// sections, and has to be found the same way.
func TestAnEditFindsASettingWrittenWithDottedKeys(t *testing.T) {
	src := "[provider]\nretry.max_attempts = 9\nbackend = \"fake\"\n"

	patched := patch(t, src, config.Edit{Key: "provider.retry.max_attempts", Value: float64(3)})

	if got := candidate(t, patched).Provider.Retry.MaxAttempts; got != 3 {
		t.Errorf("the file says %d:\n%s", got, patched.Text)
	}
	if strings.Count(string(patched.Text), "max_attempts") != 1 {
		t.Errorf("the setting is written twice:\n%s", patched.Text)
	}
}

// Taking a setting back writes it as the commented default the example began
// with, so the file reads as it did before anybody touched it.
func TestRemovingASettingCommentsItBackOut(t *testing.T) {
	src := "[provider.retry]\nmax_attempts = 9\n# jitter = 0.3\n"

	patched := patch(t, src, config.Edit{Key: "provider.retry.max_attempts", Remove: true})

	if want := "[provider.retry]\n# max_attempts = 4\n# jitter = 0.3\n"; string(patched.Text) != want {
		t.Errorf("got:\n%s\nwant:\n%s", patched.Text, want)
	}
	if got := candidate(t, patched).Provider.Retry.MaxAttempts; got != 4 {
		t.Errorf("the default is %d", got)
	}
}

func TestRemovingASettingTheFileDoesNotSetChangesNothing(t *testing.T) {
	patched := patch(t, config.Example, config.Edit{Key: "provider.retry.max_attempts", Remove: true})

	if string(patched.Text) != config.Example {
		t.Error("the file changed though there was nothing to take back")
	}
	if len(patched.Diff()) != 0 {
		t.Errorf("a change was reported: %v", patched.Diff())
	}
}

// A section the file does not have is added at the end, with a blank line
// before it.
func TestAnEditAddsASectionTheFileLacks(t *testing.T) {
	src := "[agent]\nmax_iterations = 20\n"

	patched := patch(t, src, config.Edit{Key: "tools.glob_results", Value: float64(50)})

	if want := "[agent]\nmax_iterations = 20\n\n[tools]\nglob_results = 50\n"; string(patched.Text) != want {
		t.Errorf("got:\n%s\nwant:\n%s", patched.Text, want)
	}
}

// A setting the section already has goes after its last one, not at the end of
// the file, where it would land in another section.
func TestANewSettingGoesAfterItsSectionsLastOne(t *testing.T) {
	src := "[agent]\nmax_iterations = 20\n\n# The rest.\n[tools]\nglob_results = 50\n"

	patched := patch(t, src, config.Edit{Key: "agent.permission_profile", Value: "gateway"})

	want := "[agent]\nmax_iterations = 20\npermission_profile = \"gateway\"\n\n# The rest.\n[tools]\nglob_results = 50\n"
	if string(patched.Text) != want {
		t.Errorf("got:\n%s\nwant:\n%s", patched.Text, want)
	}
}

// The last line of a file need not end in a newline, and a setting added after
// it must not be glued to it.
func TestAnEditToAFileWithNoFinalNewline(t *testing.T) {
	patched := patch(t, "[agent]\nmax_iterations = 20", config.Edit{Key: "agent.permission_profile", Value: "gateway"})

	cfg := candidate(t, patched)
	if cfg.Agent.PermissionProfile != "gateway" || cfg.Agent.MaxIterations != 20 {
		t.Errorf("the file reads %+v:\n%s", cfg.Agent, patched.Text)
	}
}

// What the operator wrote around the edit is theirs: the comments, the blank
// lines and the order. Edits across the whole example change exactly the lines
// they were for.
func TestEditsLeaveEverythingElseAlone(t *testing.T) {
	patched := patch(t, config.Example,
		config.Edit{Key: "agent.max_iterations", Value: float64(20)},
		config.Edit{Key: "provider.retry.base_delay", Value: "1s"},
		config.Edit{Key: "web.enabled", Value: true},
		config.Edit{Key: "sandbox.hidden", Value: []any{"~/.ssh", "~/.aws"}},
		config.Edit{Key: "memory.embedding.min_similarity", Value: 0.6},
	)

	at := differing(t, config.Example, string(patched.Text))
	if len(at) != 5 {
		t.Fatalf("expected five lines to change, got %v", at)
	}

	cfg := candidate(t, patched)
	if cfg.Agent.MaxIterations != 20 || !cfg.Web.Enabled || len(cfg.Sandbox.Hidden) != 2 ||
		cfg.Memory.Embedding.MinSimilarity != 0.6 || cfg.Provider.Retry.BaseDelay.String() != "1s" {
		t.Errorf("the file does not say what was edited: %+v", cfg)
	}

	// Four of the five were commented-out defaults, which become the setting.
	// web.enabled is written out in the example, so replacing it loses none.
	if comments(config.Example) != comments(string(patched.Text))+4 {
		t.Errorf("a comment was lost or kept: %d before, %d after", comments(config.Example), comments(string(patched.Text)))
	}
}

// comments counts the lines that are only a comment.
func comments(text string) int {
	var n int
	for _, line := range strings.Split(text, "\n") {
		if strings.HasPrefix(strings.TrimSpace(line), "#") {
			n++
		}
	}
	return n
}

// A list of tables is written as a unit, replaced by position, and removed by
// being set to nothing.
func TestAListOfTablesIsWrittenAndReplaced(t *testing.T) {
	servers := func(names ...string) config.Edit {
		var items []any
		for _, name := range names {
			items = append(items, map[string]any{
				"name": name, "command": "run-" + name, "args": []any{"--root", "."},
				"env": map[string]any{"LOG_LEVEL": "info"}, "oauth": false, "defer": true,
			})
		}
		return config.Edit{Key: "mcp.servers", Value: items}
	}

	two := patch(t, config.Example, servers("files", "git"))
	if got := candidate(t, two).MCP.Servers; len(got) != 2 || got[0].Name != "files" || got[1].Command != "run-git" ||
		got[0].Env["LOG_LEVEL"] != "info" || !got[1].Defer {
		t.Fatalf("two servers did not come out: %+v\n%s", got, two.Text)
	}

	one := patch(t, string(two.Text), servers("files"))
	if got := candidate(t, one).MCP.Servers; len(got) != 1 || got[0].Name != "files" {
		t.Fatalf("replacing the list left %+v\n%s", got, one.Text)
	}
	// A whole line, because the example has the same text commented out.
	if strings.Count(string(one.Text), "\n[[mcp.servers]]\n") != 1 {
		t.Errorf("the old blocks were not taken out:\n%s", one.Text)
	}

	none := patch(t, string(one.Text), config.Edit{Key: "mcp.servers", Value: []any{}})
	if got := candidate(t, none).MCP.Servers; len(got) != 0 {
		t.Errorf("an empty list left %+v", got)
	}
	if strings.Contains(string(none.Text), "\n[[mcp.servers]]\n") {
		t.Errorf("a block is still there:\n%s", none.Text)
	}
}

// A section that hangs off a list element belongs to that element, and goes
// with it.
func TestReplacingAListTakesItsSubTablesWithIt(t *testing.T) {
	src := "[[mcp.servers]]\nname = \"files\"\ncommand = \"x\"\n\n[mcp.servers.env]\nA = \"1\"\n\n[agent]\nmax_iterations = 5\n"

	patched := patch(t, src, config.Edit{Key: "mcp.servers", Value: []any{}})

	cfg := candidate(t, patched)
	if len(cfg.MCP.Servers) != 0 || cfg.Agent.MaxIterations != 5 {
		t.Errorf("got %+v and %d:\n%s", cfg.MCP.Servers, cfg.Agent.MaxIterations, patched.Text)
	}
}

// Channels live under the platform they belong to, so a list is put in that
// section when the file has it.
func TestAListIsAddedToItsOwnSection(t *testing.T) {
	src := "[gateway.discord]\naccount_id = \"main\"\n\n[tools]\nglob_results = 50\n"

	patched := patch(t, src, config.Edit{Key: "gateway.discord.channels", Value: []any{
		map[string]any{"channel_ids": []any{"111"}, "users": []any{"222"}},
	}})

	cfg := candidate(t, patched)
	if len(cfg.Gateway.Discord.Channels) != 1 || cfg.Gateway.Discord.Channels[0].Users[0] != "222" {
		t.Fatalf("the channel did not come out: %+v\n%s", cfg.Gateway.Discord.Channels, patched.Text)
	}
	if cfg.Tools.GlobResults != 50 {
		t.Errorf("a neighbouring section was disturbed:\n%s", patched.Text)
	}
}

func TestEditsToWhatIsNotASettingAreRefused(t *testing.T) {
	cases := map[string]config.Edit{
		"an unknown key":         {Key: "agent.nope", Value: "x"},
		"a section":              {Key: "agent", Value: "x"},
		"text for a number":      {Key: "agent.max_iterations", Value: "twelve"},
		"a fraction for a count": {Key: "agent.max_iterations", Value: 1.5},
		"a number for text":      {Key: "agent.permission_profile", Value: float64(1)},
		"a bad duration":         {Key: "tools.command_timeout", Value: "soon"},
		"a number in a list":     {Key: "sandbox.hidden", Value: []any{float64(1)}},
		"a list for a table":     {Key: "mcp.servers", Value: "files"},
		"an element's own key":   {Key: "mcp.servers", Value: []any{map[string]any{"nope": "x"}}},
	}

	for name, edit := range cases {
		if _, err := config.Patch([]byte(config.Example), []config.Edit{edit}); err == nil {
			t.Errorf("%s was accepted", name)
		}
	}
}

// Go's quoting has escapes TOML does not. A value that does not survive being
// written is a file that does not load.
func TestAValueSurvivesBeingWritten(t *testing.T) {
	for _, value := range []string{
		"plain", `with "quotes"`, `back\slash`, "two\nlines", "tab\there", "bell\a", "日本語 and 中文", "",
	} {
		patched := patch(t, config.Example, config.Edit{Key: "provider.anthropic.base_url", Value: value})

		if got := candidate(t, patched).Provider.Anthropic.BaseURL; got != value {
			t.Errorf("%q came back as %q:\n%s", value, got, patched.Text)
		}
	}
}

// The check that nothing else moved is only worth having if it can fail, so it
// is shown a file whose own text has the setting twice.
func TestAFileTheEditCannotSafelyChangeIsRefused(t *testing.T) {
	src := "[agent]\nmax_iterations = 20\n\n[agent.extra]\nx = 1\n"

	if _, err := config.Patch([]byte(src), []config.Edit{{Key: "agent.permission_profile", Value: "gateway"}}); err != nil {
		t.Fatalf("a file with a section the program does not know was refused: %v", err)
	}

	if _, err := config.Patch([]byte("[agent\nbroken"), []config.Edit{{Key: "agent.max_iterations", Value: float64(1)}}); err == nil {
		t.Error("a file that does not parse was patched")
	}
}

// What an edit would do has to be answerable without writing anything, and
// answered by the check the daemon uses when it starts.
func TestPreviewReportsWhatTheDaemonWouldRefuse(t *testing.T) {
	patched, problems, err := config.Preview([]byte(config.Example), []config.Edit{
		{Key: "provider.retry.base_delay", Value: "45s"},
	})
	if err != nil {
		t.Fatalf("preview: %v", err)
	}

	if len(problems) != 1 || problems[0].Key != "provider.retry.base_delay" {
		t.Fatalf("expected the delay to be refused, got %+v", problems)
	}
	if len(patched.Diff()) == 0 {
		t.Error("a refused edit still has a diff to show")
	}

	_, problems, err = config.Preview([]byte(config.Example), []config.Edit{
		{Key: "provider.retry.base_delay", Value: "1s"},
	})
	if err != nil || len(problems) != 0 {
		t.Errorf("a good edit was refused: %v %+v", err, problems)
	}
}

func TestADiffIsGroupedBySection(t *testing.T) {
	patched := patch(t, config.Example,
		config.Edit{Key: "provider.retry.max_attempts", Value: float64(6)},
		config.Edit{Key: "provider.retry.base_delay", Value: "1s"},
	)

	want := []config.DiffLine{
		{Kind: "context", Text: "@@ [provider.retry]"},
		{Kind: "removed", Text: "# max_attempts = 4"},
		{Kind: "added", Text: "max_attempts = 6"},
		{Kind: "removed", Text: `# base_delay = "500ms"`},
		{Kind: "added", Text: `base_delay = "1s"`},
	}
	if got := patched.Diff(); !reflect.DeepEqual(got, want) {
		t.Errorf("got %+v\nwant %+v", got, want)
	}
}

// What a patch produces is read by the daemon as Candidate reads it, and the
// answer is the same whether the text was patched or typed. If the two could
// disagree, "this validates" would mean nothing about whether it starts.
func TestAPatchedTextReadsTheSameAsTheSameTextTyped(t *testing.T) {
	patched := patch(t, config.Example, config.Edit{Key: "agent.max_iterations", Value: float64(20)})

	typed, err := config.Candidate([]byte(strings.Replace(
		config.Example, "# max_iterations = 12", "max_iterations = 20", 1)))
	if err != nil {
		t.Fatalf("candidate: %v", err)
	}
	if !reflect.DeepEqual(candidate(t, patched), typed) {
		t.Error("the same text reads differently when it was patched and when it was written")
	}
}

func TestFlattenReportsSettingsAsPlainValues(t *testing.T) {
	flat := config.Flatten(config.Defaults())

	checks := map[string]any{
		"provider.retry.max_attempts": int64(4),
		"provider.retry.jitter":       0.3,
		"tools.command_timeout":       "2m",
		"tools.max_command_timeout":   "10m",
		"provider.retry.budget":       "1m30s",
		"provider.retry.base_delay":   "500ms",
		"memory.enabled":              true,
		"sandbox.hidden":              []any{},
		"provider.gemini.api_key_env": []any{"GEMINI_API_KEY", "GOOGLE_API_KEY"},
		"mcp.servers":                 []any{},
	}
	for key, want := range checks {
		if got, ok := flat[key]; !ok || !reflect.DeepEqual(got, want) {
			t.Errorf("%s is %#v, want %#v", key, got, want)
		}
	}
}

// Every setting has a form, or a client would silently never offer it. This is
// the same walk that panics on a field type nobody taught it.
func TestEverySettingHasAKindAndADefault(t *testing.T) {
	settings := config.Settings()
	if len(settings) < 100 {
		t.Fatalf("only %d settings found", len(settings))
	}

	flat := config.Flatten(config.Defaults())
	for _, setting := range settings {
		if setting.Kind == "" || setting.Default == nil {
			t.Errorf("%s has no kind or default", setting.Key)
		}
		if _, ok := flat[setting.Key]; !ok {
			t.Errorf("%s is not in the flat form", setting.Key)
		}
		if setting.Kind == config.KindList && len(setting.Items) == 0 {
			t.Errorf("the list %s does not say what an element is", setting.Key)
		}
	}
}

func TestEnvironmentNameOnlyForKeysTheEnvironmentCanReach(t *testing.T) {
	if name, ok := config.EnvironmentName("provider.backend"); !ok || name != "JINGCLAW_PROVIDER_BACKEND" {
		t.Errorf("got %q %v", name, ok)
	}
	if name, ok := config.EnvironmentName("agent.max_iterations"); !ok || name != "JINGCLAW_AGENT_MAX_ITERATIONS" {
		t.Errorf("got %q %v", name, ok)
	}
	if _, ok := config.EnvironmentName("provider.anthropic.model"); ok {
		t.Error("a key three deep has no spelling as a variable")
	}
}
