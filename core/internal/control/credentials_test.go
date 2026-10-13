package control_test

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"

	"connectrpc.com/connect"
	"google.golang.org/protobuf/encoding/protojson"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/reflect/protoreflect"
	"google.golang.org/protobuf/types/known/timestamppb"

	controlv1 "github.com/KoukeNeko/JingClaw/core/gen/go/jingclaw/control/v1"
	"github.com/KoukeNeko/JingClaw/core/gen/go/jingclaw/control/v1/controlv1connect"
	"github.com/KoukeNeko/JingClaw/core/internal/config"
	"github.com/KoukeNeko/JingClaw/core/internal/control"
	"github.com/KoukeNeko/JingClaw/core/internal/fsperm"
	"github.com/KoukeNeko/JingClaw/core/internal/fsperm/permtest"
	"github.com/KoukeNeko/JingClaw/core/internal/home"
	"github.com/KoukeNeko/JingClaw/core/internal/storage"
	"github.com/KoukeNeko/JingClaw/core/internal/storage/memory"
)

// A value that is nothing like a word, so that finding any part of it in a
// reply is a leak and not a coincidence.
const credentialCanary = "q9Zr-7TxK2mVb8Lw3Nd5YpHc4Gs1"

// slotKeys are the settings that name each slot's file, in the order the
// slots are listed.
var slotKeys = []string{
	"provider.gemini.api_key_file",
	"provider.anthropic.api_key_file",
	"provider.ollama.api_key_file",
	"provider.openai_compat.api_key_file",
	"memory.embedding.api_key_file",
	"web.search.key_file",
	"gateway.discord.token_file",
	"gateway.telegram.token_file",
}

const (
	gemini    = controlv1.CredentialSlot_CREDENTIAL_SLOT_PROVIDER_GEMINI
	anthropic = controlv1.CredentialSlot_CREDENTIAL_SLOT_PROVIDER_ANTHROPIC
	ollama    = controlv1.CredentialSlot_CREDENTIAL_SLOT_PROVIDER_OLLAMA
	openaiC   = controlv1.CredentialSlot_CREDENTIAL_SLOT_PROVIDER_OPENAI_COMPAT
	embedding = controlv1.CredentialSlot_CREDENTIAL_SLOT_MEMORY_EMBEDDING
	search    = controlv1.CredentialSlot_CREDENTIAL_SLOT_WEB_SEARCH
	discord   = controlv1.CredentialSlot_CREDENTIAL_SLOT_GATEWAY_DISCORD
	telegram  = controlv1.CredentialSlot_CREDENTIAL_SLOT_GATEWAY_TELEGRAM
)

// isolateEnvironment gives a test a deployment of its own and an environment
// that says nothing, so that what a developer has exported cannot decide the
// outcome. It returns the deployment directory, which exists.
func isolateEnvironment(t *testing.T) string {
	t.Helper()

	for _, pair := range os.Environ() {
		name, _, _ := strings.Cut(pair, "=")
		switch name {
		case "GEMINI_API_KEY", "GOOGLE_API_KEY", "ANTHROPIC_API_KEY", "BRAVE_SEARCH_TOKEN",
			"DISCORD_BOT_TOKEN", "TELEGRAM_BOT_TOKEN":
		default:
			if !strings.HasPrefix(name, "JINGCLAW_") {
				continue
			}
		}
		// Unset, not blank: a blank JINGCLAW_ variable is a setting.
		t.Setenv(name, "")
		_ = os.Unsetenv(name)
	}

	root := filepath.Join(t.TempDir(), home.DirName)
	if err := os.Mkdir(root, 0o700); err != nil {
		t.Fatal(err)
	}
	t.Setenv(home.EnvVar, root)
	return root
}

type credentialFixture struct {
	client controlv1connect.CredentialServiceClient
	store  storage.DocumentStore
	root   string
	logs   *bytes.Buffer

	// begun is when the process the server stands for began.
	begun time.Time
}

// newCredentialFixture serves the credentials of a deployment whose settings
// are text.
func newCredentialFixture(t *testing.T, text string) credentialFixture {
	t.Helper()
	return newCredentialFixtureBegun(t, text, time.Now().Add(-time.Minute))
}

func newCredentialFixtureBegun(t *testing.T, text string, begun time.Time) credentialFixture {
	t.Helper()
	return newCredentialFixtureWith(t, text, begun, config.Candidate)
}

// newCredentialFixtureWith is a fixture for a process that turns the saved text
// into its settings in the way candidate does, as the daemon does with the flags
// it was started with.
func newCredentialFixtureWith(
	t *testing.T, text string, begun time.Time, candidate func([]byte) (config.Config, error),
) credentialFixture {
	t.Helper()

	root := isolateEnvironment(t)
	store := memory.New()
	if text != "" {
		seeds := []storage.DocumentSeed{{Name: storage.DocumentConfig, Text: text, Source: "default"}}
		if _, err := store.SeedDocuments(context.Background(), seeds, time.Now()); err != nil {
			t.Fatal(err)
		}
	}

	logs := &bytes.Buffer{}
	handler := control.NewCredentialServer(store, candidate, begun, slog.New(slog.NewTextHandler(logs, nil)))

	mux := http.NewServeMux()
	mux.Handle(controlv1connect.NewCredentialServiceHandler(handler))

	server := httptest.NewServer(mux)
	t.Cleanup(server.Close)

	return credentialFixture{
		client: controlv1connect.NewCredentialServiceClient(server.Client(), server.URL),
		store:  store, root: root, logs: logs, begun: begun,
	}
}

func (f credentialFixture) list(t *testing.T) []*controlv1.Credential {
	t.Helper()

	resp, err := f.client.ListCredentials(context.Background(), connect.NewRequest(&controlv1.ListCredentialsRequest{}))
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	return resp.Msg.Credentials
}

func (f credentialFixture) slot(t *testing.T, which controlv1.CredentialSlot) *controlv1.Credential {
	t.Helper()

	for _, one := range f.list(t) {
		if one.Slot == which {
			return one
		}
	}
	t.Fatalf("%v is not listed", which)
	return nil
}

func (f credentialFixture) put(which controlv1.CredentialSlot, value string) (*controlv1.Credential, error) {
	resp, err := f.client.PutCredential(context.Background(), connect.NewRequest(&controlv1.PutCredentialRequest{
		Slot: which, Value: value,
	}))
	if err != nil {
		return nil, err
	}
	return resp.Msg.Credential, nil
}

func (f credentialFixture) path(name string) string { return filepath.Join(f.root, name) }

// keyFile puts a credential file in the deployment the way an operator would.
func (f credentialFixture) keyFile(t *testing.T, name, content string) string {
	t.Helper()

	path := f.path(name)
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := fsperm.Restrict(path); err != nil {
		t.Fatal(err)
	}
	return path
}

// rest is everything in the deployment directory, sorted.
func (f credentialFixture) rest(t *testing.T) string {
	t.Helper()

	entries, err := os.ReadDir(f.root)
	if err != nil {
		t.Fatal(err)
	}
	var names []string
	for _, entry := range entries {
		names = append(names, entry.Name())
	}
	sort.Strings(names)
	return strings.Join(names, ",")
}

