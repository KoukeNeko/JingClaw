package control

import (
	"bytes"
	"context"
	"encoding/json"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"connectrpc.com/connect"
	"google.golang.org/protobuf/reflect/protoreflect"
	"google.golang.org/protobuf/reflect/protoregistry"

	controlv1 "github.com/KoukeNeko/JingClaw/core/gen/go/jingclaw/control/v1"
)

const envPassword = "secret-from-the-environment"

func decodeLogin(t *testing.T, rec *httptest.ResponseRecorder) map[string]string {
	t.Helper()
	if rec.Code != http.StatusOK {
		t.Fatalf("the login got %d, want 200", rec.Code)
	}
	var got map[string]string
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatal(err)
	}
	return got
}

func TestWithNoPasswordSetTheInitialPasswordIsTheBuiltInDefaultAndAChangeIsRequired(t *testing.T) {
	var logs bytes.Buffer
	b := open(t, t.TempDir(), newClock(), &logs)

	if !b.Initial() {
		t.Fatal("with no password file the state is not initial")
	}
	if rec := login(t, LoginHandler(b), "jingclaw-initial-not", nil); rec.Code != http.StatusUnauthorized {
		t.Errorf("a wrong password got %d", rec.Code)
	}
	got := decodeLogin(t, login(t, LoginHandler(b), DefaultInitialPassword, nil))
	if got["scope"] != string(ScopeSetup) {
		t.Errorf("the initial password gave scope %q, want %q", got["scope"], ScopeSetup)
	}
	if !strings.Contains(logs.String(), "dashboard password: initial (change it at first login)") || !strings.Contains(logs.String(), "built-in default") {
		t.Errorf("the start did not say the password is initial: %q", logs.String())
	}
	if strings.Contains(logs.String(), DefaultInitialPassword) {
		t.Error("the log holds the initial password")
	}
}

func TestTheEnvironmentGivesTheInitialPasswordWhenItIsValidAndTheDefaultNoLongerWorks(t *testing.T) {
	t.Setenv(EnvInitialPassword, envPassword)
	var logs bytes.Buffer
	b := open(t, t.TempDir(), newClock(), &logs)

	if rec := login(t, LoginHandler(b), DefaultInitialPassword, nil); rec.Code != http.StatusUnauthorized {
		t.Errorf("the default worked although the environment sets one: %d", rec.Code)
	}
	if got := decodeLogin(t, login(t, LoginHandler(b), envPassword, nil)); got["scope"] != string(ScopeSetup) {
		t.Errorf("the environment's password gave scope %q", got["scope"])
	}
	if !strings.Contains(logs.String(), "from environment") || strings.Contains(logs.String(), envPassword) {
		t.Errorf("the log should say where it came from and not what it is: %q", logs.String())
	}
}

func TestAnInvalidEnvironmentValueIsWarnedAboutWithoutItsValueAndTheDefaultIsUsed(t *testing.T) {
	for name, value := range map[string]string{"too short": "bad-9chrs", "empty": "", "too long": strings.Repeat("x", 257)} {
		t.Run(name, func(t *testing.T) {
			t.Setenv(EnvInitialPassword, value)
			var logs bytes.Buffer
			b := open(t, t.TempDir(), newClock(), &logs)

			if got := decodeLogin(t, login(t, LoginHandler(b), DefaultInitialPassword, nil)); got["scope"] != string(ScopeSetup) {
				t.Errorf("the default did not work: %v", got)
			}
			if n := strings.Count(logs.String(), "level=WARN"); n != 1 {
				t.Errorf("%d warnings, want one: %q", n, logs.String())
			}
			if value != "" && strings.Contains(logs.String(), value) {
				t.Error("the warning holds the value")
			}
		})
	}
}

func TestOnceChangedTheEnvironmentIsIgnoredEvenAfterARestart(t *testing.T) {
	t.Setenv(EnvInitialPassword, envPassword)
	dir := t.TempDir()
	clock := newClock()
	b := open(t, dir, clock, nil)
	if _, _, err := b.SetPassword(goodPassword, "", ""); err != nil {
		t.Fatal(err)
	}

	restarted := open(t, dir, clock, nil)
	if restarted.Initial() {
		t.Fatal("the restart is initial again")
	}
	if rec := login(t, LoginHandler(restarted), envPassword, nil); rec.Code != http.StatusUnauthorized {
		t.Errorf("the environment's value still logs in: %d", rec.Code)
	}
	if got := decodeLogin(t, login(t, LoginHandler(restarted), goodPassword, nil)); got["scope"] != string(ScopeControl) {
		t.Errorf("the chosen password gave scope %q, want control", got["scope"])
	}
}

