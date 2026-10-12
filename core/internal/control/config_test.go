package control_test

import (
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"connectrpc.com/connect"
	"google.golang.org/protobuf/types/known/structpb"

	controlv1 "github.com/KoukeNeko/JingClaw/core/gen/go/jingclaw/control/v1"
	"github.com/KoukeNeko/JingClaw/core/gen/go/jingclaw/control/v1/controlv1connect"
	"github.com/KoukeNeko/JingClaw/core/internal/config"
	"github.com/KoukeNeko/JingClaw/core/internal/control"
)

type configFixture struct {
	client controlv1connect.ConfigServiceClient
	path   string
	deploy string
}

// newConfigClient serves a real settings file, written as the daemon writes a
// new one, so that what is checked is the edit of that file.
func newConfigClient(t *testing.T, overrides ...control.ConfigOverride) configFixture {
	t.Helper()

	deploy := t.TempDir()
	path := filepath.Join(deploy, "config.toml")
	if err := os.WriteFile(path, []byte(config.Example), 0o600); err != nil {
		t.Fatalf("write config: %v", err)
	}
	for _, name := range []string{config.PersonaFile, config.InstructionsFile} {
		if err := os.WriteFile(filepath.Join(deploy, name), []byte("# "+name+"\n"), 0o600); err != nil {
			t.Fatalf("write %s: %v", name, err)
		}
	}

	running, _, err := config.Load(path)
	if err != nil {
		t.Fatalf("load: %v", err)
	}

	mux := http.NewServeMux()
	mux.Handle(controlv1connect.NewConfigServiceHandler(control.NewConfigServer(path, running, overrides, deploy)))

	server := httptest.NewServer(mux)
	t.Cleanup(server.Close)

	return configFixture{
		client: controlv1connect.NewConfigServiceClient(server.Client(), server.URL),
		path:   path,
		deploy: deploy,
	}
}

func edit(t *testing.T, key string, value any) *controlv1.ConfigEdit {
	t.Helper()

	converted, err := structpb.NewValue(value)
	if err != nil {
		t.Fatalf("value for %s: %v", key, err)
	}
	return &controlv1.ConfigEdit{Key: key, Value: converted}
}

func (f configFixture) get(t *testing.T) *controlv1.GetConfigResponse {
	t.Helper()

	resp, err := f.client.GetConfig(context.Background(), connect.NewRequest(&controlv1.GetConfigRequest{}))
	if err != nil {
		t.Fatalf("get config: %v", err)
	}
	return resp.Msg
}

func (f configFixture) onDisk(t *testing.T) string {
	t.Helper()

	content, err := os.ReadFile(f.path)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	return string(content)
}

// The page shows what the file says next to what is running, and which
// settings the file cannot change at all.
func TestGetConfigReportsTheFileTheRunningProcessAndWhatOverridesIt(t *testing.T) {
	fixture := newConfigClient(t, control.ConfigOverride{
		Key: "provider.backend", Source: "environment", Name: "JINGCLAW_PROVIDER_BACKEND",
	})

	got := fixture.get(t)

	if got.GetPath() != fixture.path || got.GetHash() != config.Hash([]byte(config.Example)) {
		t.Errorf("path %q hash %q", got.GetPath(), got.GetHash())
	}
	// The example sets only the backend; everything else is a default.
	if _, set := got.GetFile().AsMap()["provider.backend"]; !set {
		t.Errorf("the file's own setting is missing: %v", got.GetFile().AsMap())
	}
	if _, set := got.GetFile().AsMap()["provider.retry.max_attempts"]; set {
		t.Error("a default was reported as written in the file")
	}
	if got.GetRunning().AsMap()["provider.retry.max_attempts"] != float64(4) {
		t.Errorf("the running value is %v", got.GetRunning().AsMap()["provider.retry.max_attempts"])
	}
	if got.GetRunning().AsMap()["tools.command_timeout"] != "2m" {
		t.Errorf("durations are not in the form a file uses: %v", got.GetRunning().AsMap()["tools.command_timeout"])
	}

	overrides := got.GetOverrides()
	if len(overrides) != 1 || overrides[0].GetKey() != "provider.backend" || overrides[0].GetName() != "JINGCLAW_PROVIDER_BACKEND" {
		t.Errorf("overrides: %v", overrides)
	}
}