// saveSettings replaces the saved config the way a dashboard save would.
func (f credentialFixture) saveSettings(t *testing.T, text string) {
	t.Helper()

	doc, err := f.store.Document(context.Background(), storage.DocumentConfig)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.store.PutDocument(context.Background(), storage.DocumentConfig, text, "web", doc.Hash, time.Now()); err != nil {
		t.Fatal(err)
	}
}

func code(err error) connect.Code {
	var failed *connect.Error
	if errors.As(err, &failed) {
		return failed.Code()
	}
	return connect.Code(0)
}

// ── what is listed ─────────────────────────────────────────────────────

func TestEverySlotIsListedOnceWithTheSettingThatNamesItsFile(t *testing.T) {
	f := newCredentialFixture(t, "")

	listed := f.list(t)
	if len(listed) != len(slotKeys) {
		t.Fatalf("%d slots are listed, want %d", len(listed), len(slotKeys))
	}

	seen := map[controlv1.CredentialSlot]bool{}
	for i, one := range listed {
		if one.FileSetting != slotKeys[i] {
			t.Errorf("slot %d (%v) is named by %q, want %q", i, one.Slot, one.FileSetting, slotKeys[i])
		}
		if want := controlv1.CredentialSlot(i + 1); one.Slot != want {
			t.Errorf("slot %d is %v, want %v: they are listed in the order of the enum", i, one.Slot, want)
		}
		seen[one.Slot] = true
	}
	for number, name := range controlv1.CredentialSlot_name {
		if number != 0 && !seen[controlv1.CredentialSlot(number)] {
			t.Errorf("%s is never listed", name)
		}
	}
}

// What the table says, derived from the code that reads each credential and
// kept here so that a change to one is a change to the other.
func TestWhatEachSlotIsAndWhoReadsItFollowsTheSettings(t *testing.T) {
	const (
		required = controlv1.CredentialNeed_CREDENTIAL_NEED_REQUIRED
		optional = controlv1.CredentialNeed_CREDENTIAL_NEED_OPTIONAL
		unused   = controlv1.CredentialNeed_CREDENTIAL_NEED_UNUSED
	)

	cases := []struct {
		name string
		text string
		need map[controlv1.CredentialSlot]controlv1.CredentialNeed
	}{
		{
			"the offline provider needs nothing, and the default platform needs its token", "",
			map[controlv1.CredentialSlot]controlv1.CredentialNeed{
				gemini: unused, anthropic: unused, ollama: unused, openaiC: unused,
				embedding: unused, search: unused, discord: required, telegram: unused,
			},
		},
		{
			"gemini needs a key", "[provider]\nbackend = \"gemini\"\n",
			map[controlv1.CredentialSlot]controlv1.CredentialNeed{gemini: required, anthropic: unused, ollama: unused, openaiC: unused},
		},
		{
			"anthropic needs a key", "[provider]\nbackend = \"anthropic\"\n",
			map[controlv1.CredentialSlot]controlv1.CredentialNeed{gemini: unused, anthropic: required},
		},
		{
			"a model server on this machine may have one", "[provider]\nbackend = \"ollama\"\n",
			map[controlv1.CredentialSlot]controlv1.CredentialNeed{ollama: optional, gemini: unused},
		},
		{
			"so may any endpoint that speaks the OpenAI protocol", "[provider]\nbackend = \"openai_compat\"\n",
			map[controlv1.CredentialSlot]controlv1.CredentialNeed{openaiC: optional, ollama: unused},
		},
		{
			"an embedding endpoint is used once a model is named", "[memory.embedding]\nmodel = \"m\"\n",
			map[controlv1.CredentialSlot]controlv1.CredentialNeed{embedding: optional},
		},
		{
			"and not when memory is off", "[memory]\nenabled = false\n[memory.embedding]\nmodel = \"m\"\n",
			map[controlv1.CredentialSlot]controlv1.CredentialNeed{embedding: unused},
		},
		{
			"search needs its key when it is on and brave", "[web]\nenabled = true\n[web.search]\nbackend = \"brave\"\n",
			map[controlv1.CredentialSlot]controlv1.CredentialNeed{search: required},
		},
		{
			"not when reading is off", "[web.search]\nbackend = \"brave\"\n",
			map[controlv1.CredentialSlot]controlv1.CredentialNeed{search: unused},
		},
		{
			"nor when the backend is none", "[web]\nenabled = true\n",
			map[controlv1.CredentialSlot]controlv1.CredentialNeed{search: unused},
		},
		{
			"telegram needs its token when it is the platform", "[gateway]\nplatform = \"telegram\"\n",
			map[controlv1.CredentialSlot]controlv1.CredentialNeed{telegram: required, discord: unused},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			f := newCredentialFixture(t, tc.text)
			for slot, want := range tc.need {
				if got := f.slot(t, slot).Need; got != want {
					t.Errorf("%v is %v, want %v", slot, got, want)
				}
			}
		})
	}

	f := newCredentialFixture(t, "")
	for slot, want := range map[controlv1.CredentialSlot]controlv1.CredentialProcess{
		gemini: controlv1.CredentialProcess_CREDENTIAL_PROCESS_DAEMON, anthropic: controlv1.CredentialProcess_CREDENTIAL_PROCESS_DAEMON,
		ollama: controlv1.CredentialProcess_CREDENTIAL_PROCESS_DAEMON, openaiC: controlv1.CredentialProcess_CREDENTIAL_PROCESS_DAEMON,
		embedding: controlv1.CredentialProcess_CREDENTIAL_PROCESS_DAEMON, search: controlv1.CredentialProcess_CREDENTIAL_PROCESS_DAEMON,
		discord: controlv1.CredentialProcess_CREDENTIAL_PROCESS_GATEWAY, telegram: controlv1.CredentialProcess_CREDENTIAL_PROCESS_GATEWAY,
	} {
		if got := f.slot(t, slot).Process; got != want {
			t.Errorf("%v is read by %v, want %v", slot, got, want)
		}
	}
}

func TestTheFileAndTheVariablesAreTheOnesTheSettingsName(t *testing.T) {
	f := newCredentialFixture(t, "[provider.anthropic]\napi_key_file = \"mine.key\"\napi_key_env = [\"MY_A\", \"MY_B\"]\n")

	got := f.slot(t, gemini)
	if got.FileName != "gemini.key" || got.EnvSetting != "provider.gemini.api_key_env" ||
		strings.Join(got.EnvNames, ",") != "GEMINI_API_KEY,GOOGLE_API_KEY" {
		t.Errorf("the defaults: %+v", got)
	}

	got = f.slot(t, anthropic)
	if got.FileName != "mine.key" || got.FileSetting != "provider.anthropic.api_key_file" ||
		strings.Join(got.EnvNames, ",") != "MY_A,MY_B" {
		t.Errorf("what the settings say: %+v", got)
	}

	// A name nobody gave is no file, and the slot then has only the environment.
	got = f.slot(t, ollama)
	if got.FileName != "" || got.FileState != controlv1.CredentialFileState_CREDENTIAL_FILE_STATE_NO_NAME ||
		got.Source != controlv1.CredentialSource_CREDENTIAL_SOURCE_NONE {
		t.Errorf("no name: %+v", got)
	}
}