func TestASetupSessionIsInMemoryOnlyEndsAfterFifteenMinutesAndDoesNotSurviveARestart(t *testing.T) {
	dir := t.TempDir()
	clock := newClock()
	b := open(t, dir, clock, nil)

	token, expires, err := b.Login(DefaultInitialPassword)
	if err != nil {
		t.Fatal(err)
	}
	if want := clock.at.Add(15 * time.Minute); !expires.Equal(want) {
		t.Errorf("the setup session ends at %v, want %v", expires, want)
	}
	if scope, ok := b.Lookup(token.Value); !ok || scope != ScopeSetup {
		t.Fatalf("lookup gave %q, %v", scope, ok)
	}
	if _, err := os.Stat(filepath.Join(dir, SessionsFile)); !os.IsNotExist(err) {
		t.Errorf("a setup session caused a sessions file to be written: %v", err)
	}
	if _, err := os.Stat(filepath.Join(dir, PasswordFile)); !os.IsNotExist(err) {
		t.Errorf("signing in wrote a password file: %v", err)
	}
	if _, ok := open(t, dir, clock, nil).Lookup(token.Value); ok {
		t.Error("a setup session survived a restart")
	}

	clock.at = clock.at.Add(15*time.Minute - time.Second)
	if _, ok := b.Lookup(token.Value); !ok {
		t.Error("the setup session ended early")
	}
	clock.at = clock.at.Add(time.Second)
	if _, ok := b.Lookup(token.Value); ok {
		t.Error("the setup session outlived fifteen minutes")
	}
}

func TestASetupSessionMayCallOnlySetDashboardPasswordAndNothingElseWhateverTheService(t *testing.T) {
	b := open(t, t.TempDir(), newClock(), nil)
	setup, _, _ := b.Login(DefaultInitialPassword)
	paired, _, _ := b.Redeem(mustPair(t, b))
	operator := Token{Value: "operator-token", Scope: ScopeControl}

	reached := 0
	handler := AuthMiddleware([]Token{operator}, b, "1234",
		http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { reached++ }))

	call := func(path, bearer string) int {
		req := httptest.NewRequest(http.MethodPost, "http://127.0.0.1:1234"+path, nil)
		req.Header.Set("Authorization", "Bearer "+bearer)
		rec := httptest.NewRecorder()
		handler.ServeHTTP(rec, req)
		return rec.Code
	}

	methods := 0
	protoregistry.GlobalFiles.RangeFilesByPackage("jingclaw.control.v1", func(file protoreflect.FileDescriptor) bool {
		for i := range file.Services().Len() {
			service := file.Services().Get(i)
			for j := range service.Methods().Len() {
				methods++
				path := "/" + string(service.FullName()) + "/" + string(service.Methods().Get(j).Name())
				want := http.StatusForbidden
				if path == "/jingclaw.control.v1.BrowserService/SetDashboardPassword" {
					want = http.StatusOK
				}
				if got := call(path, setup.Value); got != want {
					t.Errorf("a setup session calling %s got %d, want %d", path, got, want)
				}
				// The others are untouched by the new scope.
				for who, bearer := range map[string]string{"operator": operator.Value, "a paired browser": paired.Value} {
					if got := call(path, bearer); got != http.StatusOK {
						t.Errorf("%s calling %s got %d, want 200", who, path, got)
					}
				}
			}
		}
		return true
	})
	if methods < 20 {
		t.Fatalf("only %d methods were found, so the protos were not read", methods)
	}

	for _, path := range []string{
		"/jingclaw.control.v1.BrowserService/CreatePairing",
		"/jingclaw.control.v1.BrowserService/ClearDashboardPassword",
		"/jingclaw.control.v1.BrowserService/SetDashboardPasswordX",
		"/jingclaw.control.v1.BrowserService/SetDashboardPassword/extra",
		"/jingclaw.control.v1.BrowserService/",
		"/jingclaw.control.v1.NoSuchService/SetDashboardPassword",
		"/",
	} {
		if got := call(path, setup.Value); got != http.StatusForbidden {
			t.Errorf("a setup session calling %q got %d, want 403", path, got)
		}
	}
	if _, ok := servicesForScope[ScopeSetup]; ok {
		t.Error("the setup scope is given whole services")
	}
	if reached != methods*2+1 {
		t.Errorf("%d calls got through, want %d", reached, methods*2+1)
	}
}

func mustPair(t *testing.T, b *Browsers) string {
	t.Helper()
	code, _, err := b.Pair()
	if err != nil {
		t.Fatal(err)
	}
	return code
}