// Checking is the question "would this start?", and asking it must not change
// anything.
func TestCheckConfigAnswersWithoutWriting(t *testing.T) {
	fixture := newConfigClient(t)

	resp, err := fixture.client.CheckConfig(context.Background(), connect.NewRequest(&controlv1.CheckConfigRequest{
		Edits: []*controlv1.ConfigEdit{edit(t, "provider.retry.base_delay", "45s")},
	}))
	if err != nil {
		t.Fatalf("check: %v", err)
	}

	problems := resp.Msg.GetProblems()
	if len(problems) != 1 || problems[0].GetKey() != "provider.retry.base_delay" {
		t.Fatalf("problems: %v", problems)
	}
	if problems[0].GetWhy() == "" || problems[0].GetFix() == "" {
		t.Errorf("a problem that does not say why or what to do: %v", problems[0])
	}

	var added []string
	for _, line := range resp.Msg.GetDiff() {
		if line.GetKind() == "added" {
			added = append(added, line.GetText())
		}
	}
	if len(added) != 1 || added[0] != `base_delay = "45s"` {
		t.Errorf("diff: %v", resp.Msg.GetDiff())
	}

	if fixture.onDisk(t) != config.Example {
		t.Error("a check changed the file")
	}
}

func TestWriteConfigChangesTheLinesItWasForAndNothingElse(t *testing.T) {
	fixture := newConfigClient(t)
	before := fixture.get(t)

	resp, err := fixture.client.WriteConfig(context.Background(), connect.NewRequest(&controlv1.WriteConfigRequest{
		Hash: before.GetHash(),
		Edits: []*controlv1.ConfigEdit{
			edit(t, "provider.retry.max_attempts", float64(6)),
			edit(t, "web.enabled", true),
		},
	}))
	if err != nil {
		t.Fatalf("write: %v", err)
	}

	if !resp.Msg.GetWritten() || len(resp.Msg.GetProblems()) != 0 {
		t.Fatalf("not written: %v", resp.Msg)
	}

	after := fixture.onDisk(t)
	if resp.Msg.GetHash() != config.Hash([]byte(after)) {
		t.Error("the hash is not the file's")
	}

	// Two lines differ, and the file still loads as what was asked for.
	was, now := strings.Split(config.Example, "\n"), strings.Split(after, "\n")
	if len(was) != len(now) {
		t.Fatalf("the number of lines changed: %d to %d", len(was), len(now))
	}
	changed := 0
	for i := range was {
		if was[i] != now[i] {
			changed++
		}
	}
	if changed != 2 {
		t.Errorf("%d lines changed, want 2", changed)
	}

	cfg, _, err := config.Load(fixture.path)
	if err != nil || cfg.Provider.Retry.MaxAttempts != 6 || !cfg.Web.Enabled {
		t.Errorf("the file reads %+v, %v", cfg.Provider.Retry, err)
	}

	again := fixture.get(t)
	if again.GetHash() != resp.Msg.GetHash() || again.GetFile().AsMap()["provider.retry.max_attempts"] != float64(6) {
		t.Error("a read after the write does not see it")
	}
}

// An edit the daemon would refuse to start with is not written, and says why.
func TestWriteConfigRefusesWhatWouldNotStart(t *testing.T) {
	fixture := newConfigClient(t)

	resp, err := fixture.client.WriteConfig(context.Background(), connect.NewRequest(&controlv1.WriteConfigRequest{
		Hash:  fixture.get(t).GetHash(),
		Edits: []*controlv1.ConfigEdit{edit(t, "context.compact_at", float64(1.5))},
	}))
	if err != nil {
		t.Fatalf("write: %v", err)
	}

	if resp.Msg.GetWritten() || len(resp.Msg.GetProblems()) == 0 || resp.Msg.GetProblems()[0].GetKey() != "context.compact_at" {
		t.Fatalf("response: %v", resp.Msg)
	}
	if fixture.onDisk(t) != config.Example {
		t.Error("a refused edit changed the file")
	}
}

