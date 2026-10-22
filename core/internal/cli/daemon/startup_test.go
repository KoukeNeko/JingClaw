package daemon

import (
	"context"
	"io"
	"log/slog"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"connectrpc.com/connect"

	controlv1 "github.com/KoukeNeko/JingClaw/core/gen/go/jingclaw/control/v1"
	"github.com/KoukeNeko/JingClaw/core/gen/go/jingclaw/control/v1/controlv1connect"
	"github.com/KoukeNeko/JingClaw/core/internal/chatgptauth"
	"github.com/KoukeNeko/JingClaw/core/internal/config"
	"github.com/KoukeNeko/JingClaw/core/internal/control"
	"github.com/KoukeNeko/JingClaw/core/internal/discovery"
	"github.com/KoukeNeko/JingClaw/core/internal/home"
	"github.com/KoukeNeko/JingClaw/core/internal/storage"
	"github.com/KoukeNeko/JingClaw/core/internal/storage/sqlite"
)

// isolate gives a test a deployment of its own and an environment that says
// nothing, so that what a developer has exported cannot decide the outcome.
func isolate(t *testing.T) string {
	t.Helper()

	for _, pair := range os.Environ() {
		name, _, _ := strings.Cut(pair, "=")
		switch {
		case strings.HasPrefix(name, "JINGCLAW_"), name == "GEMINI_API_KEY", name == "GOOGLE_API_KEY",
			name == "ANTHROPIC_API_KEY", name == "BRAVE_SEARCH_TOKEN":
			// Unset, not blank: a blank JINGCLAW_ variable is a setting.
			t.Setenv(name, "")
			_ = os.Unsetenv(name)
		}
	}

	root := filepath.Join(t.TempDir(), home.DirName)
	t.Setenv(home.EnvVar, root)
	return root
}