func TestAFileThatIsThereIsReportedWithItsTimeAndNothingElse(t *testing.T) {
	f := newCredentialFixture(t, "[provider]\nbackend = \"gemini\"\n")
	path := f.keyFile(t, "gemini.key", credentialCanary+"\n")
	modified := time.Now().Add(-time.Hour).Truncate(time.Second)
	if err := os.Chtimes(path, modified, modified); err != nil {
		t.Fatal(err)
	}

	got := f.slot(t, gemini)
	if got.Source != controlv1.CredentialSource_CREDENTIAL_SOURCE_FILE ||
		got.FileState != controlv1.CredentialFileState_CREDENTIAL_FILE_STATE_PRESENT ||
		got.Need != controlv1.CredentialNeed_CREDENTIAL_NEED_REQUIRED {
		t.Errorf("the slot: %+v", got)
	}
	if !got.ModifiedAt.AsTime().Equal(modified) {
		t.Errorf("modified at %v, want %v", got.ModifiedAt.AsTime(), modified)
	}
	if got.RestartNeeded {
		t.Error("a file written before the process began is said to need a restart")
	}

	f.keyFile(t, "discord.token", " \n")
	if got := f.slot(t, discord); got.FileState != controlv1.CredentialFileState_CREDENTIAL_FILE_STATE_EMPTY ||
		got.Source != controlv1.CredentialSource_CREDENTIAL_SOURCE_NONE {
		t.Errorf("a file with nothing in it: %+v", got)
	}
	if got := f.slot(t, telegram); got.FileState != controlv1.CredentialFileState_CREDENTIAL_FILE_STATE_ABSENT ||
		got.ModifiedAt != nil {
		t.Errorf("no file: %+v", got)
	}
}

func TestAVariableThatIsSetIsReportedByNameAndWinsOverTheFile(t *testing.T) {
	f := newCredentialFixture(t, "[provider]\nbackend = \"gemini\"\n")
	t.Setenv("GOOGLE_API_KEY", credentialCanary)
	t.Setenv("GEMINI_API_KEY", "  ")

	got := f.slot(t, gemini)
	if got.Source != controlv1.CredentialSource_CREDENTIAL_SOURCE_ENVIRONMENT || got.EnvInUse != "GOOGLE_API_KEY" {
		t.Errorf("the variable in use: %+v", got)
	}
	if got.FileState != controlv1.CredentialFileState_CREDENTIAL_FILE_STATE_ABSENT {
		t.Errorf("the file beside it is %v", got.FileState)
	}

	// The file is still there to be looked after, and it is not what decides.
	path := f.keyFile(t, "gemini.key", "from the file")
	if err := permtest.Expose(path); err != nil {
		t.Fatal(err)
	}
	got = f.slot(t, gemini)
	if got.Source != controlv1.CredentialSource_CREDENTIAL_SOURCE_ENVIRONMENT ||
		got.FileState != controlv1.CredentialFileState_CREDENTIAL_FILE_STATE_WRONG_MODE {
		t.Errorf("a variable with a file beside it that anybody could read: %+v", got)
	}
	if got.RestartNeeded {
		t.Error("a file the environment overrides is said to need a restart")
	}
	if _, err := f.put(gemini, "replaced"); err != nil {
		t.Fatal(err)
	}
	if got := f.slot(t, gemini); got.RestartNeeded || got.Source != controlv1.CredentialSource_CREDENTIAL_SOURCE_ENVIRONMENT {
		t.Errorf("saving a file the environment overrides: %+v", got)
	}
}

// A file anybody could read is not used, and a process that comes to it does
// not start. That is the commonest reason for a daemon with no agent.
func TestAFileAnybodyCouldReadIsBlockedUntilItIsRestricted(t *testing.T) {
	f := newCredentialFixture(t, "[provider]\nbackend = \"gemini\"\n")
	path := f.keyFile(t, "gemini.key", credentialCanary)
	if err := permtest.Expose(path); err != nil {
		t.Fatal(err)
	}

	got := f.slot(t, gemini)
	if got.Source != controlv1.CredentialSource_CREDENTIAL_SOURCE_BLOCKED ||
		got.FileState != controlv1.CredentialFileState_CREDENTIAL_FILE_STATE_WRONG_MODE ||
		!strings.Contains(got.FileDetail, "mode") {
		t.Errorf("a world-readable file: %+v", got)
	}
	if strings.Contains(got.FileDetail, path) {
		t.Errorf("the detail names the path: %q", got.FileDetail)
	}

	resp, err := f.client.RestrictCredentialFile(context.Background(), connect.NewRequest(&controlv1.RestrictCredentialFileRequest{Slot: gemini}))
	if err != nil {
		t.Fatalf("restrict: %v", err)
	}
	fixed := resp.Msg.Credential
	if fixed.Source != controlv1.CredentialSource_CREDENTIAL_SOURCE_FILE ||
		fixed.FileState != controlv1.CredentialFileState_CREDENTIAL_FILE_STATE_PRESENT {
		t.Errorf("after restricting: %+v", fixed)
	}
	// A chmod leaves the modification time alone; this process did it, and the
	// process that is running did not see it.
	if !fixed.RestartNeeded {
		t.Error("a repair that only a restart will use is not said to need one")
	}
	if ok, _, _ := fsperm.EnsureOwnerOnly(path); !ok {
		t.Error("the file can still be read by others")
	}
	if content, _ := os.ReadFile(path); string(content) != credentialCanary {
		t.Errorf("restricting changed the content to %q", content)
	}
}

func TestSomethingThatIsNotAFileIsBlockedAndSaid(t *testing.T) {
	f := newCredentialFixture(t, "[provider]\nbackend = \"gemini\"\n")
	if err := os.Mkdir(f.path("gemini.key"), 0o700); err != nil {
		t.Fatal(err)
	}

	got := f.slot(t, gemini)
	if got.Source != controlv1.CredentialSource_CREDENTIAL_SOURCE_BLOCKED ||
		got.FileState != controlv1.CredentialFileState_CREDENTIAL_FILE_STATE_NOT_REGULAR {
		t.Errorf("a directory at the file's name: %+v", got)
	}
}