func TestChangingFromASetupSessionReturnsAControlSessionAndSignsEveryoneElseOut(t *testing.T) {
	dir := t.TempDir()
	clock := newClock()
	b := open(t, dir, clock, nil)
	server := NewBrowserServer(b)

	setup, _, _ := b.Login(DefaultInitialPassword)
	otherSetup, _, _ := b.Login(DefaultInitialPassword)
	paired, _, _ := b.Redeem(mustPair(t, b))

	req := connect.NewRequest(&controlv1.SetDashboardPasswordRequest{NewPassword: goodPassword})
	req.Header().Set("Authorization", "Bearer "+setup.Value)
	res, err := server.SetDashboardPassword(context.Background(), req)
	if err != nil {
		t.Fatal(err)
	}
	if res.Msg.GetToken() == "" || res.Msg.GetExpiresAt() == nil {
		t.Fatalf("the reply holds no session: %+v", res.Msg)
	}
	if scope, ok := b.Lookup(res.Msg.GetToken()); !ok || scope != ScopeControl {
		t.Errorf("the returned session has scope %q, %v; want control", scope, ok)
	}
	for who, token := range map[string]string{"the setup session": setup.Value, "another setup session": otherSetup.Value, "a paired browser": paired.Value} {
		if _, ok := b.Lookup(token); ok {
			t.Errorf("%s is still in after the change", who)
		}
	}
	if b.Initial() {
		t.Error("the password is still initial")
	}
	if _, ok := open(t, dir, clock, nil).Lookup(res.Msg.GetToken()); !ok {
		t.Error("the returned session did not survive a restart")
	}

	// A caller that is not a setup session gets no new session.
	req = connect.NewRequest(&controlv1.SetDashboardPasswordRequest{NewPassword: "another good password", CurrentPassword: goodPassword})
	req.Header().Set("Authorization", "Bearer "+res.Msg.GetToken())
	res2, err := server.SetDashboardPassword(context.Background(), req)
	if err != nil || res2.Msg.GetToken() != "" {
		t.Errorf("a change from a control session returned %+v, %v", res2.Msg, err)
	}
}

func TestTheNewPasswordCannotBeTheInitialOneWhateverWhereItCameFrom(t *testing.T) {
	for name, env := range map[string]string{"default": "", "environment": envPassword} {
		t.Run(name, func(t *testing.T) {
			initial := DefaultInitialPassword
			if env != "" {
				t.Setenv(EnvInitialPassword, env)
				initial = env
			}
			b := open(t, t.TempDir(), newClock(), nil)
			setup, _, _ := b.Login(initial)

			_, _, err := b.SetPassword(initial, "", setup.Value)
			if err != ErrPasswordInitial {
				t.Fatalf("got %v, want ErrPasswordInitial", err)
			}
			if connect.CodeOf(passwordError(err)) != connect.CodeFailedPrecondition {
				t.Error("the service does not answer it as a failed precondition")
			}
			if !b.Initial() {
				t.Error("a refused change ended the initial state")
			}
			if _, ok := b.Lookup(setup.Value); !ok {
				t.Error("a refused change ended the setup session")
			}
			// Nor later, from a chosen password.
			if _, _, err := b.SetPassword(goodPassword, "", ""); err != nil {
				t.Fatal(err)
			}
			if _, _, err := b.SetPassword(initial, goodPassword, ""); err != ErrPasswordInitial {
				t.Errorf("changing back to the initial one: %v", err)
			}
		})
	}
}

func TestResettingToInitialDeletesTheFileSignsEveryoneOutAndRequiresAChangeAgain(t *testing.T) {
	dir := t.TempDir()
	clock := newClock()
	b := open(t, dir, clock, nil)
	_, _, _ = b.SetPassword(goodPassword, "", "")
	one, _, _ := b.Login(goodPassword)
	two, _, _ := b.Login(goodPassword)

	var logs bytes.Buffer
	b.logger = slog_to(&logs)
	if err := b.ClearPassword(goodPassword); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(logs.String(), "reset to the initial one") || strings.Contains(logs.String(), goodPassword) {
		t.Errorf("the reset was not logged cleanly: %q", logs.String())
	}
	for _, token := range []Token{one, two} {
		if _, ok := b.Lookup(token.Value); ok {
			t.Error("a session survived the reset")
		}
	}
	if _, err := os.Stat(filepath.Join(dir, PasswordFile)); !os.IsNotExist(err) {
		t.Errorf("the password file is still there: %v", err)
	}
	if got := decodeLogin(t, login(t, LoginHandler(b), DefaultInitialPassword, nil)); got["scope"] != string(ScopeSetup) {
		t.Errorf("after the reset the initial password gave %q", got["scope"])
	}
	if rec := login(t, LoginHandler(b), goodPassword, nil); rec.Code != http.StatusUnauthorized {
		t.Errorf("the old password still logs in: %d", rec.Code)
	}
	// Resetting what is already initial is harmless and needs no password.
	if err := b.ClearPassword(""); err != nil {
		t.Errorf("resetting the initial state: %v", err)
	}
}