// seedDatabase saves a document the way a dashboard would have, before any
// daemon has started on the deployment.
func seedDatabase(t *testing.T, root, name, text string) {
	t.Helper()

	data := filepath.Join(root, home.DataName)
	if err := os.MkdirAll(data, 0o700); err != nil {
		t.Fatal(err)
	}
	store, err := sqlite.Open(context.Background(), filepath.Join(data, "jingclaw.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = store.Close() }()

	if _, err := store.PutDocument(context.Background(), name, text, "web", "", time.Now()); err != nil {
		t.Fatal(err)
	}
}

// running is a daemon started in this process.
type running struct {
	t        *testing.T
	file     discovery.File
	cancel   context.CancelFunc
	finished chan struct{}
	err      error
}

// start runs the daemon the way the command does and returns once a client
// could find it.
func start(t *testing.T, root string, args ...string) *running {
	t.Helper()

	ctx, cancel := context.WithCancel(context.Background())
	d := &running{t: t, cancel: cancel, finished: make(chan struct{})}
	go func() {
		d.err = runContext(ctx, args)
		close(d.finished)
	}()
	t.Cleanup(func() { d.stop() })

	path := filepath.Join(root, home.RunName, "daemon.json")
	deadline := time.Now().Add(20 * time.Second)
	for time.Now().Before(deadline) {
		select {
		case <-d.finished:
			t.Fatalf("the daemon stopped before it was reachable: %v", d.err)
		default:
		}
		if file, err := discovery.Read(path); err == nil {
			d.file = file
			return d
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatal("the daemon never wrote its discovery file")
	return nil
}

// stop ends the daemon the way a signal does, and says how it went.
func (d *running) stop() {
	d.t.Helper()

	d.cancel()
	select {
	case <-d.finished:
	case <-time.After(20 * time.Second):
		d.t.Error("the daemon did not stop")
		return
	}
	if d.err != nil {
		d.t.Errorf("the daemon stopped with an error: %v", d.err)
	}
}

// runWithin runs the daemon for a start that is expected to end by itself.
//
// Bounded, so that a start which was meant to be refused and was not shows up
// as a failure and not as a test that serves until it is killed.
func runWithin(t *testing.T, args ...string) error {
	t.Helper()

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	err := runContext(ctx, args)
	if err == nil && ctx.Err() != nil {
		t.Fatal("the daemon was still serving when the test gave up on it")
	}
	return err
}

func bearer(token string) connect.Interceptor {
	return connect.UnaryInterceptorFunc(func(next connect.UnaryFunc) connect.UnaryFunc {
		return func(ctx context.Context, req connect.AnyRequest) (connect.AnyResponse, error) {
			req.Header().Set("Authorization", "Bearer "+token)
			return next(ctx, req)
		}
	})
}

func (d *running) settings() controlv1connect.ConfigServiceClient {
	return controlv1connect.NewConfigServiceClient(http.DefaultClient, d.file.BaseURL,
		connect.WithInterceptors(bearer(d.file.Token)))
}

func (d *running) state() *controlv1.GetAgentStateResponse {
	d.t.Helper()

	resp, err := d.settings().GetAgentState(context.Background(), connect.NewRequest(&controlv1.GetAgentStateRequest{}))
	if err != nil {
		d.t.Fatalf("GetAgentState: %v", err)
	}
	return resp.Msg
}

func (d *running) config() *controlv1.GetConfigResponse {
	d.t.Helper()

	resp, err := d.settings().GetConfig(context.Background(), connect.NewRequest(&controlv1.GetConfigRequest{}))
	if err != nil {
		d.t.Fatalf("GetConfig: %v", err)
	}
	return resp.Msg
}

// call posts an empty request to a method with a credential and says what the
// server answered.
func (d *running) call(method, token string) int {
	d.t.Helper()

	req, err := http.NewRequest(http.MethodPost, d.file.BaseURL+"/"+method, strings.NewReader("{}"))
	if err != nil {
		d.t.Fatal(err)
	}
	req.Header.Set("Content-Type", "application/json")
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	return d.do(req)
}

func (d *running) do(req *http.Request) int {
	d.t.Helper()

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		d.t.Fatal(err)
	}
	defer func() { _ = resp.Body.Close() }()
	_, _ = io.Copy(io.Discard, resp.Body)
	return resp.StatusCode
}

// problem finds the one a daemon reported for a key, and for "" the first
// without one.
func problem(state *controlv1.GetAgentStateResponse, key string) *controlv1.ConfigProblem {
	for _, found := range state.Problems {
		if found.Key == key {
			return found
		}
	}
	return nil
}

// Saved settings that will not do leave a daemon that says so.
//
// This used to be an exit with a report on a terminal. A deployment that is
// run by a supervisor, or in a container, or by somebody who does not have the
// terminal open, has no one reading that, and a daemon that is not there
// cannot be asked what was wrong with it.
func TestSettingsThatFailValidationStartARepairDaemonThatSaysWhy(t *testing.T) {
	root := isolate(t)
	seedDatabase(t, root, storage.DocumentConfig, "[provider]\nbackend = \"nonsense\"\n")

	d := start(t, root)

	if d.file.Mode != discovery.ModeRepair {
		t.Errorf("the discovery file says mode %q, want %q", d.file.Mode, discovery.ModeRepair)
	}
	if d.file.GatewayToken != "" {
		t.Error("a daemon with no ingress handed out a gateway token")
	}

	state := d.state()
	if state.Mode != "repair" {
		t.Errorf("the daemon says it is %q", state.Mode)
	}
	found := problem(state, "provider.backend")
	if found == nil {
		t.Fatalf("the problem with the setting is not listed: %v", state.Problems)
	}
	if found.Source != "settings" || found.Why == "" || found.Fix == "" || !strings.Contains(found.Value, "nonsense") {
		t.Errorf("the problem is %+v, want a setting with its value, why and fix", found)
	}
	if state.StartedAt == nil {
		t.Error("it does not say when it started")
	}

	// What is saved is shown, and nothing of it is running.
	saved := d.config()
	if !strings.Contains(saved.Text, "nonsense") || saved.ParseError != "" {
		t.Errorf("the saved text is %q with parse error %q", saved.Text, saved.ParseError)
	}
	if saved.RunningHash != "" || !saved.PendingRestart {
		t.Errorf("a daemon that runs nothing says its running hash is %q and pending restart %v",
			saved.RunningHash, saved.PendingRestart)
	}
}

// Text that cannot be read at all is the harder case, and the one the page has
// to be able to open: the reason is in the text, and the text is what is shown.
func TestSavedSettingsThatCannotBeReadStartARepairDaemonThatCanBeFixedAndRestarted(t *testing.T) {
	root := isolate(t)
	broken := "this is = not [toml\n"
	seedDatabase(t, root, storage.DocumentConfig, broken)

	d := start(t, root)

	state := d.state()
	if state.Mode != "repair" || len(state.Problems) == 0 {
		t.Fatalf("state is %+v, want repair with a reason", state)
	}
	if state.Problems[0].Source != "settings" || state.Problems[0].Why == "" {
		t.Errorf("the reason is %+v", state.Problems[0])
	}

	saved := d.config()
	if saved.Text != broken || saved.ParseError == "" || saved.File != nil && len(saved.File.Fields) != 0 {
		t.Errorf("the page is given text %q, parse error %q and file %v", saved.Text, saved.ParseError, saved.File)
	}

	// Corrected where the daemon can be reached, which is the point.
	fixed := "[agent]\nmax_iterations = 7\n"
	replaced, err := d.settings().ReplaceConfigText(context.Background(), connect.NewRequest(&controlv1.ReplaceConfigTextRequest{
		Text: fixed, Hash: saved.Hash,
	}))
	if err != nil || !replaced.Msg.Written || len(replaced.Msg.Problems) != 0 {
		t.Fatalf("the correction was not saved: %v %+v", err, replaced)
	}
	if !replaced.Msg.PendingRestart {
		t.Error("it does not say the correction is not running yet")
	}

	// Nothing reloads. The daemon stays what it started as until it is restarted.
	if state := d.state(); state.Mode != "repair" {
		t.Errorf("the daemon changed its mind without being restarted: %q", state.Mode)
	}
	d.stop()

	again := start(t, root)
	if again.file.Mode != "" || again.file.GatewayToken == "" {
		t.Errorf("after the restart the discovery file says mode %q and gateway token %q",
			again.file.Mode, again.file.GatewayToken)
	}
	if state := again.state(); state.Mode != "running" || len(state.Problems) != 0 {
		t.Errorf("after the restart the state is %+v", state)
	}
	now := again.config()
	if now.Text != fixed || now.RunningHash != config.Hash([]byte(fixed)) || now.PendingRestart {
		t.Errorf("the corrected text is not what runs: %q, running %q, pending %v",
			now.Text, now.RunningHash, now.PendingRestart)
	}
}

// A repair daemon is the same door with fewer rooms behind it.
//
// Every request is still held to the credential, the host check and the scope
// map. The rooms it does not have are not refused but absent: with the control
// token in hand, a service it does not serve is a path that does not exist.
func TestARepairDaemonServesOnlyWhatRepairNeedsBehindTheSameChecks(t *testing.T) {
	root := isolate(t)
	seedDatabase(t, root, storage.DocumentConfig, "[provider]\nbackend = \"nonsense\"\n")
	d := start(t, root)

	mounted := map[string]bool{
		controlv1connect.ConfigServiceName:      true,
		controlv1connect.CredentialServiceName:  true,
		controlv1connect.ChatGPTAuthServiceName: true,
		controlv1connect.BrowserServiceName:     true,
	}
	for service, method := range map[string]string{
		controlv1connect.SessionServiceName:        "ListSessions",
		controlv1connect.ArtifactServiceName:       "StatArtifact",
		controlv1connect.MemoryServiceName:         "ListMemories",
		controlv1connect.ChannelServiceName:        "ListBindings",
		controlv1connect.GatewayIngressServiceName: "GetGatewaySettings",
		controlv1connect.SkillServiceName:          "ListSkills",
	} {
		if mounted[service] {
			t.Fatalf("%s is expected to be unmounted and mounted at once", service)
		}
		if got := d.call(service+"/"+method, d.file.Token); got != http.StatusNotFound {
			t.Errorf("%s answers %d to the control token in repair mode, want %d", service, got, http.StatusNotFound)
		}
	}

	if got := d.call(controlv1connect.ConfigServiceName+"/GetAgentState", d.file.Token); got != http.StatusOK {
		t.Errorf("the settings answer %d to the control token", got)
	}
	if got := d.call(controlv1connect.CredentialServiceName+"/ListCredentials", d.file.Token); got != http.StatusOK {
		t.Errorf("the credentials answer %d to the control token", got)
	}
	if got := d.call(controlv1connect.BrowserServiceName+"/CreatePairing", d.file.Token); got != http.StatusOK {
		t.Errorf("the pairing answers %d to the control token", got)
	}
	if got := d.call(controlv1connect.ChatGPTAuthServiceName+"/GetChatGPTAccount", d.file.Token); got != http.StatusOK {
		t.Errorf("the ChatGPT account answers %d to the control token", got)
	}
	if got := d.call(controlv1connect.ChatGPTAuthServiceName+"/GetChatGPTAccount", ""); got != http.StatusUnauthorized {
		t.Errorf("the ChatGPT account answers %d to no credential", got)
	}
	if got := d.call(controlv1connect.CredentialServiceName+"/ListCredentials", ""); got != http.StatusUnauthorized {
		t.Errorf("the credentials answer %d to no credential", got)
	}

	// No credential, a wrong one, and the right one for a foreign host: nothing
	// about repair mode makes any of them work.
	if got := d.call(controlv1connect.ConfigServiceName+"/GetConfig", ""); got != http.StatusUnauthorized {
		t.Errorf("no credential gets %d", got)
	}
	if got := d.call(controlv1connect.ConfigServiceName+"/GetConfig", "not-the-token"); got != http.StatusUnauthorized {
		t.Errorf("a wrong credential gets %d", got)
	}
	foreign, _ := http.NewRequest(http.MethodPost, d.file.BaseURL+"/"+controlv1connect.ConfigServiceName+"/GetConfig", strings.NewReader("{}"))
	foreign.Host = "evil.example:80"
	foreign.Header.Set("Content-Type", "application/json")
	foreign.Header.Set("Authorization", "Bearer "+d.file.Token)
	if got := d.do(foreign); got != http.StatusForbidden {
		t.Errorf("the right credential on a foreign host gets %d", got)
	}

	// And the page is the one it always was.
	page, _ := http.NewRequest(http.MethodGet, d.file.BaseURL+"/ui/", nil)
	if got := d.do(page); got != http.StatusOK {
		t.Errorf("the page answers %d", got)
	}
	foreignPage, _ := http.NewRequest(http.MethodGet, d.file.BaseURL+"/ui/", nil)
	foreignPage.Host = "evil.example:80"
	if got := d.do(foreignPage); got != http.StatusForbidden {
		t.Errorf("the page answers %d to a foreign host", got)
	}
}

// The list of services is made in one place, and without an agent it is short.
//
// The routes are built from it. The credential check is not: it is the table of
// what each credential may ask for, written out on purpose so that a new service
// is reachable by no credential until somebody decides. The two are different
// questions, and a service that is not here is absent whatever the table says:
// with the control token, a request for one is a 404 and not a 403, which
// TestARepairDaemonServesOnlyWhatRepairNeedsBehindTheSameChecks asks.
func TestTheServicesAreChosenInOnePlace(t *testing.T) {
	store, err := sqlite.Open(context.Background(), filepath.Join(t.TempDir(), "jingclaw.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = store.Close() }()

	var got []string
	signin := chatgptauth.New(filepath.Join(t.TempDir(), chatgptauth.FileName), chatgptauth.DefaultEndpoints())
	for _, one := range (serving{store: store, signin: signin}).services(nil, control.AgentState{}) {
		got = append(got, one.path)
	}

	want := []string{
		"/" + controlv1connect.ConfigServiceName + "/",
		"/" + controlv1connect.CredentialServiceName + "/",
		"/" + controlv1connect.ChatGPTAuthServiceName + "/",
		"/" + controlv1connect.BrowserServiceName + "/",
	}
	if strings.Join(got, ",") != strings.Join(want, ",") {
		t.Errorf("a daemon with no agent serves %v, want %v", got, want)
	}
}

// Settings that read and pass can still not be run: a credential that is not
// there, a program that is not installed. They used to end the process with a
// line of text, and the line is now a reason the daemon can show.
func TestAnAgentThatCannotBeBuiltStartsARepairDaemonWithTheReasonItUsedToExitWith(t *testing.T) {
	t.Run("a credential that is missing", func(t *testing.T) {
		root := isolate(t)
		t.Setenv(config.FileEnvVar, "[provider]\nbackend = \"gemini\"\n")

		d := start(t, root)

		state := d.state()
		if state.Mode != "repair" || len(state.Problems) != 1 {
			t.Fatalf("state is %+v", state)
		}
		reason := state.Problems[0]
		if reason.Source != "startup" || !strings.Contains(reason.Why, "no Gemini API key") ||
			!strings.Contains(reason.Why, "GEMINI_API_KEY") || reason.Fix == "" {
			t.Errorf("the reason is %+v, want what the daemon used to print and how to try again", reason)
		}

		// The settings are fine, and are shown as such; only nothing is running.
		if saved := d.config(); saved.ParseError != "" || saved.RunningHash != "" {
			t.Errorf("config says parse error %q and running hash %q", saved.ParseError, saved.RunningHash)
		}
	})

	t.Run("the ChatGPT backend with nobody signed in", func(t *testing.T) {
		root := isolate(t)
		t.Setenv(config.FileEnvVar, "[provider]\nbackend = \"chatgpt\"\n")

		d := start(t, root)

		state := d.state()
		if state.Mode != "repair" || len(state.Problems) != 1 {
			t.Fatalf("state is %+v", state)
		}
		reason := state.Problems[0]
		if reason.Source != "startup" || !strings.Contains(reason.Why, "not signed in") ||
			!strings.Contains(reason.Fix, "dashboard") || !strings.Contains(reason.Fix, "ChatGPT") {
			t.Errorf("the reason is %+v, want that nobody is signed in and where to do it", reason)
		}

		// And repair mode is where the sign-in is made: it answers.
		if got := d.call(controlv1connect.ChatGPTAuthServiceName+"/GetChatGPTAccount", d.file.Token); got != http.StatusOK {
			t.Errorf("the ChatGPT account answers %d", got)
		}
	})

	t.Run("a credential that anyone could read", func(t *testing.T) {
		root := isolate(t)
		t.Setenv(config.FileEnvVar, "[provider]\nbackend = \"gemini\"\n")
		if err := os.MkdirAll(root, 0o700); err != nil {
			t.Fatal(err)
		}
		key := filepath.Join(root, "gemini.key")
		if err := os.WriteFile(key, []byte("not-a-real-key-but-secret-looking\n"), 0o644); err != nil {
			t.Fatal(err)
		}
		if err := os.Chmod(key, 0o644); err != nil {
			t.Fatal(err)
		}

		d := start(t, root)

		state := d.state()
		if state.Mode != "repair" || len(state.Problems) != 1 || state.Problems[0].Source != "startup" {
			t.Fatalf("state is %+v", state)
		}
		if !strings.Contains(state.Problems[0].Why, key) {
			t.Errorf("the reason does not say which file: %q", state.Problems[0].Why)
		}
		// The file is named and its contents are not, anywhere a client can look.
		for _, said := range []string{state.Problems[0].Why, state.Problems[0].Fix} {
			if strings.Contains(said, "secret-looking") {
				t.Errorf("a credential was returned: %q", said)
			}
		}
	})

	t.Run("a program that reading pages needs", func(t *testing.T) {
		root := isolate(t)
		t.Setenv(config.FileEnvVar,
			"[web]\nenabled = true\nbackend = \"browser\"\npython = \""+filepath.Join(root, "no-such-python3")+"\"\n")

		d := start(t, root)

		state := d.state()
		if state.Mode != "repair" || len(state.Problems) != 1 {
			t.Fatalf("state is %+v", state)
		}
		if !strings.Contains(state.Problems[0].Why, "web.enabled") {
			t.Errorf("the reason does not say how to run without it: %q", state.Problems[0].Why)
		}
	})
}

// A key that anybody could read is put right from the daemon it kept from
// starting, and the next start runs.
//
// The search key stands in for any: it is checked when the agent is built, as a
// provider's is, and needs no network to be.
func TestAKeyAnybodyCouldReadIsPutRightFromTheRepairDaemonItCaused(t *testing.T) {
	root := isolate(t)
	t.Setenv(config.FileEnvVar, "[web]\nenabled = true\nbackend = \"none\"\n[web.search]\nbackend = \"brave\"\n")
	if err := os.MkdirAll(root, 0o700); err != nil {
		t.Fatal(err)
	}
	key := filepath.Join(root, "brave-search.token")
	if err := os.WriteFile(key, []byte("not-a-real-token-but-secret-looking\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(key, 0o644); err != nil {
		t.Fatal(err)
	}

	d := start(t, root)
	if state := d.state(); state.Mode != "repair" {
		t.Fatalf("state is %+v, want a daemon with no agent", state)
	}

	credentials := controlv1connect.NewCredentialServiceClient(http.DefaultClient, d.file.BaseURL,
		connect.WithInterceptors(bearer(d.file.Token)))
	slot := controlv1.CredentialSlot_CREDENTIAL_SLOT_WEB_SEARCH

	listed, err := credentials.ListCredentials(context.Background(), connect.NewRequest(&controlv1.ListCredentialsRequest{}))
	if err != nil {
		t.Fatalf("ListCredentials: %v", err)
	}
	var before *controlv1.Credential
	for _, one := range listed.Msg.Credentials {
		if one.Slot == slot {
			before = one
		}
	}
	if before == nil || before.Source != controlv1.CredentialSource_CREDENTIAL_SOURCE_BLOCKED ||
		before.FileState != controlv1.CredentialFileState_CREDENTIAL_FILE_STATE_WRONG_MODE ||
		before.Need != controlv1.CredentialNeed_CREDENTIAL_NEED_REQUIRED {
		t.Fatalf("the repair daemon says %+v of the key that kept it from starting", before)
	}

	fixed, err := credentials.RestrictCredentialFile(context.Background(),
		connect.NewRequest(&controlv1.RestrictCredentialFileRequest{Slot: slot}))
	if err != nil {
		t.Fatalf("RestrictCredentialFile: %v", err)
	}
	if got := fixed.Msg.Credential; got.Source != controlv1.CredentialSource_CREDENTIAL_SOURCE_FILE || !got.RestartNeeded {
		t.Errorf("after restricting it: %+v", got)
	}
	if state := d.state(); state.Mode != "repair" {
		t.Errorf("the daemon changed its mind without being restarted: %q", state.Mode)
	}
	d.stop()

	again := start(t, root)
	if state := again.state(); state.Mode != "running" || len(state.Problems) != 0 {
		t.Errorf("after the restart the state is %+v", state)
	}
}

// Starting properly is starting as it was.
func TestANormalStartIsUnaffected(t *testing.T) {
	root := isolate(t)
	t.Setenv(config.PersonaEnvVar, "You are 江委員.\n")

	d := start(t, root)

	if d.file.Mode != "" || d.file.GatewayToken == "" {
		t.Errorf("the discovery file says mode %q and gateway token %q", d.file.Mode, d.file.GatewayToken)
	}

	state := d.state()
	if state.Mode != "running" || len(state.Problems) != 0 || state.StartedAt == nil {
		t.Errorf("state is %+v", state)
	}

	saved := d.config()
	if saved.Text != config.Example || saved.RunningHash != config.Hash([]byte(config.Example)) || saved.PendingRestart {
		t.Errorf("the example is not what runs: running %q, pending %v", saved.RunningHash, saved.PendingRestart)
	}

	persona, err := d.settings().GetInstructionFile(context.Background(), connect.NewRequest(&controlv1.GetInstructionFileRequest{
		File: controlv1.InstructionFile_INSTRUCTION_FILE_PERSONA,
	}))
	if err != nil {
		t.Fatalf("GetInstructionFile: %v", err)
	}
	if persona.Msg.Text != "You are 江委員.\n" || persona.Msg.RunningHash != persona.Msg.Hash || persona.Msg.PendingRestart {
		t.Errorf("the persona is %+v", persona.Msg)
	}

	// The rooms are all there.
	for service, method := range map[string]string{
		controlv1connect.SessionServiceName: "ListSessions",
		controlv1connect.MemoryServiceName:  "ListMemories",
		controlv1connect.ChannelServiceName: "ListBindings",
		controlv1connect.SkillServiceName:   "ListSkills",
	} {
		if got := d.call(service+"/"+method, d.file.Token); got != http.StatusOK {
			t.Errorf("%s answers %d to the control token", service, got)
		}
	}
	if got := d.call(controlv1connect.SessionServiceName+"/ListSessions", d.file.GatewayToken); got != http.StatusForbidden {
		t.Errorf("the gateway token reaches the sessions: %d", got)
	}

	// The credentials are served with an agent as well as without one, and are
	// the operator's alone.
	if got := d.call(controlv1connect.CredentialServiceName+"/ListCredentials", d.file.Token); got != http.StatusOK {
		t.Errorf("the credentials answer %d to the control token", got)
	}
	for _, method := range []string{"ListCredentials", "PutCredential", "DeleteCredential", "RestrictCredentialFile"} {
		if got := d.call(controlv1connect.CredentialServiceName+"/"+method, d.file.GatewayToken); got != http.StatusForbidden {
			t.Errorf("the gateway token reaches %s: %d", method, got)
		}
	}

	// The skills are served with an agent and not without one, and are the
	// operator's alone.
	for _, method := range []string{"ListSkills", "StageSkill", "GetStagedSkill", "ActivateSkill", "DiscardStagedSkill", "RemoveSkill"} {
		if got := d.call(controlv1connect.SkillServiceName+"/"+method, d.file.GatewayToken); got != http.StatusForbidden {
			t.Errorf("the gateway token reaches %s: %d", method, got)
		}
	}
}

// The first start after an upgrade runs on what the files said, and the files
// are no longer what it reads.
func TestTheFirstStartAfterAnUpgradeRunsOnTheImportedFiles(t *testing.T) {
	root := isolate(t)
	if _, err := home.Create(root); err != nil {
		t.Fatal(err)
	}
	configText := "# mine\n[agent]\nmax_iterations = 7\n"
	for name, text := range map[string]string{
		home.ConfigName:         configText,
		config.InstructionsFile: "Prefer table-driven tests.\n",
		config.PersonaFile:      "Never chatty.\n",
	} {
		if err := os.WriteFile(filepath.Join(root, name), []byte(text), 0o600); err != nil {
			t.Fatal(err)
		}
	}

	d := start(t, root)

	saved := d.config()
	if saved.Text != configText || saved.RunningHash != config.Hash([]byte(configText)) {
		t.Errorf("the settings are %q, running %q", saved.Text, saved.RunningHash)
	}
	if got := saved.Running.GetFields()["agent.max_iterations"].GetNumberValue(); got != 7 {
		t.Errorf("the daemon runs with agent.max_iterations %v, want the imported 7", got)
	}

	for _, name := range []string{home.ConfigName, config.InstructionsFile, config.PersonaFile} {
		if _, err := os.Stat(filepath.Join(root, name)); !os.IsNotExist(err) {
			t.Errorf("%s is still there (%v)", name, err)
		}
		if _, err := os.Stat(filepath.Join(root, name+".imported")); err != nil {
			t.Errorf("%s.imported: %v", name, err)
		}
	}
}

// A start that would open a different database from the one a file belongs to
// does not start at all: it is a mistake an operator can fix in one line, and a
// repair daemon on an empty database would hide it.
func TestAStartThatWouldOpenAnotherDatabaseIsRefused(t *testing.T) {
	root := isolate(t)
	if _, err := home.Create(root); err != nil {
		t.Fatal(err)
	}
	elsewhere := filepath.Join(t.TempDir(), "elsewhere")
	if err := os.WriteFile(filepath.Join(root, home.ConfigName), []byte("[server]\ndata_dir = "+quoted(elsewhere)+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	err := runWithin(t)
	if err == nil || !strings.Contains(err.Error(), "JINGCLAW_SERVER_DATA_DIR="+elsewhere) {
		t.Fatalf("the start was not refused with the variable to set: %v", err)
	}
	if _, statErr := os.Stat(filepath.Join(root, home.ConfigName)); statErr != nil {
		t.Errorf("the file was moved by a start that was refused: %v", statErr)
	}
}

// What the text says about where the database is does not matter, because it
// cannot be read from the database it would be pointing at.
func TestSavedTextCannotMoveTheDatabase(t *testing.T) {
	root := isolate(t)
	seedDatabase(t, root, storage.DocumentConfig, "[server]\ndata_dir = \"/nonexistent/elsewhere\"\nruntime_dir = \"/nonexistent/run\"\n")

	d := start(t, root)

	if state := d.state(); state.Mode != "running" {
		t.Fatalf("a text that names a place the daemon cannot use stopped it from starting: %+v", state)
	}
	saved := d.config()
	for _, key := range []string{"server.data_dir", "server.runtime_dir"} {
		if got := saved.Running.GetFields()[key].GetStringValue(); got != "" {
			t.Errorf("the daemon runs with %s %q from the saved text", key, got)
		}
		if _, listed := saved.File.GetFields()[key]; listed {
			t.Errorf("%s is offered as a saved setting", key)
		}
	}
	if _, err := os.Stat(filepath.Join(root, home.RunName, "daemon.json")); err != nil {
		t.Errorf("the discovery file is not where the process was told: %v", err)
	}
}

// The prompt is made of the documents that were read, and of nothing a file
// says. It is what --print-prompt is for.
func TestThePromptIsBuiltFromTheSavedDocumentsAndNotFromFiles(t *testing.T) {
	root := isolate(t)
	t.Setenv(config.PersonaEnvVar, "You are 江委員.\n")
	t.Setenv(config.InstructionsEnvVar, "Prefer table-driven tests.\n")

	asked := func() string {
		t.Helper()
		return capturingStdout(t, func() {
			if err := Run(context.Background(), []string{"--print-prompt"}); err != nil {
				t.Fatalf("--print-prompt: %v", err)
			}
		})
	}

	first := asked()
	for _, want := range []string{"From PERSONA.md:\n\nYou are 江委員.", "From AGENTS.md:\n\nPrefer table-driven tests."} {
		if !strings.Contains(first, want) {
			t.Errorf("the prompt does not say %q:\n%s", want, first)
		}
	}
	if strings.Index(first, "AGENTS.md") > strings.Index(first, "PERSONA.md") {
		t.Errorf("the documents are not in the order they always were:\n%s", first)
	}

	// A file in the directory afterwards is not a thing the prompt reads.
	if err := os.WriteFile(filepath.Join(root, config.PersonaFile), []byte("A file that is ignored.\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if again := asked(); strings.Contains(again, "A file that is ignored.") {
		t.Errorf("the prompt was built from a file:\n%s", again)
	}
}

// A trial run with a database of its own leaves the deployment's files where
// they are.
//
// The files are what the deployment's own database is made from, and a trial
// that set them aside would have taken them into a database nobody opens again.
func TestAQuestionAskedWithADatabaseOfItsOwnLeavesTheDeploymentsFilesAlone(t *testing.T) {
	root := isolate(t)
	if err := os.MkdirAll(root, 0o700); err != nil {
		t.Fatal(err)
	}
	for name, text := range map[string]string{home.ConfigName: "[agent]\nmax_iterations = 7\n", config.InstructionsFile: "my rules\n"} {
		if err := os.WriteFile(filepath.Join(root, name), []byte(text), 0o600); err != nil {
			t.Fatal(err)
		}
	}

	trial := capturingStdout(t, func() {
		if err := Run(context.Background(), []string{"--data-dir", filepath.Join(t.TempDir(), "scratch"), "--print-prompt"}); err != nil {
			t.Fatalf("--print-prompt: %v", err)
		}
	})
	if !strings.Contains(trial, "my rules") {
		t.Errorf("the trial did not answer from the files:\n%s", trial)
	}
	for _, name := range []string{home.ConfigName, config.InstructionsFile} {
		if _, err := os.Stat(filepath.Join(root, name)); err != nil {
			t.Errorf("%s was taken by a trial run: %v", name, err)
		}
	}

	// The deployment's own start is the one that takes them.
	real := capturingStdout(t, func() {
		if err := Run(context.Background(), []string{"--print-prompt"}); err != nil {
			t.Fatalf("--print-prompt: %v", err)
		}
	})
	if !strings.Contains(real, "my rules") {
		t.Errorf("the deployment's own database did not get the files:\n%s", real)
	}
}

// A configuration in a variable that no start could use is refused, and
// correcting the variable is enough.
//
// Saved, it would have outlasted the correction: a row is never replaced by a
// variable, and where a container is the only place the variable is set there
// is no dashboard to correct the row in.
func TestACorrectedConfigVariableTakesEffectAfterAMistakenOneWasRefused(t *testing.T) {
	root := isolate(t)
	t.Setenv(config.FileEnvVar, "[agent]\nmax_iterations = -5\n")

	err := runWithin(t)
	if err == nil || !strings.Contains(err.Error(), config.FileEnvVar) {
		t.Fatalf("a configuration that cannot run did not stop the start by name: %v", err)
	}

	t.Setenv(config.FileEnvVar, "[agent]\nmax_iterations = 5\n")
	d := start(t, root)

	if state := d.state(); state.Mode == "repair" {
		t.Errorf("the daemon is in repair mode after the variable was corrected: %v", state.Problems)
	}
	if saved := d.config(); !strings.Contains(saved.Text, "max_iterations = 5") {
		t.Errorf("the saved text is %q, want the corrected variable", saved.Text)
	}
}

// A question about settings that cannot answer is an error, as it was. There
// is no daemon to repair for a command that was only going to print.
func TestAskingForThePromptWithSettingsThatDoNotPassIsAnError(t *testing.T) {
	root := isolate(t)
	seedDatabase(t, root, storage.DocumentConfig, "[provider]\nbackend = \"nonsense\"\n")

	if err := runWithin(t, "--print-prompt"); err == nil {
		t.Fatal("a question that could not be answered returned no error")
	}
}

func TestAskingForTheModelsStillAnswers(t *testing.T) {
	isolate(t)

	said := capturingStdout(t, func() {
		if err := Run(context.Background(), []string{"--list-models"}); err != nil {
			t.Fatalf("--list-models: %v", err)
		}
	})
	if strings.TrimSpace(said) == "" {
		t.Error("it listed nothing")
	}
}

// capturingStdout returns what fn printed.
func capturingStdout(t *testing.T, fn func()) string {
	t.Helper()

	reader, writer, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	was := os.Stdout
	os.Stdout = writer
	defer func() { os.Stdout = was }()

	done := make(chan string)
	go func() {
		out, _ := io.ReadAll(reader)
		done <- string(out)
	}()

	fn()
	_ = writer.Close()
	return <-done
}

// --init makes the places things go and nothing for anybody to edit.
//
// The settings are made by the first start, in the database. A file written
// here would be one that nothing reads, which is worse than none: somebody
// would edit it.
func TestInitCreatesDirectoriesAndNoSettingsFiles(t *testing.T) {
	root := isolate(t)

	said := capturingStdout(t, func() {
		if err := Run(context.Background(), []string{"--init"}); err != nil {
			t.Fatalf("--init: %v", err)
		}
	})

	listing, err := os.ReadDir(root)
	if err != nil {
		t.Fatal(err)
	}
	var found []string
	for _, entry := range listing {
		if !entry.IsDir() {
			t.Errorf("--init created a file: %s", entry.Name())
		}
		found = append(found, entry.Name())
	}
	for _, want := range []string{home.WorkspaceName, home.DataName, home.RunName} {
		if !strings.Contains(strings.Join(found, " "), want) {
			t.Errorf("--init did not create %s (made %v)", want, found)
		}
	}

	for _, stale := range []string{home.ConfigName, config.InstructionsFile, config.PersonaFile} {
		if strings.Contains(said, stale) {
			t.Errorf("--init talks about %s, which it does not make:\n%s", stale, said)
		}
	}
	if !strings.Contains(said, "dashboard") {
		t.Errorf("--init does not say where the settings are edited:\n%s", said)
	}
}

// An address this API must never be on is one of the mistakes the settings can
// hold, so a daemon with no agent cannot take its address from them.
func TestARepairDaemonListensOnLoopbackWhateverTheSettingsSay(t *testing.T) {
	logger := slog.New(slog.DiscardHandler)

	for _, addr := range []string{"0.0.0.0:0", "[::]:0", "10.1.2.3:0", "not an address", ""} {
		listener, err := listen("", addr, true, logger)
		if err != nil {
			t.Errorf("%q: %v", addr, err)
			continue
		}
		if ip := listener.Addr().(*net.TCPAddr).IP; !ip.IsLoopback() {
			t.Errorf("%q: listening on %s", addr, ip)
		}
		_ = listener.Close()
	}

	// The port is kept when it can be, so that what was told where to find the
	// daemon still can.
	probe, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	port := probe.Addr().(*net.TCPAddr).Port
	_ = probe.Close()

	kept, err := listen("", net.JoinHostPort("0.0.0.0", strconv.Itoa(port)), true, logger)
	if err != nil {
		t.Fatalf("the port named by the settings: %v", err)
	}
	if got := kept.Addr().(*net.TCPAddr).Port; got != port {
		t.Errorf("listening on %d, want the named %d", got, port)
	}

	// And a port that is taken is not a reason to be unreachable.
	moved, err := listen("", net.JoinHostPort("0.0.0.0", strconv.Itoa(port)), true, logger)
	if err != nil {
		t.Fatalf("with the port taken: %v", err)
	}
	if got := moved.Addr().(*net.TCPAddr).Port; got == port {
		t.Errorf("two listeners on %d", got)
	}
	_ = moved.Close()
	_ = kept.Close()

	// A daemon with an agent is not so forgiving: the settings were checked,
	// and an address that cannot be used is a reason not to start.
	if listener, err := listen("", "not an address", false, logger); err == nil {
		_ = listener.Close()
		t.Error("a daemon with an agent listened on an address that is not one")
	}
}