func TestRestartIsNeededWhenAFileIsWrittenAfterTheProcessBeganAndOnlyThen(t *testing.T) {
	begun := time.Now().Add(-time.Hour)
	f := newCredentialFixtureBegun(t, "[provider]\nbackend = \"gemini\"\n", begun)
	path := f.keyFile(t, "gemini.key", "k")

	// Whole seconds: a file system that keeps less than a nanosecond would
	// round anything finer, and what is asked is only which side of the start.
	for name, want := range map[string]bool{"before": false, "after": true} {
		at := begun.Add(-time.Minute)
		if want {
			at = begun.Add(time.Minute)
		}
		if err := os.Chtimes(path, at, at); err != nil {
			t.Fatal(err)
		}
		if got := f.slot(t, gemini).RestartNeeded; got != want {
			t.Errorf("a file written %s the process began: restart needed is %v", name, got)
		}
	}

	// And not for what nothing reads.
	f.saveSettings(t, "")
	if f.slot(t, gemini).RestartNeeded {
		t.Error("a credential nothing reads is said to need a restart")
	}
}

// ── saving ─────────────────────────────────────────────────────────────

func TestPutWritesTheFileOwnerOnlyWithExactlyTheValueAndNothingBeside(t *testing.T) {
	f := newCredentialFixture(t, "[provider]\nbackend = \"gemini\"\n")

	got, err := f.put(gemini, credentialCanary+"\n")
	if err != nil {
		t.Fatalf("put: %v", err)
	}

	content, err := os.ReadFile(f.path("gemini.key"))
	if err != nil || string(content) != credentialCanary {
		t.Errorf("the file holds %q (%v)", content, err)
	}
	if ok, _, _ := fsperm.EnsureOwnerOnly(f.path("gemini.key")); !ok {
		t.Error("the file can be read by others")
	}
	if rest := f.rest(t); rest != "gemini.key" {
		t.Errorf("the deployment directory holds %q: no backup, no temporary file", rest)
	}
	if got.Source != controlv1.CredentialSource_CREDENTIAL_SOURCE_FILE || got.ModifiedAt == nil || !got.RestartNeeded ||
		got.FileState != controlv1.CredentialFileState_CREDENTIAL_FILE_STATE_PRESENT {
		t.Errorf("the reply: %+v", got)
	}
	if info, _ := os.Stat(f.path("gemini.key")); !got.ModifiedAt.AsTime().Equal(info.ModTime()) {
		t.Errorf("the reply says %v, the file says %v", got.ModifiedAt.AsTime(), info.ModTime())
	}
}

func TestPutRepairsAFileAnybodyCouldRead(t *testing.T) {
	f := newCredentialFixture(t, "[provider]\nbackend = \"gemini\"\n")
	path := f.keyFile(t, "gemini.key", "old")
	if err := permtest.Expose(path); err != nil {
		t.Fatal(err)
	}

	got, err := f.put(gemini, "new")
	if err != nil {
		t.Fatalf("put: %v", err)
	}
	if got.Source != controlv1.CredentialSource_CREDENTIAL_SOURCE_FILE {
		t.Errorf("after saving: %+v", got)
	}
	if ok, _, _ := fsperm.EnsureOwnerOnly(path); !ok {
		t.Error("saving again did not repair the mode")
	}
}

func TestPutSavesSeveralSlotsToTheirOwnFiles(t *testing.T) {
	f := newCredentialFixture(t, "")

	for slot, name := range map[controlv1.CredentialSlot]string{
		gemini: "gemini.key", search: "brave-search.token", discord: "discord.token", telegram: "telegram.token",
	} {
		if _, err := f.put(slot, "value for "+name); err != nil {
			t.Fatalf("%v: %v", slot, err)
		}
	}
	if rest := f.rest(t); rest != "brave-search.token,discord.token,gemini.key,telegram.token" {
		t.Errorf("the deployment directory holds %q", rest)
	}
}

func TestPutRefusesAValueThatCannotBeACredentialWithoutRepeatingIt(t *testing.T) {
	f := newCredentialFixture(t, "")
	f.keyFile(t, "gemini.key", "kept")

	for name, value := range map[string]string{
		"empty": "", "spaces": "   ", "two lines": credentialCanary + "\n" + credentialCanary,
		// Text that is not UTF-8 cannot be sent at all: the string field of a
		// request is refused by the codec before the service sees it, and
		// secret.CheckValue is tested with it directly.
		"a NUL":    credentialCanary + "\x00",
		"too long": credentialCanary + strings.Repeat("x", 8192),
	} {
		_, err := f.put(gemini, value)
		if code(err) != connect.CodeInvalidArgument {
			t.Errorf("%s: got %v, want invalid argument", name, err)
			continue
		}
		if strings.Contains(err.Error(), credentialCanary) {
			t.Errorf("%s: the refusal repeats the value: %v", name, err)
		}
	}

	if _, err := f.client.PutCredential(context.Background(), connect.NewRequest(&controlv1.PutCredentialRequest{Value: "x"})); code(err) != connect.CodeInvalidArgument {
		t.Errorf("no slot: %v", err)
	}
	if content, _ := os.ReadFile(f.path("gemini.key")); string(content) != "kept" {
		t.Errorf("a refused save changed the file to %q", content)
	}
	if rest := f.rest(t); rest != "gemini.key" {
		t.Errorf("the deployment directory holds %q", rest)
	}
}

// The commonest way to name no file is a slot whose default is none, and what
// is wanted then is the setting, not a guess at a file.
func TestPutWithoutAFileNameSaysWhichSettingToGiveAndSuggestsOne(t *testing.T) {
	f := newCredentialFixture(t, "")

	_, err := f.put(anthropic, "k")
	if code(err) != connect.CodeFailedPrecondition {
		t.Fatalf("got %v, want a failed precondition", err)
	}
	if !strings.Contains(err.Error(), "provider.anthropic.api_key_file") || !strings.Contains(err.Error(), "anthropic.key") ||
		!strings.Contains(err.Error(), "first") {
		t.Errorf("the refusal does not say to set the setting first, or what to: %v", err)
	}
	if rest := f.rest(t); rest != "" {
		t.Errorf("the deployment directory holds %q", rest)
	}

	// The settings are read afresh, so giving the name is all it takes.
	f.saveSettings(t, "[provider.anthropic]\napi_key_file = \"anthropic.key\"\n")
	if _, err := f.put(anthropic, "k"); err != nil {
		t.Errorf("after the setting was given: %v", err)
	}
	if rest := f.rest(t); rest != "anthropic.key" {
		t.Errorf("the deployment directory holds %q", rest)
	}

	// Taking it away again takes the file's name away with it.
	f.saveSettings(t, "[provider.gemini]\napi_key_file = \"\"\n")
	if _, err := f.put(gemini, "k"); code(err) != connect.CodeFailedPrecondition {
		t.Errorf("a setting emptied on purpose is not the default again: %v", err)
	}
}