// Somebody editing the file in a terminal while the page is open is not
// something to write over without either of them having seen the other.
func TestWriteConfigRefusesAFileThatChangedAfterItWasRead(t *testing.T) {
	fixture := newConfigClient(t)
	started := fixture.get(t).GetHash()

	theirs := config.Example + "\n# somebody was here\n"
	if err := os.WriteFile(fixture.path, []byte(theirs), 0o600); err != nil {
		t.Fatal(err)
	}

	_, err := fixture.client.WriteConfig(context.Background(), connect.NewRequest(&controlv1.WriteConfigRequest{
		Hash:  started,
		Edits: []*controlv1.ConfigEdit{edit(t, "agent.max_iterations", float64(20))},
	}))
	if connect.CodeOf(err) != connect.CodeFailedPrecondition {
		t.Fatalf("got %v", err)
	}
	if fixture.onDisk(t) != theirs {
		t.Error("their edit was lost")
	}
}

// Whatever the file says, the variable wins. Offering to edit it would be
// offering an edit that does nothing and looks as though it worked.
func TestAnEditToWhatTheEnvironmentOverridesIsRefused(t *testing.T) {
	fixture := newConfigClient(t, control.ConfigOverride{
		Key: "provider.backend", Source: "environment", Name: "JINGCLAW_PROVIDER_BACKEND",
	})

	for name, call := range map[string]func() error{
		"check": func() error {
			_, err := fixture.client.CheckConfig(context.Background(), connect.NewRequest(&controlv1.CheckConfigRequest{
				Edits: []*controlv1.ConfigEdit{edit(t, "provider.backend", "anthropic")},
			}))
			return err
		},
		"write": func() error {
			_, err := fixture.client.WriteConfig(context.Background(), connect.NewRequest(&controlv1.WriteConfigRequest{
				Hash:  fixture.get(t).GetHash(),
				Edits: []*controlv1.ConfigEdit{edit(t, "provider.backend", "anthropic")},
			}))
			return err
		},
	} {
		err := call()
		if connect.CodeOf(err) != connect.CodeInvalidArgument || !strings.Contains(err.Error(), "JINGCLAW_PROVIDER_BACKEND") {
			t.Errorf("%s: got %v", name, err)
		}
	}
}

func TestEditsThatAreNotEditsAreRefused(t *testing.T) {
	fixture := newConfigClient(t)
	hash := fixture.get(t).GetHash()

	cases := map[string][]*controlv1.ConfigEdit{
		"no edits":         nil,
		"an unknown key":   {edit(t, "agent.nope", "x")},
		"the wrong type":   {edit(t, "agent.max_iterations", "twelve")},
		"no value at all":  {{Key: "agent.max_iterations"}},
		"a bad duration":   {edit(t, "tools.command_timeout", "soon")},
		"a section":        {edit(t, "agent", "x")},
		"a fraction count": {edit(t, "agent.max_iterations", 1.5)},
	}
	for name, edits := range cases {
		_, err := fixture.client.WriteConfig(context.Background(), connect.NewRequest(&controlv1.WriteConfigRequest{Hash: hash, Edits: edits}))
		if connect.CodeOf(err) != connect.CodeInvalidArgument {
			t.Errorf("%s: got %v", name, err)
		}
	}
	if fixture.onDisk(t) != config.Example {
		t.Error("a refused edit changed the file")
	}
}

func TestAnEditRemovedPutsTheDefaultBack(t *testing.T) {
	fixture := newConfigClient(t)

	write := func(edits ...*controlv1.ConfigEdit) {
		t.Helper()
		resp, err := fixture.client.WriteConfig(context.Background(), connect.NewRequest(&controlv1.WriteConfigRequest{
			Hash: fixture.get(t).GetHash(), Edits: edits,
		}))
		if err != nil || !resp.Msg.GetWritten() {
			t.Fatalf("write: %v %v", err, resp)
		}
	}

	write(edit(t, "agent.max_iterations", float64(30)))
	write(&controlv1.ConfigEdit{Key: "agent.max_iterations", Remove: true})

	if fixture.onDisk(t) != config.Example {
		t.Error("taking the setting back did not leave the file as it was")
	}
}

