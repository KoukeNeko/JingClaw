package control_test

import (
	"context"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"connectrpc.com/connect"
	"google.golang.org/protobuf/types/known/structpb"

	controlv1 "github.com/KoukeNeko/JingClaw/core/gen/go/jingclaw/control/v1"
	"github.com/KoukeNeko/JingClaw/core/gen/go/jingclaw/control/v1/controlv1connect"
	"github.com/KoukeNeko/JingClaw/core/internal/config"
	"github.com/KoukeNeko/JingClaw/core/internal/control"
	"github.com/KoukeNeko/JingClaw/core/internal/storage"
	"github.com/KoukeNeko/JingClaw/core/internal/storage/memory"
	"github.com/KoukeNeko/JingClaw/core/internal/storage/sqlite"
)

const (
	startedPersona = "# PERSONA\n"
	startedAgents  = "# AGENTS\n"
)

type configFixture struct {
	client controlv1connect.ConfigServiceClient
	store  storage.DocumentStore

	// server is the handler itself, for what the wire would not let through.
	server *control.ConfigServer

	// race is set for a fixture built with configOptions.racing.
	race *racer
}

// racer is a store that lets something else save in the gap between the
// service reading a document and saving its edit of it, which no test through
// the service alone can aim at.
type racer struct {
	storage.DocumentStore

	once sync.Once
	hook func()
}

func (r *racer) PutDocument(
	ctx context.Context, name, text, source, expect string, at time.Time,
) (storage.Document, error) {
	r.once.Do(func() {
		if r.hook != nil {
			r.hook()
		}
	})
	return r.DocumentStore.PutDocument(ctx, name, text, source, expect, at)
}

// configOptions is how a test differs from a daemon that started with the
// example, an agent, and nothing overriding anything.
type configOptions struct {
	overrides []control.ConfigOverride
	state     control.AgentState

	// seed replaces the document texts a daemon "started" with. An empty text
	// is a document that was never written.
	seed map[string]string

	// racing puts a racer between the service and the store.
	racing bool

	// store is where the documents are kept, in memory when nil.
	store storage.Store
}

// newConfigClient serves a store holding the three documents as a first start
// leaves them, and a daemon that started from exactly those, so that what is
// checked is the edit of saved text and nothing is pending until a test makes
// it so.
func newConfigClient(t *testing.T, overrides ...control.ConfigOverride) configFixture {
	t.Helper()

	return newConfigClientWith(t, configOptions{overrides: overrides})
}