// A name that climbs out of the deployment made the daemon read a mode-600 file
// outside it and send what was in it to a provider as a key. The same name must
// not be written to either.
func TestAFileNameThatIsNotAllowedIsNeitherWrittenNorRemovedNorRestricted(t *testing.T) {
	for _, name := range []string{"../outside.key", "/tmp/outside.key", "sub/dir.key", "config.toml", "AGENTS.md", "x.bak.1", "-lead"} {
		t.Run(name, func(t *testing.T) {
			f := newCredentialFixture(t, fmt.Sprintf("[provider.gemini]\napi_key_file = %q\n", name))
			outside := filepath.Join(filepath.Dir(f.root), "outside.key")
			if err := os.WriteFile(outside, []byte("not yours"), 0o600); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(f.path("config.toml"), []byte("kept\n"), 0o600); err != nil {
				t.Fatal(err)
			}
			before := f.rest(t)

			_, putErr := f.put(gemini, credentialCanary)
			_, deleteErr := f.client.DeleteCredential(context.Background(), connect.NewRequest(&controlv1.DeleteCredentialRequest{Slot: gemini}))
			_, restrictErr := f.client.RestrictCredentialFile(context.Background(), connect.NewRequest(&controlv1.RestrictCredentialFileRequest{Slot: gemini}))
			for call, err := range map[string]error{"put": putErr, "delete": deleteErr, "restrict": restrictErr} {
				if code(err) != connect.CodeFailedPrecondition || !strings.Contains(err.Error(), "provider.gemini.api_key_file") ||
					!strings.Contains(err.Error(), "letters, digits") {
					t.Errorf("%s: %v, want a failed precondition that says what a name may be", call, err)
				}
			}

			if got, _ := os.ReadFile(outside); string(got) != "not yours" {
				t.Errorf("the file outside holds %q", got)
			}
			if got, _ := os.ReadFile(f.path("config.toml")); string(got) != "kept\n" {
				t.Errorf("config.toml holds %q", got)
			}
			if f.rest(t) != before {
				t.Errorf("the deployment directory was %q and is %q", before, f.rest(t))
			}

			got := f.slot(t, gemini)
			if got.FileState != controlv1.CredentialFileState_CREDENTIAL_FILE_STATE_INVALID_NAME ||
				got.Source != controlv1.CredentialSource_CREDENTIAL_SOURCE_BLOCKED || got.FileName != "" ||
				!strings.Contains(got.FileDetail, "letters, digits") {
				t.Errorf("listed as %+v", got)
			}
		})
	}
}

func TestTwoSlotsThatShareAFileAreNeitherOfThemWritten(t *testing.T) {
	f := newCredentialFixture(t, "[provider.gemini]\napi_key_file = \"shared.key\"\n[provider.anthropic]\napi_key_file = \"shared.key\"\n")

	for _, slot := range []controlv1.CredentialSlot{gemini, anthropic} {
		if _, err := f.put(slot, "k"); code(err) != connect.CodeFailedPrecondition || !strings.Contains(err.Error(), "same file") {
			t.Errorf("%v: %v", slot, err)
		}
	}
	if rest := f.rest(t); rest != "" {
		t.Errorf("the deployment directory holds %q", rest)
	}
}

// The daemon is run with flags that override the saved text, and a start reads
// the credential of the backend that will run. A repair daemon told to use
// gemini over a saved offline backend wants a gemini key, and the dashboard has
// to say so and say that a restart is what puts it to use.
func TestWhatTheProcessWasGivenOnItsCommandLineDecidesWhichSlotIsNeeded(t *testing.T) {
	withFlags := func(text []byte) (config.Config, error) {
		cfg, err := config.Candidate(text)
		cfg.Provider.Backend = "gemini"
		return cfg, err
	}
	f := newCredentialFixtureWith(t, "[provider]\nbackend = \"fake\"\n", time.Now().Add(-time.Minute), withFlags)

	if got := f.slot(t, gemini); got.Need != controlv1.CredentialNeed_CREDENTIAL_NEED_REQUIRED {
		t.Errorf("the key of the backend the process was told to run is %v, not required", got.Need)
	}

	saved, err := f.put(gemini, credentialCanary)
	if err != nil {
		t.Fatalf("put: %v", err)
	}
	if !saved.RestartNeeded {
		t.Error("a key that the running process does not hold, for the backend it was told to run, is not said to need a restart")
	}
}

// A setting that lists variable names is not checked before it is listed, and
// what is in it may be a key somebody pasted there. It is not listed back, and
// a variable by that name is not what the next start is said to read.
func TestAnEntryThatIsNotAVariableNameIsNeitherListedBackNorLookedUp(t *testing.T) {
	const pasted = "AIzaSyD-9tSrke72PouQMnMX-a7eZSW0jkFMBWY"

	f := newCredentialFixture(t, "[provider]\nbackend = \"gemini\"\n[provider.gemini]\napi_key_env = [\"MY_GEMINI_KEY\", \""+pasted+"\"]\n")
	t.Setenv(pasted, "set under that name")

	got := f.slot(t, gemini)
	if strings.Join(got.EnvNames, ",") != "MY_GEMINI_KEY" {
		t.Errorf("the variables listed are %v, want only the one that is a name", got.EnvNames)
	}
	if got.Source != controlv1.CredentialSource_CREDENTIAL_SOURCE_NONE || got.EnvInUse != "" {
		t.Errorf("a variable that cannot be a listed name is read: %+v", got)
	}
	if said := protojson.Format(got); strings.Contains(said, pasted) {
		t.Errorf("the listing repeats what was pasted: %s", said)
	}
}

// Settings files are renamed to <name>.imported rather than deleted, and are the
// only copy of what they held. A slot pointed at one is refused, so that saving
// to it cannot overwrite it and deleting from it cannot remove it.
func TestAnImportedSettingsFileIsNeitherOverwrittenNorDeletedNorRestrictedThroughASlot(t *testing.T) {
	f := newCredentialFixture(t, "[provider.gemini]\napi_key_file = \"PERSONA.md.imported\"\n")
	f.keyFile(t, "PERSONA.md.imported", "You are terse.\n")

	_, putErr := f.put(gemini, credentialCanary)
	_, deleteErr := f.client.DeleteCredential(context.Background(), connect.NewRequest(&controlv1.DeleteCredentialRequest{Slot: gemini}))
	_, restrictErr := f.client.RestrictCredentialFile(context.Background(), connect.NewRequest(&controlv1.RestrictCredentialFileRequest{Slot: gemini}))
	for call, err := range map[string]error{"put": putErr, "delete": deleteErr, "restrict": restrictErr} {
		if code(err) != connect.CodeFailedPrecondition || !strings.Contains(err.Error(), "keeps for something else") {
			t.Errorf("%s: %v, want a failed precondition that says the name is kept", call, err)
		}
	}
	if got, err := os.ReadFile(f.path("PERSONA.md.imported")); err != nil || string(got) != "You are terse.\n" {
		t.Errorf("the imported file holds %q (%v)", got, err)
	}
}