func TestGuessesAtTheInitialPasswordAreThrottledExactlyLikeAnyOther(t *testing.T) {
	clock := newClock()
	b := open(t, t.TempDir(), clock, nil)
	handler := LoginHandler(b)

	for i := 0; i < 5; i++ {
		if rec := login(t, handler, "guess", nil); rec.Code != http.StatusUnauthorized {
			t.Fatalf("attempt %d got %d", i+1, rec.Code)
		}
	}
	rec := login(t, handler, DefaultInitialPassword, nil)
	if rec.Code != http.StatusTooManyRequests || rec.Header().Get("Retry-After") != "1" {
		t.Fatalf("the right initial password during the throttle: %d, Retry-After %q", rec.Code, rec.Header().Get("Retry-After"))
	}
	clock.at = clock.at.Add(time.Second)
	if rec := login(t, handler, DefaultInitialPassword, nil); rec.Code != http.StatusOK {
		t.Errorf("after the wait: %d", rec.Code)
	}
	// The service's own check of a current password shares the same count.
	_, _, _ = b.SetPassword(goodPassword, "", "")
	for i := 0; i < 5; i++ {
		_, _, _ = b.SetPassword("another good password", "wrong", "")
	}
	if rec := login(t, handler, goodPassword, nil); rec.Code != http.StatusTooManyRequests {
		t.Errorf("guesses through the service did not count: %d", rec.Code)
	}
}

func TestAuthSaysThePasswordIsSetAndWhetherItIsInitialAndNeverWhatItIs(t *testing.T) {
	t.Setenv(EnvInitialPassword, envPassword)
	b := open(t, t.TempDir(), newClock(), nil)
	probe := func() string {
		rec := httptest.NewRecorder()
		AuthHandler(b).ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/ui/auth", nil))
		return strings.TrimSpace(rec.Body.String())
	}
	if got := probe(); got != `{"initial":true,"passwordSet":true}` {
		t.Errorf("in the initial state the page is told %q", got)
	}
	_, _, _ = b.SetPassword(goodPassword, "", "")
	if got := probe(); got != `{"initial":false,"passwordSet":true}` {
		t.Errorf("after a change the page is told %q", got)
	}
	for _, secret := range []string{DefaultInitialPassword, envPassword} {
		if strings.Contains(probe(), secret) {
			t.Error("the answer holds an initial password")
		}
	}
}

func TestNoPasswordAppearsInTheLogsFilesOrAnswersInTheInitialStateOrAfterAChange(t *testing.T) {
	canary := "canary-initial-5b1e77c2"
	t.Setenv(EnvInitialPassword, canary)
	dir := t.TempDir()
	var logs bytes.Buffer
	b := open(t, dir, newClock(), &logs)
	server := NewBrowserServer(b)

	var answers strings.Builder
	rec := login(t, LoginHandler(b), canary, nil)
	answers.WriteString(rec.Body.String())
	answers.WriteString(login(t, LoginHandler(b), canary+"x", nil).Body.String())
	probe := httptest.NewRecorder()
	AuthHandler(b).ServeHTTP(probe, httptest.NewRequest(http.MethodGet, "/ui/auth", nil))
	answers.WriteString(probe.Body.String())

	setup := decodeLogin(t, rec)["token"]
	chosen := "canary-chosen-9d0a4e17"
	for _, newPassword := range []string{canary, "short"} {
		req := connect.NewRequest(&controlv1.SetDashboardPasswordRequest{NewPassword: newPassword})
		req.Header().Set("Authorization", "Bearer "+setup)
		_, err := server.SetDashboardPassword(context.Background(), req)
		if err == nil {
			t.Fatal("a refused password was accepted")
		}
		answers.WriteString(err.Error())
	}
	req := connect.NewRequest(&controlv1.SetDashboardPasswordRequest{NewPassword: chosen})
	req.Header().Set("Authorization", "Bearer "+setup)
	res, err := server.SetDashboardPassword(context.Background(), req)
	if err != nil {
		t.Fatal(err)
	}
	answers.WriteString(res.Msg.String())
	answers.WriteString(login(t, LoginHandler(b), chosen, nil).Body.String())

	files := ""
	entries, _ := os.ReadDir(dir)
	for _, entry := range entries {
		raw, _ := os.ReadFile(filepath.Join(dir, entry.Name()))
		files += string(raw)
	}
	for what, text := range map[string]string{"log": logs.String(), "file": files, "answer": answers.String()} {
		for _, secret := range []string{canary, chosen} {
			if strings.Contains(text, secret) {
				t.Errorf("the %s holds a password", what)
			}
		}
	}
	if !strings.Contains(logs.String(), "dashboard password changed") {
		t.Errorf("the change was not logged: %q", logs.String())
	}
}

func slog_to(w *bytes.Buffer) *slog.Logger {
	return slog.New(slog.NewTextHandler(w, &slog.HandlerOptions{Level: slog.LevelDebug}))
}