func TestInstructionFilesAreReadAndWrittenWithTheSameCare(t *testing.T) {
	fixture := newConfigClient(t)
	ctx := context.Background()

	got, err := fixture.client.GetInstructionFile(ctx, connect.NewRequest(&controlv1.GetInstructionFileRequest{
		File: controlv1.InstructionFile_INSTRUCTION_FILE_PERSONA,
	}))
	if err != nil || got.Msg.GetText() != "# PERSONA.md\n" {
		t.Fatalf("get: %v %v", err, got)
	}

	put, err := fixture.client.PutInstructionFile(ctx, connect.NewRequest(&controlv1.PutInstructionFileRequest{
		File: controlv1.InstructionFile_INSTRUCTION_FILE_PERSONA, Text: "Be brief.\n", Hash: got.Msg.GetHash(),
	}))
	if err != nil {
		t.Fatalf("put: %v", err)
	}

	onDisk, _ := os.ReadFile(filepath.Join(fixture.deploy, config.PersonaFile))
	if string(onDisk) != "Be brief.\n" || put.Msg.GetHash() != config.Hash(onDisk) {
		t.Errorf("the file is %q", onDisk)
	}

	// The same edit again starts from a version that is no longer there.
	_, err = fixture.client.PutInstructionFile(ctx, connect.NewRequest(&controlv1.PutInstructionFileRequest{
		File: controlv1.InstructionFile_INSTRUCTION_FILE_PERSONA, Text: "Stale.\n", Hash: got.Msg.GetHash(),
	}))
	if connect.CodeOf(err) != connect.CodeFailedPrecondition {
		t.Errorf("a stale write was accepted: %v", err)
	}

	_, err = fixture.client.GetInstructionFile(ctx, connect.NewRequest(&controlv1.GetInstructionFileRequest{}))
	if connect.CodeOf(err) != connect.CodeInvalidArgument {
		t.Errorf("a request that names no file: %v", err)
	}
}

// A file somebody wrote after this one was opened empty must not be replaced
// by an edit that began from nothing.
func TestAnEditThatStartedFromNothingDoesNotReplaceWhatIsThereNow(t *testing.T) {
	fixture := newConfigClient(t)

	_, err := fixture.client.PutInstructionFile(context.Background(), connect.NewRequest(&controlv1.PutInstructionFileRequest{
		File: controlv1.InstructionFile_INSTRUCTION_FILE_AGENTS, Text: "mine\n", Hash: "",
	}))
	if connect.CodeOf(err) != connect.CodeFailedPrecondition {
		t.Errorf("got %v", err)
	}
	if got, _ := os.ReadFile(filepath.Join(fixture.deploy, config.InstructionsFile)); string(got) != "# AGENTS.md\n" {
		t.Errorf("the file is %q", got)
	}
}

func TestInstructionFilesAreLimitedInSize(t *testing.T) {
	fixture := newConfigClient(t)

	got, err := fixture.client.GetInstructionFile(context.Background(), connect.NewRequest(&controlv1.GetInstructionFileRequest{
		File: controlv1.InstructionFile_INSTRUCTION_FILE_AGENTS,
	}))
	if err != nil {
		t.Fatal(err)
	}

	_, err = fixture.client.PutInstructionFile(context.Background(), connect.NewRequest(&controlv1.PutInstructionFileRequest{
		File: controlv1.InstructionFile_INSTRUCTION_FILE_AGENTS, Text: strings.Repeat("x", 1<<20+1), Hash: got.Msg.GetHash(),
	}))
	if connect.CodeOf(err) != connect.CodeInvalidArgument {
		t.Errorf("got %v", err)
	}
}