func TestPutRefusesALinkAtTheFileAndLeavesBothEndsAlone(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("creating a symbolic link needs a privilege most Windows accounts do not have")
	}

	f := newCredentialFixture(t, "")
	target := filepath.Join(filepath.Dir(f.root), "elsewhere.key")
	if err := os.WriteFile(target, []byte("not yours"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(target, f.path("gemini.key")); err != nil {
		t.Fatal(err)
	}

	_, putErr := f.put(gemini, credentialCanary)
	_, deleteErr := f.client.DeleteCredential(context.Background(), connect.NewRequest(&controlv1.DeleteCredentialRequest{Slot: gemini}))
	_, restrictErr := f.client.RestrictCredentialFile(context.Background(), connect.NewRequest(&controlv1.RestrictCredentialFileRequest{Slot: gemini}))
	for call, err := range map[string]error{"put": putErr, "delete": deleteErr, "restrict": restrictErr} {
		if code(err) != connect.CodeFailedPrecondition || !strings.Contains(err.Error(), "exists and is not a credential file") {
			t.Errorf("%s: %v", call, err)
		}
	}

	if got, _ := os.ReadFile(target); string(got) != "not yours" {
		t.Errorf("the target holds %q", got)
	}
	if info, _ := os.Stat(target); info.Mode().Perm() != 0o644 {
		t.Errorf("the target's mode is now %v", info.Mode().Perm())
	}
	if info, err := os.Lstat(f.path("gemini.key")); err != nil || info.Mode()&os.ModeSymlink == 0 {
		t.Errorf("the link is gone: %v %v", info, err)
	}
	if rest := f.rest(t); rest != "gemini.key" {
		t.Errorf("the deployment directory holds %q", rest)
	}
}

func TestPutRefusesWhatCannotHoldAFileWithoutWritingAnywhere(t *testing.T) {
	t.Run("a deployment that is not anywhere", func(t *testing.T) {
		f := newCredentialFixture(t, "")
		t.Setenv(home.EnvVar, home.None)

		_, err := f.put(gemini, credentialCanary)
		if code(err) != connect.CodeFailedPrecondition || !strings.Contains(err.Error(), "no deployment directory") {
			t.Errorf("got %v", err)
		}
		// Asking is still possible: with nowhere to keep one, no file is there.
		if got := f.slot(t, gemini); got.FileState != controlv1.CredentialFileState_CREDENTIAL_FILE_STATE_ABSENT {
			t.Errorf("listed as %+v", got)
		}
	})

	t.Run("a deployment directory that is gone", func(t *testing.T) {
		f := newCredentialFixture(t, "")
		gone := filepath.Join(t.TempDir(), "gone")
		t.Setenv(home.EnvVar, gone)

		_, err := f.put(gemini, credentialCanary)
		if code(err) != connect.CodeFailedPrecondition || !strings.Contains(err.Error(), "does not exist") {
			t.Errorf("got %v", err)
		}
		if _, statErr := os.Stat(gone); !os.IsNotExist(statErr) {
			t.Errorf("the directory was made: %v", statErr)
		}
	})

	t.Run("settings that cannot be read", func(t *testing.T) {
		f := newCredentialFixture(t, "this is = not [toml\n")

		_, putErr := f.put(gemini, credentialCanary)
		_, listErr := f.client.ListCredentials(context.Background(), connect.NewRequest(&controlv1.ListCredentialsRequest{}))
		for call, err := range map[string]error{"put": putErr, "list": listErr} {
			if code(err) != connect.CodeFailedPrecondition || !strings.Contains(err.Error(), "correct the settings first") {
				t.Errorf("%s: got %v", call, err)
			}
		}
		if rest := f.rest(t); rest != "" {
			t.Errorf("the deployment directory holds %q", rest)
		}
	})
}

// The deployment directory may be reached through a link, and is written where
// it really is.
func TestADeploymentReachedThroughALinkIsWrittenAtItsRealPlace(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("creating a symbolic link needs a privilege most Windows accounts do not have")
	}

	f := newCredentialFixture(t, "")
	through := filepath.Join(filepath.Dir(f.root), "through")
	if err := os.Symlink(f.root, through); err != nil {
		t.Fatal(err)
	}
	t.Setenv(home.EnvVar, through)

	if _, err := f.put(gemini, credentialCanary); err != nil {
		t.Fatal(err)
	}
	if content, _ := os.ReadFile(f.path("gemini.key")); string(content) != credentialCanary {
		t.Errorf("the real directory holds %q", content)
	}
}