func newConfigClientWith(t *testing.T, options configOptions) configFixture {
	t.Helper()

	texts := map[string]string{
		storage.DocumentConfig:  config.Example,
		storage.DocumentAgents:  startedAgents,
		storage.DocumentPersona: startedPersona,
	}
	for name, text := range options.seed {
		texts[name] = text
	}

	var store storage.DocumentStore = memory.New()
	if options.store != nil {
		store = options.store
	}
	var seeds []storage.DocumentSeed
	for _, name := range []string{storage.DocumentConfig, storage.DocumentAgents, storage.DocumentPersona} {
		if texts[name] != "" {
			seeds = append(seeds, storage.DocumentSeed{Name: name, Text: texts[name], Source: "default"})
		}
	}
	if _, err := store.SeedDocuments(context.Background(), seeds, time.Now()); err != nil {
		t.Fatalf("seed: %v", err)
	}

	running, err := config.Candidate([]byte(texts[storage.DocumentConfig]))
	if err != nil {
		t.Fatalf("candidate: %v", err)
	}

	started := control.StartedDocuments{
		Config:  storage.DocumentHash(texts[storage.DocumentConfig]),
		Agents:  storage.DocumentHash(texts[storage.DocumentAgents]),
		Persona: storage.DocumentHash(texts[storage.DocumentPersona]),
	}

	var served storage.DocumentStore = store
	var race *racer
	if options.racing {
		race = &racer{DocumentStore: store}
		served = race
	}

	handler := control.NewConfigServer(served, running, options.overrides, started, options.state)

	mux := http.NewServeMux()
	mux.Handle(controlv1connect.NewConfigServiceHandler(handler))

	server := httptest.NewServer(mux)
	t.Cleanup(server.Close)

	return configFixture{
		client: controlv1connect.NewConfigServiceClient(server.Client(), server.URL),
		store:  store,
		server: handler,
		race:   race,
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

// saved is the document as the database holds it, which is what a restart
// would read.
func (f configFixture) saved(t *testing.T, name string) storage.Document {
	t.Helper()

	doc, err := f.store.Document(context.Background(), name)
	if err != nil {
		t.Fatalf("read %s: %v", name, err)
	}
	return doc
}

// elsewhere saves a document the way another connection to the same database
// would, behind the back of the service under test.
func (f configFixture) elsewhere(t *testing.T, name, text string) {
	t.Helper()

	doc := f.saved(t, name)
	if _, err := f.store.PutDocument(context.Background(), name, text, "file", doc.Hash, time.Now()); err != nil {
		t.Fatalf("save %s elsewhere: %v", name, err)
	}
}

func (f configFixture) getInstruction(t *testing.T, file controlv1.InstructionFile) *controlv1.GetInstructionFileResponse {
	t.Helper()

	resp, err := f.client.GetInstructionFile(context.Background(), connect.NewRequest(&controlv1.GetInstructionFileRequest{File: file}))
	if err != nil {
		t.Fatalf("get %v: %v", file, err)
	}
	return resp.Msg
}

func (f configFixture) putInstruction(
	file controlv1.InstructionFile, text, hash string,
) (*controlv1.PutInstructionFileResponse, error) {
	resp, err := f.client.PutInstructionFile(context.Background(), connect.NewRequest(&controlv1.PutInstructionFileRequest{
		File: file, Text: text, Hash: hash,
	}))
	if err != nil {
		return nil, err
	}
	return resp.Msg, nil
}

func (f configFixture) replace(text, hash string) (*controlv1.ReplaceConfigTextResponse, error) {
	resp, err := f.client.ReplaceConfigText(context.Background(), connect.NewRequest(&controlv1.ReplaceConfigTextRequest{
		Text: text, Hash: hash,
	}))
	if err != nil {
		return nil, err
	}
	return resp.Msg, nil
}

// The page shows what is saved next to what is running, and which settings the
// saved text cannot change at all.
func TestGetConfigReportsTheSavedTextTheRunningProcessAndWhatOverridesIt(t *testing.T) {
	fixture := newConfigClient(t, control.ConfigOverride{
		Key: "provider.backend", Source: "environment", Name: "JINGCLAW_PROVIDER_BACKEND",
	})

	got := fixture.get(t)

	if got.GetHash() != config.Hash([]byte(config.Example)) || got.GetText() != config.Example {
		t.Errorf("hash %q, and the text is not the example", got.GetHash())
	}
	if got.GetPath() != "" {
		t.Errorf("a path %q for settings that are not in a file", got.GetPath())
	}
	if got.GetVersion() != 1 || got.GetParseError() != "" {
		t.Errorf("version %d, parse error %q", got.GetVersion(), got.GetParseError())
	}
	// What started is what is saved, so nothing waits for a restart.
	if got.GetRunningHash() != got.GetHash() || got.GetPendingRestart() {
		t.Errorf("running %q, saved %q, pending %v", got.GetRunningHash(), got.GetHash(), got.GetPendingRestart())
	}

	// The example sets only the backend; everything else is a default.
	if _, set := got.GetFile().AsMap()["provider.backend"]; !set {
		t.Errorf("the saved text's own setting is missing: %v", got.GetFile().AsMap())
	}
	if _, set := got.GetFile().AsMap()["provider.retry.max_attempts"]; set {
		t.Error("a default was reported as written in the saved text")
	}
	if got.GetRunning().AsMap()["provider.retry.max_attempts"] != float64(4) {
		t.Errorf("the running value is %v", got.GetRunning().AsMap()["provider.retry.max_attempts"])
	}
	if got.GetRunning().AsMap()["tools.command_timeout"] != "2m" {
		t.Errorf("durations are not in the form a text uses: %v", got.GetRunning().AsMap()["tools.command_timeout"])
	}

	overrides := got.GetOverrides()
	if len(overrides) != 1 || overrides[0].GetKey() != "provider.backend" || overrides[0].GetName() != "JINGCLAW_PROVIDER_BACKEND" {
		t.Errorf("overrides: %v", overrides)
	}
}

// A config that does not parse is exactly the one that has to be shown, so the
// page that repairs it can put it in front of the operator.
func TestGetConfigStillAnswersForTextThatDoesNotParse(t *testing.T) {
	fixture := newConfigClient(t)
	broken := "[agent\nmax_iterations = 4\n"
	fixture.elsewhere(t, storage.DocumentConfig, broken)

	got := fixture.get(t)

	if got.GetText() != broken || got.GetHash() != config.Hash([]byte(broken)) {
		t.Errorf("the text is %q", got.GetText())
	}
	if got.GetParseError() == "" {
		t.Error("text that does not parse came back without saying so")
	}
	if len(got.GetFile().AsMap()) != 0 {
		t.Errorf("settings were read out of text that does not parse: %v", got.GetFile().AsMap())
	}
	// And it is not what is running, which the page must also be able to say.
	if !got.GetPendingRestart() || got.GetVersion() != 2 {
		t.Errorf("pending %v, version %d", got.GetPendingRestart(), got.GetVersion())
	}
	if got.GetRunning().AsMap()["agent.max_iterations"] != float64(12) {
		t.Errorf("what is running was lost: %v", got.GetRunning().AsMap()["agent.max_iterations"])
	}
}

// Edits are applied to text, so text they cannot be applied to is the operator's
// to fix with the whole of it, not an internal fault.
func TestAnEditCannotBeAppliedToTextThatDoesNotParse(t *testing.T) {
	fixture := newConfigClient(t)
	fixture.elsewhere(t, storage.DocumentConfig, "[agent\n")
	hash := fixture.get(t).GetHash()

	_, err := fixture.client.CheckConfig(context.Background(), connect.NewRequest(&controlv1.CheckConfigRequest{
		Edits: []*controlv1.ConfigEdit{edit(t, "agent.max_iterations", float64(3))},
	}))
	if connect.CodeOf(err) != connect.CodeInvalidArgument {
		t.Errorf("check: got %v", err)
	}

	_, err = fixture.client.WriteConfig(context.Background(), connect.NewRequest(&controlv1.WriteConfigRequest{
		Hash: hash, Edits: []*controlv1.ConfigEdit{edit(t, "agent.max_iterations", float64(3))},
	}))
	if connect.CodeOf(err) != connect.CodeInvalidArgument {
		t.Errorf("write: got %v", err)
	}
	if fixture.saved(t, storage.DocumentConfig).Text != "[agent\n" {
		t.Error("a refused edit changed the text")
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

	if doc := fixture.saved(t, storage.DocumentConfig); doc.Text != config.Example || doc.Version != 1 {
		t.Errorf("a check changed the config: version %d", doc.Version)
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

	doc := fixture.saved(t, storage.DocumentConfig)
	after := doc.Text
	if resp.Msg.GetHash() != config.Hash([]byte(after)) || resp.Msg.GetHash() != doc.Hash {
		t.Error("the hash is not the saved text's")
	}
	if resp.Msg.GetVersion() != 2 || doc.Source != "web" {
		t.Errorf("version %d, source %q", resp.Msg.GetVersion(), doc.Source)
	}
	// Saved, and not running: the daemon reads its settings once.
	if !resp.Msg.GetPendingRestart() {
		t.Error("a saved change was not reported as waiting for a restart")
	}

	// Two lines differ, and the text still loads as what was asked for.
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

	cfg, err := config.Candidate([]byte(after))
	if err != nil || cfg.Provider.Retry.MaxAttempts != 6 || !cfg.Web.Enabled {
		t.Errorf("the text reads %+v, %v", cfg.Provider.Retry, err)
	}

	again := fixture.get(t)
	if again.GetHash() != resp.Msg.GetHash() || again.GetFile().AsMap()["provider.retry.max_attempts"] != float64(6) {
		t.Error("a read after the write does not see it")
	}
	// The process still says what it started from.
	if again.GetRunningHash() != before.GetHash() || again.GetRunning().AsMap()["provider.retry.max_attempts"] != float64(4) {
		t.Errorf("what is running changed: %v", again.GetRunning().AsMap()["provider.retry.max_attempts"])
	}
}

// The text is the operator's. What they wrote in it — their comments, their
// order, a setting a later version will know about — is still there after an
// edit made from the page.
func TestWriteConfigKeepsTheOperatorsCommentsAndSettingsThisVersionDoesNotKnow(t *testing.T) {
	mine := "# Notes from the person who runs this.\n" +
		"[agent]\n" +
		"max_iterations = 8 # enough for the nightly job\n" +
		"\n" +
		"# Added in a version newer than this one.\n" +
		"[from_the_future]\n" +
		"switch = true\n"
	fixture := newConfigClientWith(t, configOptions{seed: map[string]string{storage.DocumentConfig: mine}})

	resp, err := fixture.client.WriteConfig(context.Background(), connect.NewRequest(&controlv1.WriteConfigRequest{
		Hash:  fixture.get(t).GetHash(),
		Edits: []*controlv1.ConfigEdit{edit(t, "agent.permission_profile", "gateway")},
	}))
	if err != nil || !resp.Msg.GetWritten() {
		t.Fatalf("write: %v %v", resp, err)
	}

	after := fixture.saved(t, storage.DocumentConfig).Text
	for _, kept := range []string{
		"# Notes from the person who runs this.\n",
		"max_iterations = 8 # enough for the nightly job\n",
		"# Added in a version newer than this one.\n[from_the_future]\nswitch = true\n",
	} {
		if !strings.Contains(after, kept) {
			t.Errorf("%q was lost from:\n%s", kept, after)
		}
	}
	if !strings.Contains(after, `permission_profile = "gateway"`) {
		t.Errorf("the edit is not in:\n%s", after)
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
	if doc := fixture.saved(t, storage.DocumentConfig); doc.Text != config.Example || doc.Version != 1 {
		t.Errorf("a refused edit changed the config: version %d", doc.Version)
	}
}

// Somebody saving the same document from another place while the page is open
// is not something to write over without either of them having seen the other.
func TestWriteConfigRefusesATextThatChangedAfterItWasRead(t *testing.T) {
	fixture := newConfigClient(t)
	started := fixture.get(t).GetHash()

	theirs := config.Example + "\n# somebody was here\n"
	fixture.elsewhere(t, storage.DocumentConfig, theirs)

	_, err := fixture.client.WriteConfig(context.Background(), connect.NewRequest(&controlv1.WriteConfigRequest{
		Hash:  started,
		Edits: []*controlv1.ConfigEdit{edit(t, "agent.max_iterations", float64(20))},
	}))
	if connect.CodeOf(err) != connect.CodeFailedPrecondition {
		t.Fatalf("got %v", err)
	}
	if fixture.saved(t, storage.DocumentConfig).Text != theirs {
		t.Error("their edit was lost")
	}
}

// Nothing makes the second of two writers that read the same version wait for
// the first, so what stops it from discarding the first's change is the
// comparison in the store. Many at once, from the same starting point, and
// exactly one of them is the edit that started from the text that is there.
func TestOnlyOneOfSeveralEditsFromTheSameVersionIsSaved(t *testing.T) {
	fixture := newConfigClient(t)
	hash := fixture.get(t).GetHash()

	const writers = 12
	var (
		wg       sync.WaitGroup
		mu       sync.Mutex
		saved    int
		refused  int
		surprise []error
	)
	for i := range writers {
		edits := []*controlv1.ConfigEdit{edit(t, "agent.max_iterations", float64(20+i))}

		wg.Add(1)
		go func() {
			defer wg.Done()

			resp, err := fixture.client.WriteConfig(context.Background(), connect.NewRequest(&controlv1.WriteConfigRequest{
				Hash: hash, Edits: edits,
			}))

			mu.Lock()
			defer mu.Unlock()
			switch {
			case err == nil && resp.Msg.GetWritten():
				saved++
			case connect.CodeOf(err) == connect.CodeFailedPrecondition:
				refused++
			default:
				surprise = append(surprise, err)
			}
		}()
	}
	wg.Wait()

	if saved != 1 || refused != writers-1 || len(surprise) != 0 {
		t.Errorf("%d saved, %d refused, unexpected: %v", saved, refused, surprise)
	}
	if doc := fixture.saved(t, storage.DocumentConfig); doc.Version != 2 {
		t.Errorf("the config is at version %d, want exactly one more than it started at", doc.Version)
	}
}

// The same, aimed: somebody saves after this call has read the document and
// before it has saved its own. The hash it was sent matched what it read, so
// only the store's comparison can catch it, and it must.
func TestAWriteThatLosesTheRaceBetweenReadingAndSavingIsRefused(t *testing.T) {
	fixture := newConfigClientWith(t, configOptions{racing: true})
	hash := fixture.get(t).GetHash()
	theirs := config.Example + "\n# somebody was here\n"

	agents := controlv1.InstructionFile_INSTRUCTION_FILE_AGENTS
	agentsHash := fixture.getInstruction(t, agents).GetHash()

	for name, call := range map[string]struct {
		document string
		do       func() error
	}{
		"edits": {storage.DocumentConfig, func() error {
			_, err := fixture.client.WriteConfig(context.Background(), connect.NewRequest(&controlv1.WriteConfigRequest{
				Hash: hash, Edits: []*controlv1.ConfigEdit{edit(t, "agent.max_iterations", float64(20))},
			}))
			return err
		}},
		"a replacement": {storage.DocumentConfig, func() error {
			_, err := fixture.replace("[agent]\nmax_iterations = 3\n", hash)
			return err
		}},
		"an instruction file": {storage.DocumentAgents, func() error {
			_, err := fixture.putInstruction(agents, "mine\n", agentsHash)
			return err
		}},
	} {
		fixture.race.once = sync.Once{}
		fixture.race.hook = func() { fixture.elsewhere(t, call.document, theirs) }

		if err := call.do(); connect.CodeOf(err) != connect.CodeFailedPrecondition {
			t.Errorf("%s: got %v", name, err)
		}
		if got := fixture.saved(t, call.document).Text; got != theirs {
			t.Errorf("%s: their save was lost, the document is %q", name, got)
		}

		// Put back for the next, so each starts from the same document.
		doc := fixture.saved(t, call.document)
		original := map[string]string{storage.DocumentConfig: config.Example, storage.DocumentAgents: startedAgents}[call.document]
		if _, err := fixture.store.PutDocument(context.Background(), call.document, original, "file", doc.Hash, time.Now()); err != nil {
			t.Fatalf("restore: %v", err)
		}
		hash = storage.DocumentHash(config.Example)
		agentsHash = storage.DocumentHash(startedAgents)
	}
}

// Whatever the saved text says, the variable wins. Offering to edit it would be
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
	if doc := fixture.saved(t, storage.DocumentConfig); doc.Text != config.Example || doc.Version != 1 {
		t.Errorf("a refused edit changed the config: version %d", doc.Version)
	}
}

// Where the settings are kept cannot be one of them, so an edit to either is a
// problem with the edit, said beside any others, and nothing is saved.
func TestAnEditToABootstrapKeyIsAProblemAndIsNeverSaved(t *testing.T) {
	fixture := newConfigClient(t)
	hash := fixture.get(t).GetHash()

	checked, err := fixture.client.CheckConfig(context.Background(), connect.NewRequest(&controlv1.CheckConfigRequest{
		Edits: []*controlv1.ConfigEdit{
			edit(t, "server.data_dir", "/elsewhere"),
			edit(t, "agent.max_iterations", float64(3)),
		},
	}))
	if err != nil {
		t.Fatalf("check: %v", err)
	}
	problems := checked.Msg.GetProblems()
	if len(problems) != 1 || problems[0].GetKey() != "server.data_dir" {
		t.Fatalf("problems: %v", problems)
	}
	if !strings.Contains(problems[0].GetFix(), "JINGCLAW_SERVER_DATA_DIR") || !strings.Contains(problems[0].GetFix(), "--data-dir") {
		t.Errorf("the problem does not say how to give it instead: %v", problems[0])
	}
	// The edit beside it is still worked out, so one answer says everything.
	if len(checked.Msg.GetDiff()) == 0 {
		t.Error("the other edit was not previewed")
	}

	for _, key := range []string{"server.data_dir", "server.runtime_dir"} {
		written, err := fixture.client.WriteConfig(context.Background(), connect.NewRequest(&controlv1.WriteConfigRequest{
			Hash: hash,
			Edits: []*controlv1.ConfigEdit{
				edit(t, "agent.max_iterations", float64(3)),
				edit(t, key, "/elsewhere"),
			},
		}))
		if err != nil {
			t.Fatalf("write %s: %v", key, err)
		}
		if written.Msg.GetWritten() || len(written.Msg.GetProblems()) != 1 || written.Msg.GetProblems()[0].GetKey() != key {
			t.Errorf("%s: %v", key, written.Msg)
		}
	}

	// Taking one back is no different: the text is not where it lives.
	removed, err := fixture.client.WriteConfig(context.Background(), connect.NewRequest(&controlv1.WriteConfigRequest{
		Hash: hash, Edits: []*controlv1.ConfigEdit{{Key: "server.runtime_dir", Remove: true}},
	}))
	if err != nil || removed.Msg.GetWritten() || len(removed.Msg.GetProblems()) != 1 {
		t.Errorf("taking one back: %v %v", removed, err)
	}

	if doc := fixture.saved(t, storage.DocumentConfig); doc.Text != config.Example || doc.Version != 1 {
		t.Errorf("a refused edit changed the config: version %d", doc.Version)
	}
}

func TestAnEditRemovedPutsTheDefaultBack(t *testing.T) {
	fixture := newConfigClient(t)

	write := func(edits ...*controlv1.ConfigEdit) *controlv1.WriteConfigResponse {
		t.Helper()
		resp, err := fixture.client.WriteConfig(context.Background(), connect.NewRequest(&controlv1.WriteConfigRequest{
			Hash: fixture.get(t).GetHash(), Edits: edits,
		}))
		if err != nil || !resp.Msg.GetWritten() {
			t.Fatalf("write: %v %v", err, resp)
		}
		return resp.Msg
	}

	if !write(edit(t, "agent.max_iterations", float64(30))).GetPendingRestart() {
		t.Error("a change was not reported as waiting for a restart")
	}
	back := write(&controlv1.ConfigEdit{Key: "agent.max_iterations", Remove: true})

	if fixture.saved(t, storage.DocumentConfig).Text != config.Example {
		t.Error("taking the setting back did not leave the text as it was")
	}
	// Back to the text the process started from, so there is nothing to restart
	// for, though it is not the first version.
	if back.GetPendingRestart() || back.GetVersion() != 3 {
		t.Errorf("pending %v, version %d", back.GetPendingRestart(), back.GetVersion())
	}
}

// A save that says what is already there is not a new version, or the button
// pressed without an edit would push a real earlier version out of the twenty.
func TestSavingWhatIsAlreadyThereAddsNoVersion(t *testing.T) {
	fixture := newConfigClient(t)
	hash := fixture.get(t).GetHash()

	resp, err := fixture.replace(config.Example, hash)
	if err != nil || !resp.GetWritten() || resp.GetVersion() != 1 || resp.GetHash() != hash {
		t.Fatalf("replace: %v %v", resp, err)
	}

	persona := fixture.getInstruction(t, controlv1.InstructionFile_INSTRUCTION_FILE_PERSONA)
	put, err := fixture.putInstruction(controlv1.InstructionFile_INSTRUCTION_FILE_PERSONA, startedPersona, persona.GetHash())
	if err != nil || put.GetVersion() != 1 {
		t.Fatalf("put: %v %v", put, err)
	}

	versions, err := fixture.client.ListDocumentVersions(context.Background(), connect.NewRequest(&controlv1.ListDocumentVersionsRequest{
		Document: controlv1.SettingsDocument_SETTINGS_DOCUMENT_CONFIG,
	}))
	if err != nil || len(versions.Msg.GetVersions()) != 1 {
		t.Errorf("versions: %v %v", versions, err)
	}
}

// ── replacing the whole text ───────────────────────────────────────────

func TestReplaceConfigTextSavesText(t *testing.T) {
	fixture := newConfigClient(t)

	replacement := "# mine\n[agent]\nmax_iterations = 7\n\n[unknown_to_this_version]\nx = 1\n"
	resp, err := fixture.replace(replacement, fixture.get(t).GetHash())
	if err != nil {
		t.Fatalf("replace: %v", err)
	}

	if !resp.GetWritten() || len(resp.GetProblems()) != 0 {
		t.Fatalf("not written: %v", resp)
	}
	doc := fixture.saved(t, storage.DocumentConfig)
	if doc.Text != replacement || doc.Source != "web" || resp.GetHash() != config.Hash([]byte(replacement)) {
		t.Errorf("saved %q from %q", doc.Text, doc.Source)
	}
	if !resp.GetPendingRestart() || resp.GetVersion() != 2 {
		t.Errorf("pending %v, version %d", resp.GetPendingRestart(), resp.GetVersion())
	}
}

// The check an edit gets, on the whole text: what would not start is not saved.
func TestReplaceConfigTextRefusesWhatWouldNotStart(t *testing.T) {
	fixture := newConfigClient(t)
	hash := fixture.get(t).GetHash()

	resp, err := fixture.replace("[agent]\nmax_iterations = 0\n", hash)
	if err != nil {
		t.Fatalf("replace: %v", err)
	}

	if resp.GetWritten() || len(resp.GetProblems()) != 1 || resp.GetProblems()[0].GetKey() != "agent.max_iterations" {
		t.Fatalf("response: %v", resp)
	}
	if resp.GetProblems()[0].GetWhy() == "" || resp.GetProblems()[0].GetFix() == "" {
		t.Errorf("a problem that does not say why or what to do: %v", resp.GetProblems()[0])
	}
	if doc := fixture.saved(t, storage.DocumentConfig); doc.Text != config.Example || doc.Version != 1 || resp.GetHash() != hash {
		t.Errorf("a refused replacement changed the config: version %d", doc.Version)
	}
}

// Text that cannot be read at all is the commonest mistake in repairing a
// config, and is answered like the rest: as problems beside the editor.
func TestReplaceConfigTextRefusesTextThatCannotBeRead(t *testing.T) {
	fixture := newConfigClient(t)
	hash := fixture.get(t).GetHash()

	for name, text := range map[string]string{
		"not TOML":                     "[agent\nmax_iterations = 4\n",
		"the wrong type":               "[agent]\nmax_iterations = \"twelve\"\n",
		"a duplicate key":              "[agent]\nmax_iterations = 1\nmax_iterations = 2\n",
		"a line that is not a setting": "this is not a setting\n",
	} {
		resp, err := fixture.replace(text, hash)
		if err != nil {
			t.Errorf("%s: %v", name, err)
			continue
		}
		if resp.GetWritten() || len(resp.GetProblems()) == 0 || resp.GetProblems()[0].GetWhy() == "" {
			t.Errorf("%s: response %v", name, resp)
		}
	}
	if doc := fixture.saved(t, storage.DocumentConfig); doc.Text != config.Example || doc.Version != 1 {
		t.Errorf("a refused replacement changed the config: version %d", doc.Version)
	}
}

// A text that sets where the settings are kept is refused whole, and each key
// is named.
func TestReplaceConfigTextRefusesTextThatSetsABootstrapKey(t *testing.T) {
	fixture := newConfigClient(t)

	resp, err := fixture.replace("[server]\ndata_dir = \"/a\"\nruntime_dir = \"/b\"\n", fixture.get(t).GetHash())
	if err != nil {
		t.Fatalf("replace: %v", err)
	}

	if resp.GetWritten() || len(resp.GetProblems()) != 2 ||
		resp.GetProblems()[0].GetKey() != "server.data_dir" || resp.GetProblems()[1].GetKey() != "server.runtime_dir" {
		t.Fatalf("response: %v", resp)
	}
	if fixture.saved(t, storage.DocumentConfig).Version != 1 {
		t.Error("a refused replacement changed the config")
	}
}

// The reason this call exists: text that edits cannot be applied to can still
// be replaced, and what replaces it is checked, not trusted.
func TestReplaceConfigTextRepairsTextThatDoesNotParse(t *testing.T) {
	fixture := newConfigClient(t)
	fixture.elsewhere(t, storage.DocumentConfig, "[agent\nmax_iterations = 4\n")
	hash := fixture.get(t).GetHash()

	if resp, err := fixture.replace("[agent\nstill broken\n", hash); err != nil || resp.GetWritten() {
		t.Fatalf("broken text replaced broken text: %v %v", resp, err)
	}

	resp, err := fixture.replace(config.Example, hash)
	if err != nil || !resp.GetWritten() {
		t.Fatalf("replace: %v %v", resp, err)
	}
	if got := fixture.get(t); got.GetParseError() != "" || got.GetText() != config.Example {
		t.Errorf("after the repair: %q, %q", got.GetText(), got.GetParseError())
	}
}

func TestReplaceConfigTextRefusesAStaleHashAndTextNoDocumentTakes(t *testing.T) {
	fixture := newConfigClient(t)
	started := fixture.get(t).GetHash()
	fixture.elsewhere(t, storage.DocumentConfig, config.Example+"\n# theirs\n")

	if _, err := fixture.replace(config.Example+"\n# mine\n", started); connect.CodeOf(err) != connect.CodeFailedPrecondition {
		t.Errorf("a stale replacement: %v", err)
	}

	current := fixture.get(t).GetHash()
	if _, err := fixture.replace("# "+strings.Repeat("x", 1<<20)+"\n", current); connect.CodeOf(err) != connect.CodeInvalidArgument {
		t.Errorf("too large: got %v", err)
	}

	// The wire refuses a string that is not UTF-8 before the handler is
	// reached, so this is the handler's own check, asked directly.
	_, err := fixture.server.ReplaceConfigText(context.Background(), connect.NewRequest(&controlv1.ReplaceConfigTextRequest{
		Text: "# \xff\xfe\n", Hash: current,
	}))
	if connect.CodeOf(err) != connect.CodeInvalidArgument {
		t.Errorf("not UTF-8: got %v", err)
	}
	if !strings.HasSuffix(fixture.saved(t, storage.DocumentConfig).Text, "# theirs\n") {
		t.Error("their text was lost")
	}
}

// ── the two instruction documents ──────────────────────────────────────

func TestInstructionFilesAreReadAndWrittenWithTheSameCare(t *testing.T) {
	fixture := newConfigClient(t)
	ctx := context.Background()

	got := fixture.getInstruction(t, controlv1.InstructionFile_INSTRUCTION_FILE_PERSONA)
	if got.GetText() != startedPersona || got.GetVersion() != 1 || got.GetPath() != "" {
		t.Fatalf("get: %v", got)
	}

	put, err := fixture.putInstruction(controlv1.InstructionFile_INSTRUCTION_FILE_PERSONA, "Be brief.\n", got.GetHash())
	if err != nil {
		t.Fatalf("put: %v", err)
	}

	saved := fixture.saved(t, storage.DocumentPersona)
	if saved.Text != "Be brief.\n" || put.GetHash() != config.Hash([]byte(saved.Text)) || saved.Source != "web" {
		t.Errorf("the document is %q", saved.Text)
	}
	if put.GetVersion() != 2 || !put.GetPendingRestart() {
		t.Errorf("version %d, pending %v", put.GetVersion(), put.GetPendingRestart())
	}
	// The other document is not the one that was written.
	if fixture.saved(t, storage.DocumentAgents).Text != startedAgents {
		t.Error("writing the persona changed the agents document")
	}

	// The same edit again starts from a version that is no longer there.
	_, err = fixture.putInstruction(controlv1.InstructionFile_INSTRUCTION_FILE_PERSONA, "Stale.\n", got.GetHash())
	if connect.CodeOf(err) != connect.CodeFailedPrecondition {
		t.Errorf("a stale write was accepted: %v", err)
	}

	_, err = fixture.client.GetInstructionFile(ctx, connect.NewRequest(&controlv1.GetInstructionFileRequest{}))
	if connect.CodeOf(err) != connect.CodeInvalidArgument {
		t.Errorf("a request that names no file: %v", err)
	}
	_, err = fixture.putInstruction(controlv1.InstructionFile_INSTRUCTION_FILE_UNSPECIFIED, "x", "")
	if connect.CodeOf(err) != connect.CodeInvalidArgument {
		t.Errorf("a write that names no file: %v", err)
	}
}

// A document somebody saved after this one was opened empty must not be
// replaced by an edit that began from nothing.
func TestAnEditThatStartedFromNothingDoesNotReplaceWhatIsThereNow(t *testing.T) {
	fixture := newConfigClient(t)

	_, err := fixture.putInstruction(controlv1.InstructionFile_INSTRUCTION_FILE_AGENTS, "mine\n", "")
	if connect.CodeOf(err) != connect.CodeFailedPrecondition {
		t.Errorf("got %v", err)
	}
	if got := fixture.saved(t, storage.DocumentAgents).Text; got != startedAgents {
		t.Errorf("the document is %q", got)
	}
}

// A deployment that never wrote a persona has none, and that reads as empty and
// can be started from, like a document that was cleared.
func TestADocumentThatWasNeverWrittenReadsAsEmptyAndTakesAFirstEdit(t *testing.T) {
	fixture := newConfigClientWith(t, configOptions{seed: map[string]string{
		storage.DocumentPersona: "",
	}})
	persona := controlv1.InstructionFile_INSTRUCTION_FILE_PERSONA

	empty := fixture.getInstruction(t, persona)
	if empty.GetText() != "" || empty.GetHash() != "" || empty.GetRunningHash() != "" || empty.GetPendingRestart() {
		t.Fatalf("an empty document reads as %v", empty)
	}

	put, err := fixture.putInstruction(persona, "Now I exist.\n", "")
	if err != nil || !put.GetPendingRestart() {
		t.Fatalf("the first edit: %v %v", put, err)
	}

	// And clearing it again is a document that says nothing, not an error and
	// not nothing left to start from.
	cleared, err := fixture.putInstruction(persona, "", put.GetHash())
	if err != nil || cleared.GetHash() != "" || cleared.GetPendingRestart() {
		t.Fatalf("clearing it: %v %v", cleared, err)
	}
	if _, err := fixture.putInstruction(persona, "Again.\n", ""); err != nil {
		t.Errorf("an edit that started from the cleared document: %v", err)
	}
}

func TestInstructionFilesAreLimitedInSizeAndToText(t *testing.T) {
	fixture := newConfigClient(t)
	agents := controlv1.InstructionFile_INSTRUCTION_FILE_AGENTS
	hash := fixture.getInstruction(t, agents).GetHash()

	if _, err := fixture.putInstruction(agents, strings.Repeat("x", 1<<20+1), hash); connect.CodeOf(err) != connect.CodeInvalidArgument {
		t.Errorf("too large: got %v", err)
	}

	// The wire refuses a string that is not UTF-8 before the handler is
	// reached, so this is the handler's own check, asked directly.
	_, err := fixture.server.PutInstructionFile(context.Background(), connect.NewRequest(&controlv1.PutInstructionFileRequest{
		File: agents, Text: "# \xc3\x28 is not text\n", Hash: hash,
	}))
	if connect.CodeOf(err) != connect.CodeInvalidArgument {
		t.Errorf("not UTF-8: got %v", err)
	}

	// The limit is on the most that is taken, not one byte less.
	if _, err := fixture.putInstruction(agents, strings.Repeat("x", 1<<20), hash); err != nil {
		t.Errorf("a document at the limit: %v", err)
	}
	if fixture.saved(t, storage.DocumentAgents).Version != 2 {
		t.Error("only the document at the limit should have been saved")
	}
}

// What is saved is not what is running until the daemon starts again, and a
// text put back to what the process started with is the one that is running.
func TestPendingRestartFlipsWithTheSavedTextAndClearsWhenItReturns(t *testing.T) {
	fixture := newConfigClient(t)
	persona := controlv1.InstructionFile_INSTRUCTION_FILE_PERSONA

	started := fixture.getInstruction(t, persona)
	if started.GetPendingRestart() || started.GetRunningHash() != started.GetHash() {
		t.Fatalf("the document the process started from is pending: %v", started)
	}

	changed, err := fixture.putInstruction(persona, "Different.\n", started.GetHash())
	if err != nil || !changed.GetPendingRestart() {
		t.Fatalf("a change: %v %v", changed, err)
	}
	read := fixture.getInstruction(t, persona)
	if !read.GetPendingRestart() || read.GetRunningHash() != started.GetHash() || read.GetHash() != changed.GetHash() {
		t.Errorf("reading it back: %v", read)
	}

	back, err := fixture.putInstruction(persona, startedPersona, changed.GetHash())
	if err != nil || back.GetPendingRestart() || back.GetVersion() != 3 {
		t.Fatalf("putting it back: %v %v", back, err)
	}
	if read := fixture.getInstruction(t, persona); read.GetPendingRestart() {
		t.Error("a document back at the text the process started with is still pending")
	}
}

// ── history ────────────────────────────────────────────────────────────

func TestVersionsAreListedNewestFirstAndReadBack(t *testing.T) {
	fixture := newConfigClient(t)
	ctx := context.Background()
	persona := controlv1.InstructionFile_INSTRUCTION_FILE_PERSONA

	hash := fixture.getInstruction(t, persona).GetHash()
	for _, text := range []string{"Second.\n", "Third.\n"} {
		put, err := fixture.putInstruction(persona, text, hash)
		if err != nil {
			t.Fatalf("put: %v", err)
		}
		hash = put.GetHash()
	}

	listed, err := fixture.client.ListDocumentVersions(ctx, connect.NewRequest(&controlv1.ListDocumentVersionsRequest{
		Document: controlv1.SettingsDocument_SETTINGS_DOCUMENT_PERSONA,
	}))
	if err != nil {
		t.Fatalf("list: %v", err)
	}

	versions := listed.Msg.GetVersions()
	if len(versions) != 3 {
		t.Fatalf("versions: %v", versions)
	}
	for i, want := range []struct {
		version int64
		source  string
		hash    string
	}{
		{3, "web", config.Hash([]byte("Third.\n"))},
		{2, "web", config.Hash([]byte("Second.\n"))},
		{1, "default", config.Hash([]byte(startedPersona))},
	} {
		got := versions[i]
		if got.GetVersion() != want.version || got.GetSource() != want.source || got.GetHash() != want.hash || got.GetSavedAt() == nil {
			t.Errorf("version %d of the list: %v", i, got)
		}
	}
	if versions[0].GetHash() != hash {
		t.Error("the first in the list is not the current document")
	}

	read, err := fixture.client.GetDocumentVersion(ctx, connect.NewRequest(&controlv1.GetDocumentVersionRequest{
		Document: controlv1.SettingsDocument_SETTINGS_DOCUMENT_PERSONA, Version: 1,
	}))
	if err != nil || read.Msg.GetText() != startedPersona || read.Msg.GetInfo().GetVersion() != 1 {
		t.Fatalf("read the first: %v %v", read, err)
	}

	// Restoring is saving that text again: a new version, and nothing lost.
	restored, err := fixture.putInstruction(persona, read.Msg.GetText(), hash)
	if err != nil || restored.GetVersion() != 4 || restored.GetPendingRestart() {
		t.Fatalf("restore: %v %v", restored, err)
	}

	// Another document's history is its own.
	other, err := fixture.client.ListDocumentVersions(ctx, connect.NewRequest(&controlv1.ListDocumentVersionsRequest{
		Document: controlv1.SettingsDocument_SETTINGS_DOCUMENT_AGENTS,
	}))
	if err != nil || len(other.Msg.GetVersions()) != 1 {
		t.Errorf("the agents history: %v %v", other, err)
	}
}

func TestAVersionThatWasNeverKeptOrADocumentNeverNamedIsRefused(t *testing.T) {
	fixture := newConfigClient(t)
	ctx := context.Background()

	_, err := fixture.client.GetDocumentVersion(ctx, connect.NewRequest(&controlv1.GetDocumentVersionRequest{
		Document: controlv1.SettingsDocument_SETTINGS_DOCUMENT_CONFIG, Version: 99,
	}))
	if connect.CodeOf(err) != connect.CodeNotFound {
		t.Errorf("a version never written: %v", err)
	}

	_, err = fixture.client.GetDocumentVersion(ctx, connect.NewRequest(&controlv1.GetDocumentVersionRequest{Version: 1}))
	if connect.CodeOf(err) != connect.CodeInvalidArgument {
		t.Errorf("a request that names no document: %v", err)
	}
	_, err = fixture.client.ListDocumentVersions(ctx, connect.NewRequest(&controlv1.ListDocumentVersionsRequest{}))
	if connect.CodeOf(err) != connect.CodeInvalidArgument {
		t.Errorf("a list that names no document: %v", err)
	}
}

// ── on the real database ───────────────────────────────────────────────

// Everything above runs against the in-memory store, which the storage tests
// hold to the same contract as the database. This is the same service on the
// database itself, for the parts that depend on it: one statement doing the
// comparison and the write, and an absent row reading as empty.
func TestTheServiceKeepsItsDocumentsInTheDatabase(t *testing.T) {
	ctx := context.Background()
	store, err := sqlite.Open(ctx, filepath.Join(t.TempDir(), "jingclaw.db"))
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	t.Cleanup(func() { _ = store.Close() })

	fixture := newConfigClientWith(t, configOptions{store: store, seed: map[string]string{
		storage.DocumentPersona: "",
	}})
	hash := fixture.get(t).GetHash()

	// Twelve edits from the one version: one is saved, the rest are told so,
	// and none of them is told the database is busy.
	const writers = 12
	var (
		wg      sync.WaitGroup
		mu      sync.Mutex
		saved   int
		refused int
		other   []error
	)
	for i := range writers {
		edits := []*controlv1.ConfigEdit{edit(t, "agent.max_iterations", float64(20+i))}

		wg.Add(1)
		go func() {
			defer wg.Done()

			resp, err := fixture.client.WriteConfig(ctx, connect.NewRequest(&controlv1.WriteConfigRequest{Hash: hash, Edits: edits}))

			mu.Lock()
			defer mu.Unlock()
			switch {
			case err == nil && resp.Msg.GetWritten():
				saved++
			case connect.CodeOf(err) == connect.CodeFailedPrecondition:
				refused++
			default:
				other = append(other, err)
			}
		}()
	}
	wg.Wait()
	if saved != 1 || refused != writers-1 || len(other) != 0 {
		t.Errorf("%d saved, %d refused, unexpected: %v", saved, refused, other)
	}

	// A document nobody has written reads as empty, takes a first edit, and is
	// listed once it has one.
	persona := controlv1.InstructionFile_INSTRUCTION_FILE_PERSONA
	if got := fixture.getInstruction(t, persona); got.GetText() != "" || got.GetHash() != "" || got.GetVersion() != 0 {
		t.Fatalf("a document never written reads as %v", got)
	}
	put, err := fixture.putInstruction(persona, "Brief.\n", "")
	if err != nil || put.GetVersion() != 1 {
		t.Fatalf("first edit: %v %v", put, err)
	}
	versions, err := fixture.client.ListDocumentVersions(ctx, connect.NewRequest(&controlv1.ListDocumentVersionsRequest{
		Document: controlv1.SettingsDocument_SETTINGS_DOCUMENT_PERSONA,
	}))
	if err != nil || len(versions.Msg.GetVersions()) != 1 || versions.Msg.GetVersions()[0].GetSource() != "web" {
		t.Errorf("versions: %v %v", versions, err)
	}
}

// ── whether there is an agent ──────────────────────────────────────────

func TestAgentStateSaysARunningDaemonHasNothingToRepair(t *testing.T) {
	started := time.Date(2026, 10, 13, 9, 30, 0, 0, time.UTC)
	fixture := newConfigClientWith(t, configOptions{state: control.AgentState{StartedAt: started}})

	resp, err := fixture.client.GetAgentState(context.Background(), connect.NewRequest(&controlv1.GetAgentStateRequest{}))
	if err != nil {
		t.Fatalf("state: %v", err)
	}
	if resp.Msg.GetMode() != "running" || len(resp.Msg.GetProblems()) != 0 {
		t.Errorf("state: %v", resp.Msg)
	}
	if !resp.Msg.GetStartedAt().AsTime().Equal(started) {
		t.Errorf("started at %v", resp.Msg.GetStartedAt().AsTime())
	}
}

func TestAgentStateSaysWhyThereIsNoAgent(t *testing.T) {
	fixture := newConfigClientWith(t, configOptions{state: control.AgentState{
		Repair: true,
		Problems: []control.AgentProblem{
			{
				Problem: config.Problem{Key: "agent.max_iterations", Value: "0", Why: "must be greater than zero", Fix: "Use 1 or more."},
				Source:  "settings",
			},
			{
				Problem: config.Problem{Why: "the model provider could not be built: no key", Fix: "Set GEMINI_API_KEY."},
				Source:  "startup",
			},
		},
	}})

	resp, err := fixture.client.GetAgentState(context.Background(), connect.NewRequest(&controlv1.GetAgentStateRequest{}))
	if err != nil {
		t.Fatalf("state: %v", err)
	}

	if resp.Msg.GetMode() != "repair" || len(resp.Msg.GetProblems()) != 2 {
		t.Fatalf("state: %v", resp.Msg)
	}
	first, second := resp.Msg.GetProblems()[0], resp.Msg.GetProblems()[1]
	if first.GetKey() != "agent.max_iterations" || first.GetValue() != "0" || first.GetSource() != "settings" ||
		first.GetWhy() == "" || first.GetFix() == "" {
		t.Errorf("first: %v", first)
	}
	if second.GetSource() != "startup" || second.GetKey() != "" || second.GetWhy() == "" {
		t.Errorf("second: %v", second)
	}
	if resp.Msg.GetStartedAt() != nil {
		t.Errorf("a start time nobody gave: %v", resp.Msg.GetStartedAt())
	}
}