// compare-and-set is on the modification time. It never looks at what is in the
// file: restoring the time over different content passes, and a changed time
// over the same content does not.
func TestPutExpectsTheModificationTimeAndNeverTheContent(t *testing.T) {
	f := newCredentialFixture(t, "")
	path := f.keyFile(t, "gemini.key", "first")
	listed := f.slot(t, gemini).ModifiedAt
	then := listed.AsTime()

	stale := time.Date(2001, 2, 3, 4, 5, 6, 0, time.UTC)
	_, err := f.client.PutCredential(context.Background(), connect.NewRequest(&controlv1.PutCredentialRequest{
		Slot: gemini, Value: "stale", ExpectModifiedAt: timestamppb.New(stale),
	}))
	if code(err) != connect.CodeFailedPrecondition || !strings.Contains(err.Error(), "list the credentials again") {
		t.Errorf("a time the file does not have: %v", err)
	}
	if content, _ := os.ReadFile(path); string(content) != "first" {
		t.Errorf("a refused save changed the file to %q", content)
	}

	// Somebody else wrote it, with different content, and put the time back.
	if err := os.WriteFile(path, []byte("different"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Chtimes(path, then, then); err != nil {
		t.Fatal(err)
	}
	resp, err := f.client.PutCredential(context.Background(), connect.NewRequest(&controlv1.PutCredentialRequest{
		Slot: gemini, Value: "second", ExpectModifiedAt: listed,
	}))
	if err != nil {
		t.Fatalf("the time the file has: %v", err)
	}
	if content, _ := os.ReadFile(path); string(content) != "second" {
		t.Errorf("the file holds %q", content)
	}

	// And the reply carries the time the next save can expect.
	if _, err := f.client.PutCredential(context.Background(), connect.NewRequest(&controlv1.PutCredentialRequest{
		Slot: gemini, Value: "third", ExpectModifiedAt: resp.Msg.Credential.ModifiedAt,
	})); err != nil {
		t.Errorf("the time the reply gave: %v", err)
	}

	// A file that has gone, expected as it was.
	if err := os.Remove(path); err != nil {
		t.Fatal(err)
	}
	if _, err := f.client.PutCredential(context.Background(), connect.NewRequest(&controlv1.PutCredentialRequest{
		Slot: gemini, Value: "fourth", ExpectModifiedAt: listed,
	})); code(err) != connect.CodeFailedPrecondition {
		t.Errorf("a file that was removed: %v", err)
	}
	if rest := f.rest(t); rest != "" {
		t.Errorf("the deployment directory holds %q", rest)
	}
}

// A save that began with no file must not replace one that has appeared since:
// the person never saw it, and there is no backup.
func TestPutThatExpectedNoFileLeavesOneThatAppearedAlone(t *testing.T) {
	f := newCredentialFixture(t, "")

	if _, err := f.client.PutCredential(context.Background(), connect.NewRequest(&controlv1.PutCredentialRequest{
		Slot: gemini, Value: "first", ExpectAbsent: true,
	})); err != nil {
		t.Fatalf("no file yet: %v", err)
	}

	_, err := f.client.PutCredential(context.Background(), connect.NewRequest(&controlv1.PutCredentialRequest{
		Slot: gemini, Value: "second", ExpectAbsent: true,
	}))
	if code(err) != connect.CodeFailedPrecondition || !strings.Contains(err.Error(), "list the credentials again") {
		t.Errorf("a file that is there: %v", err)
	}
	if content, _ := os.ReadFile(f.path("gemini.key")); string(content) != "first" {
		t.Errorf("a refused save changed the file to %q", content)
	}
}

// ── deleting and restricting ───────────────────────────────────────────

func TestDeleteRemovesTheFileAndSucceedsWhenThereIsNone(t *testing.T) {
	f := newCredentialFixture(t, "[provider]\nbackend = \"gemini\"\n")
	f.keyFile(t, "gemini.key", credentialCanary)
	f.keyFile(t, "discord.token", "another")

	remove := func() *controlv1.Credential {
		resp, err := f.client.DeleteCredential(context.Background(), connect.NewRequest(&controlv1.DeleteCredentialRequest{Slot: gemini}))
		if err != nil {
			t.Fatalf("delete: %v", err)
		}
		return resp.Msg.Credential
	}

	got := remove()
	if f.rest(t) != "discord.token" {
		t.Errorf("the deployment directory holds %q: only the slot's own file goes", f.rest(t))
	}
	if got.FileState != controlv1.CredentialFileState_CREDENTIAL_FILE_STATE_ABSENT ||
		got.Source != controlv1.CredentialSource_CREDENTIAL_SOURCE_NONE {
		t.Errorf("after deleting: %+v", got)
	}
	// The running process still holds what it read, and a start now would find
	// nothing, which is a change a modification time cannot show.
	if !got.RestartNeeded {
		t.Error("a deleted key that the running process holds is not said to need a restart")
	}

	again := remove()
	if again.FileState != controlv1.CredentialFileState_CREDENTIAL_FILE_STATE_ABSENT {
		t.Errorf("deleting what is not there: %+v", again)
	}
}

func TestDeleteAndRestrictOfAFileThatIsNotThereDifferAsTheyShould(t *testing.T) {
	f := newCredentialFixture(t, "")

	if _, err := f.client.RestrictCredentialFile(context.Background(), connect.NewRequest(&controlv1.RestrictCredentialFileRequest{Slot: gemini})); code(err) != connect.CodeFailedPrecondition {
		t.Errorf("restricting a file that is not there: %v", err)
	}
	resp, err := f.client.DeleteCredential(context.Background(), connect.NewRequest(&controlv1.DeleteCredentialRequest{Slot: gemini}))
	if err != nil {
		t.Fatal(err)
	}
	if resp.Msg.Credential.RestartNeeded {
		t.Error("deleting nothing is said to need a restart")
	}
}

func TestDeleteRefusesADirectoryAtTheFilesName(t *testing.T) {
	f := newCredentialFixture(t, "")
	if err := os.Mkdir(f.path("gemini.key"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(f.path("gemini.key"), "inside"), []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}

	_, err := f.client.DeleteCredential(context.Background(), connect.NewRequest(&controlv1.DeleteCredentialRequest{Slot: gemini}))
	if code(err) != connect.CodeFailedPrecondition || !strings.Contains(err.Error(), "exists and is not a credential file") {
		t.Errorf("got %v", err)
	}
	if _, statErr := os.Stat(filepath.Join(f.path("gemini.key"), "inside")); statErr != nil {
		t.Errorf("what was inside is gone: %v", statErr)
	}
}

// ── what is never said ─────────────────────────────────────────────────

// The replies have no field that could hold a value, a size, a hash or a mask,
// and a field added later has to be added here on purpose.
func TestNothingAReplyCanCarryIsAnythingButAStateOfTheSlot(t *testing.T) {
	allowed := map[string]protoreflect.Kind{
		"slot": protoreflect.EnumKind, "process": protoreflect.EnumKind, "need": protoreflect.EnumKind,
		"source": protoreflect.EnumKind, "file_state": protoreflect.EnumKind,
		"file_setting": protoreflect.StringKind, "file_name": protoreflect.StringKind,
		"env_setting": protoreflect.StringKind, "env_names": protoreflect.StringKind, "env_in_use": protoreflect.StringKind,
		"file_detail":    protoreflect.StringKind,
		"modified_at":    protoreflect.MessageKind,
		"restart_needed": protoreflect.BoolKind,
	}

	fields := (&controlv1.Credential{}).ProtoReflect().Descriptor().Fields()
	for i := range fields.Len() {
		field := fields.Get(i)
		want, ok := allowed[string(field.Name())]
		if !ok || field.Kind() != want {
			t.Errorf("Credential.%s (%v) is not one of the fields that describe the state of a slot", field.Name(), field.Kind())
		}
	}
	if fields.Len() != len(allowed) {
		t.Errorf("Credential has %d fields, this list has %d", fields.Len(), len(allowed))
	}
	if got := fields.ByName("modified_at").Message().FullName(); got != "google.protobuf.Timestamp" {
		t.Errorf("modified_at is a %s", got)
	}

	// And the replies are built of nothing else.
	for _, reply := range []proto.Message{
		&controlv1.ListCredentialsResponse{}, &controlv1.PutCredentialResponse{},
		&controlv1.DeleteCredentialResponse{}, &controlv1.RestrictCredentialFileResponse{},
	} {
		replyFields := reply.ProtoReflect().Descriptor().Fields()
		for i := range replyFields.Len() {
			field := replyFields.Get(i)
			if field.Message() == nil || field.Message().FullName() != "jingclaw.control.v1.Credential" {
				t.Errorf("%s.%s is not a Credential", reply.ProtoReflect().Descriptor().Name(), field.Name())
			}
		}
	}
}

// leaks are the forms a value takes when something tries to be helpful about it.
func leaks(value string) map[string]string {
	sum := sha256.Sum256([]byte(value))
	forms := map[string]string{
		"the value":         value,
		"its first four":    value[:4],
		"its last four":     value[len(value)-4:],
		"standard base64":   base64.StdEncoding.EncodeToString([]byte(value)),
		"URL base64":        base64.URLEncoding.EncodeToString([]byte(value)),
		"raw base64":        base64.RawStdEncoding.EncodeToString([]byte(value)),
		"hex":               hex.EncodeToString([]byte(value)),
		"its SHA-256":       hex.EncodeToString(sum[:]),
		"the start of that": hex.EncodeToString(sum[:])[:12],
		"reversed":          reverse(value),
	}
	for _, mask := range []string{"*", "•", "x", "X", "."} {
		forms["a mask of its length ("+mask+")"] = strings.Repeat(mask, len(value))
	}
	return forms
}

func reverse(s string) string {
	r := []rune(s)
	for i, j := 0, len(r)-1; i < j; i, j = i+1, j-1 {
		r[i], r[j] = r[j], r[i]
	}
	return string(r)
}

// Every reply, every error and everything logged, over every call and every
// kind of refusal, with a value that is looked for in all of them in every form
// it could be shown in.
func TestAValueIsNotInAnythingTheServiceSaysAnywhere(t *testing.T) {
	f := newCredentialFixture(t, "[provider]\nbackend = \"gemini\"\n")

	var said []string
	say := func(parts ...any) {
		for _, part := range parts {
			switch v := part.(type) {
			case proto.Message:
				encoded, _ := proto.Marshal(v)
				said = append(said, string(encoded), protojson.Format(v), fmt.Sprintf("%v %+v", v, v))
			case *controlv1.Credential:
				encoded, _ := proto.Marshal(v)
				said = append(said, string(encoded), protojson.Format(v), fmt.Sprintf("%v %+v", v, v))
			case error:
				if v != nil {
					said = append(said, v.Error(), fmt.Sprintf("%v %+v", v, v))
				}
			}
		}
	}

	sayList := func(list []*controlv1.Credential) {
		for _, one := range list {
			say(one)
		}
	}

	ctx := context.Background()
	put, err := f.client.PutCredential(ctx, connect.NewRequest(&controlv1.PutCredentialRequest{Slot: gemini, Value: credentialCanary}))
	if err != nil {
		t.Fatalf("put: %v", err)
	}
	say(put.Msg)
	sayList(f.list(t))

	// Refused for every reason there is, each with the value in the request.
	for _, value := range []string{credentialCanary + "\n" + credentialCanary, credentialCanary + "\x00", credentialCanary + strings.Repeat("a", 9000)} {
		_, err := f.put(gemini, value)
		say(err)
	}
	_, err = f.put(anthropic, credentialCanary)
	say(err)
	_, err = f.client.PutCredential(ctx, connect.NewRequest(&controlv1.PutCredentialRequest{
		Slot: gemini, Value: credentialCanary, ExpectModifiedAt: timestamppb.New(time.Unix(1, 0)),
	}))
	say(err)
	_, err = f.client.PutCredential(ctx, connect.NewRequest(&controlv1.PutCredentialRequest{Value: credentialCanary}))
	say(err)

	if err := permtest.Expose(f.path("gemini.key")); err != nil {
		t.Fatal(err)
	}
	sayList(f.list(t))
	restricted, err := f.client.RestrictCredentialFile(ctx, connect.NewRequest(&controlv1.RestrictCredentialFileRequest{Slot: gemini}))
	if err != nil {
		t.Fatalf("restrict: %v", err)
	}
	say(restricted.Msg)
	deleted, err := f.client.DeleteCredential(ctx, connect.NewRequest(&controlv1.DeleteCredentialRequest{Slot: gemini}))
	if err != nil {
		t.Fatalf("delete: %v", err)
	}
	say(deleted.Msg)
	_, err = f.client.RestrictCredentialFile(ctx, connect.NewRequest(&controlv1.RestrictCredentialFileRequest{Slot: gemini}))
	say(err)

	// And with the value in the environment instead of a file.
	t.Setenv("GEMINI_API_KEY", credentialCanary)
	fromEnvironment := f.list(t)
	sayList(fromEnvironment)
	if got := fromEnvironment[0]; got.EnvInUse != "GEMINI_API_KEY" {
		t.Fatalf("the variable was not in use: %+v", got)
	}

	said = append(said, f.logs.String())
	if f.logs.Len() == 0 {
		t.Error("nothing was logged, so there is no log to look in")
	}

	for form, text := range leaks(credentialCanary) {
		for _, one := range said {
			if strings.Contains(one, text) {
				t.Errorf("%s is in what the service said: %.200s", form, one)
			}
		}
	}
	// What it does log is that something happened to a slot, and which.
	if logged := f.logs.String(); !strings.Contains(logged, "credential saved") || !strings.Contains(logged, "provider.gemini") {
		t.Errorf("the log does not say a credential was saved: %q", logged)
	}
	if logged := f.logs.String(); strings.Contains(logged, "gemini.key") {
		t.Errorf("the log names the file: %q", logged)
	}
}

// ── one writer at a time ───────────────────────────────────────────────

func TestSavesFromManyCallersLeaveOneWholeValueAndNothingBeside(t *testing.T) {
	f := newCredentialFixture(t, "")

	const callers = 24
	values := make(map[string]bool, callers)
	var group sync.WaitGroup
	for i := range callers {
		value := fmt.Sprintf("value-%02d-%s", i, strings.Repeat("v", 200))
		values[value] = true

		group.Add(2)
		go func() {
			defer group.Done()
			if _, err := f.put(gemini, value); err != nil {
				t.Errorf("save: %v", err)
			}
		}()
		go func() {
			defer group.Done()
			if _, err := f.client.ListCredentials(context.Background(), connect.NewRequest(&controlv1.ListCredentialsRequest{})); err != nil {
				t.Errorf("list: %v", err)
			}
		}()
	}
	group.Wait()

	content, err := os.ReadFile(f.path("gemini.key"))
	if err != nil || !values[string(content)] {
		t.Errorf("the file holds %q (%v), which is not one of the values saved", content, err)
	}
	if rest := f.rest(t); rest != "gemini.key" {
		t.Errorf("the deployment directory holds %q", rest)
	}
	if ok, _, _ := fsperm.EnsureOwnerOnly(f.path("gemini.key")); !ok {
		t.Error("the file can be read by others")
	}
}

// Saves that all started from the same version cannot all succeed. The check
// and the write are one step under one lock, so the first changes the time and
// every other finds it changed; two that both passed the check and then both
// wrote would each have discarded the other unseen.
func TestOnlyOneOfManySavesThatStartedFromTheSameVersionSucceeds(t *testing.T) {
	f := newCredentialFixture(t, "")
	path := f.keyFile(t, "gemini.key", "first")
	then := time.Now().Add(-time.Hour).Truncate(time.Second)
	if err := os.Chtimes(path, then, then); err != nil {
		t.Fatal(err)
	}

	const callers = 32
	var (
		group     sync.WaitGroup
		succeeded int
		refused   int
		counting  sync.Mutex
		start     = make(chan struct{})
	)
	for i := range callers {
		group.Add(1)
		go func() {
			defer group.Done()
			<-start

			_, err := f.client.PutCredential(context.Background(), connect.NewRequest(&controlv1.PutCredentialRequest{
				Slot: gemini, Value: fmt.Sprintf("from-%d", i), ExpectModifiedAt: timestamppb.New(then),
			}))

			counting.Lock()
			defer counting.Unlock()
			switch {
			case err == nil:
				succeeded++
			case code(err) == connect.CodeFailedPrecondition:
				refused++
			default:
				t.Errorf("save %d: %v", i, err)
			}
		}()
	}
	close(start)
	group.Wait()

	if succeeded != 1 || refused != callers-1 {
		t.Errorf("%d saves succeeded and %d were refused, want 1 and %d", succeeded, refused, callers-1)
	}
	if rest := f.rest(t); rest != "gemini.key" {
		t.Errorf("the deployment directory holds %q", rest)
	}
}
