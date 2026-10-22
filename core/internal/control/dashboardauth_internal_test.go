package control

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"connectrpc.com/connect"

	controlv1 "github.com/KoukeNeko/JingClaw/core/gen/go/jingclaw/control/v1"
)

const goodPassword = "correct horse battery"

type testClock struct{ at time.Time }

func (c *testClock) now() time.Time { return c.at }

// open is Browsers kept in dir, with a hash cheap enough to make in a test.
func open(t *testing.T, dir string, clock *testClock, logs *bytes.Buffer) *Browsers {
	t.Helper()
	logger := slog.New(slog.DiscardHandler)
	if logs != nil {
		logger = slog.New(slog.NewTextHandler(logs, &slog.HandlerOptions{Level: slog.LevelDebug}))
	}
	b := OpenBrowsers(clock.now, dir, logger)
	b.iterations = 1000
	return b
}

func newClock() *testClock { return &testClock{at: time.Unix(1_700_000_000, 0)} }

func login(t *testing.T, handler http.Handler, password string, mutate func(*http.Request)) *httptest.ResponseRecorder {
	t.Helper()
	body, _ := json.Marshal(map[string]string{"password": password})
	req := httptest.NewRequest(http.MethodPost, "http://127.0.0.1:1234/ui/login", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	if mutate != nil {
		mutate(req)
	}
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	return rec
}

func TestThePasswordIsNeverWrittenOrLoggedOnlyItsSaltedHash(t *testing.T) {
	dir := t.TempDir()
	var logs bytes.Buffer
	b := open(t, dir, newClock(), &logs)
	canary := "canary-password-7f3a9c"

	if _, _, err := b.SetPassword(canary, "", ""); err != nil {
		t.Fatal(err)
	}
	rec := login(t, LoginHandler(b), canary, nil)
	wrong := login(t, LoginHandler(b), canary+"x", nil)

	for _, name := range []string{PasswordFile, SessionsFile} {
		raw, err := os.ReadFile(filepath.Join(dir, name))
		if err != nil {
			t.Fatal(err)
		}
		if strings.Contains(string(raw), canary) {
			t.Errorf("%s holds the password", name)
		}
	}
	raw, _ := os.ReadFile(filepath.Join(dir, PasswordFile))
	var record passwordRecord
	if err := json.Unmarshal(raw, &record); err != nil || record.Salt == "" || record.Hash == "" || record.Iterations != 1000 {
		t.Errorf("the record is %+v (%v), want a salt, a hash and the parameters", record, err)
	}
	for what, text := range map[string]string{"log": logs.String(), "login": rec.Body.String(), "refusal": wrong.Body.String()} {
		if strings.Contains(text, canary) {
			t.Errorf("the %s holds the password", what)
		}
	}
	if rec.Code != http.StatusOK {
		t.Errorf("the right password got %d", rec.Code)
	}
}

func TestTwoPasswordsTheSameAreHashedDifferently(t *testing.T) {
	first := open(t, t.TempDir(), newClock(), nil)
	second := open(t, t.TempDir(), newClock(), nil)
	_, _, _ = first.SetPassword(goodPassword, "", "")
	_, _, _ = second.SetPassword(goodPassword, "", "")
	if first.password.Hash == second.password.Hash || first.password.Salt == second.password.Salt {
		t.Error("the same password gave the same salt or hash")
	}
}

func TestOnlyTheRightPasswordLogsIn(t *testing.T) {
	b := open(t, t.TempDir(), newClock(), nil)
	if _, _, err := b.SetPassword(goodPassword, "", ""); err != nil {
		t.Fatal(err)
	}
	handler := LoginHandler(b)

	if rec := login(t, handler, goodPassword+"!", nil); rec.Code != http.StatusUnauthorized {
		t.Errorf("a wrong password got %d", rec.Code)
	}
	rec := login(t, handler, goodPassword, nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("the right password got %d", rec.Code)
	}
	var given map[string]string
	if err := json.Unmarshal(rec.Body.Bytes(), &given); err != nil || given["token"] == "" || given["expires_at"] == "" {
		t.Fatalf("the reply is %q, want the shape of a pairing", rec.Body.String())
	}
	if scope, ok := b.Lookup(given["token"]); !ok || scope != ScopeControl {
		t.Errorf("the session has scope %q, %v; want the control scope", scope, ok)
	}
}

func TestFailuresAreSlowedDownForEverybodyAndASuccessEndsIt(t *testing.T) {
	clock := newClock()
	b := open(t, t.TempDir(), clock, nil)
	_, _, _ = b.SetPassword(goodPassword, "", "")
	handler := LoginHandler(b)

	for i := 0; i < 5; i++ {
		if rec := login(t, handler, "wrong", nil); rec.Code != http.StatusUnauthorized {
			t.Fatalf("attempt %d got %d", i+1, rec.Code)
		}
	}
	// Refused unlooked at, even for the right password, with how long to wait.
	rec := login(t, handler, goodPassword, nil)
	if rec.Code != http.StatusTooManyRequests || rec.Header().Get("Retry-After") != "1" {
		t.Fatalf("after five failures: %d, Retry-After %q", rec.Code, rec.Header().Get("Retry-After"))
	}

	// The wait doubles with each failure and stops at a minute.
	for _, wait := range []string{"2", "4", "8", "16", "32", "60", "60"} {
		clock.at = clock.at.Add(b.failed.wait(clock.at))
		if rec := login(t, handler, "wrong", nil); rec.Code != http.StatusUnauthorized {
			t.Fatalf("a failure after waiting got %d", rec.Code)
		}
		if got := login(t, handler, goodPassword, nil).Header().Get("Retry-After"); got != wait {
			t.Errorf("the wait is %ss, want %ss", got, wait)
		}
	}

	clock.at = clock.at.Add(b.failed.wait(clock.at))
	if rec := login(t, handler, goodPassword, nil); rec.Code != http.StatusOK {
		t.Fatalf("the right password after waiting got %d", rec.Code)
	}
	for i := 0; i < 5; i++ {
		if rec := login(t, handler, "wrong", nil); rec.Code != http.StatusUnauthorized {
			t.Fatalf("after a success, attempt %d got %d, want the count to start over", i+1, rec.Code)
		}
	}
}

func TestFailuresSpreadOverMoreThanAMinuteDoNotAddUp(t *testing.T) {
	clock := newClock()
	b := open(t, t.TempDir(), clock, nil)
	_, _, _ = b.SetPassword(goodPassword, "", "")
	handler := LoginHandler(b)

	for i := 0; i < 20; i++ {
		if rec := login(t, handler, "wrong", nil); rec.Code != http.StatusUnauthorized {
			t.Fatalf("attempt %d got %d", i+1, rec.Code)
		}
		clock.at = clock.at.Add(20 * time.Second)
		if i%4 == 3 {
			clock.at = clock.at.Add(time.Minute)
		}
	}
}

func TestALoginFromAnotherOriginOrNotAsJSONIsRefused(t *testing.T) {
	b := open(t, t.TempDir(), newClock(), nil)
	_, _, _ = b.SetPassword(goodPassword, "", "")
	handler := LoginHandler(b)

	cases := map[string]struct {
		mutate func(*http.Request)
		want   int
	}{
		"another origin":      {func(r *http.Request) { r.Header.Set("Origin", "https://evil.example") }, http.StatusForbidden},
		"the null origin":     {func(r *http.Request) { r.Header.Set("Origin", "null") }, http.StatusForbidden},
		"another port":        {func(r *http.Request) { r.Header.Set("Origin", "http://127.0.0.1:9999") }, http.StatusForbidden},
		"form encoding":       {func(r *http.Request) { r.Header.Set("Content-Type", "application/x-www-form-urlencoded") }, http.StatusUnsupportedMediaType},
		"plain text":          {func(r *http.Request) { r.Header.Set("Content-Type", "text/plain") }, http.StatusUnsupportedMediaType},
		"no content type":     {func(r *http.Request) { r.Header.Del("Content-Type") }, http.StatusUnsupportedMediaType},
		"the same origin":     {func(r *http.Request) { r.Header.Set("Origin", "http://127.0.0.1:1234") }, http.StatusOK},
		"JSON with a charset": {func(r *http.Request) { r.Header.Set("Content-Type", "application/json; charset=utf-8") }, http.StatusOK},
	}
	for name, one := range cases {
		if got := login(t, handler, goodPassword, one.mutate).Code; got != one.want {
			t.Errorf("%s: got %d, want %d", name, got, one.want)
		}
	}

	// A refusal on those grounds is not a guess at the password.
	if b.failed.failures != 0 {
		t.Errorf("%d failures were counted for requests that never tried a password", b.failed.failures)
	}

	big := httptest.NewRequest(http.MethodPost, "http://127.0.0.1:1234/ui/login",
		strings.NewReader(`{"password":"`+strings.Repeat("a", 1<<20)+`"}`))
	big.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, big)
	if rec.Code != http.StatusBadRequest {
		t.Errorf("a megabyte of password got %d", rec.Code)
	}
}

func TestAPasswordHasToBeTenToTwoHundredFiftySixCharacters(t *testing.T) {
	for _, bad := range []string{"", "123456789", strings.Repeat("a", 257), strings.Repeat("字", 257)} {
		if _, _, err := open(t, t.TempDir(), newClock(), nil).SetPassword(bad, "", ""); !errors.Is(err, ErrPasswordPolicy) {
			t.Errorf("a password of %d bytes: %v", len(bad), err)
		}
	}
	for _, good := range []string{"1234567890", strings.Repeat("a", 256), strings.Repeat("字", 10)} {
		if _, _, err := open(t, t.TempDir(), newClock(), nil).SetPassword(good, "", ""); err != nil {
			t.Errorf("%q: %v", good, err)
		}
	}
}

func TestChangingOrResettingThePasswordNeedsTheCurrentOneAndSignsOutTheOthers(t *testing.T) {
	dir := t.TempDir()
	clock := newClock()
	b := open(t, dir, clock, nil)
	_, _, _ = b.SetPassword(goodPassword, "", "")

	mine, _, _ := b.Login(goodPassword)
	other, _, _ := b.Login(goodPassword)

	if _, _, err := b.SetPassword("another good password", "", mine.Value); !errors.Is(err, ErrWrongPassword) {
		t.Fatalf("changing without the current one: %v", err)
	}
	if _, _, err := b.SetPassword("another good password", "not it", mine.Value); !errors.Is(err, ErrWrongPassword) {
		t.Fatalf("changing with a wrong current one: %v", err)
	}
	if _, ok := b.Lookup(other.Value); !ok {
		t.Fatal("a refused change signed somebody out")
	}

	if _, _, err := b.SetPassword("another good password", goodPassword, mine.Value); err != nil {
		t.Fatal(err)
	}
	if _, ok := b.Lookup(other.Value); ok {
		t.Error("another browser is still in after the password changed")
	}
	if _, ok := b.Lookup(mine.Value); !ok {
		t.Error("the browser that changed the password was signed out")
	}
	if _, _, err := b.Login(goodPassword); err == nil {
		t.Error("the old password still works")
	}
	if _, _, err := b.Login("another good password"); err != nil {
		t.Errorf("the new password: %v", err)
	}

	again, _, _ := b.Login("another good password")
	if err := b.ClearPassword("not it"); !errors.Is(err, ErrWrongPassword) {
		t.Fatalf("resetting with a wrong password: %v", err)
	}
	if err := b.ClearPassword("another good password"); err != nil {
		t.Fatal(err)
	}
	for who, token := range map[string]Token{"another browser": again, "the browser that asked": mine} {
		if _, ok := b.Lookup(token.Value); ok {
			t.Errorf("%s is still in after the password was reset", who)
		}
	}
	if !b.Initial() {
		t.Error("the password is not the initial one after a reset")
	}
	if _, err := os.Stat(filepath.Join(dir, PasswordFile)); !os.IsNotExist(err) {
		t.Errorf("the file is still there: %v", err)
	}

	// And what was revoked stays revoked after a restart.
	restarted := open(t, dir, clock, nil)
	if _, ok := restarted.Lookup(again.Value); ok {
		t.Error("a revoked session came back after a restart")
	}
	if _, ok := restarted.Lookup(mine.Value); ok {
		t.Error("a session revoked by the reset came back after a restart")
	}
	if !restarted.Initial() {
		t.Error("the restart does not start from the initial password")
	}
}

func TestTheFilesAreOwnerOnlyAndLeaveNoTemporaryFilesBehind(t *testing.T) {
	dir := t.TempDir()
	b := open(t, dir, newClock(), nil)
	_, _, _ = b.SetPassword(goodPassword, "", "")
	_, _, _ = b.Login(goodPassword)

	entries, _ := os.ReadDir(dir)
	var names []string
	for _, entry := range entries {
		names = append(names, entry.Name())
		if runtime.GOOS == "windows" {
			continue
		}
		if info, _ := entry.Info(); info.Mode().Perm() != 0o600 {
			t.Errorf("%s is mode %#o, want 0600", entry.Name(), info.Mode().Perm())
		}
	}
	if strings.Join(names, ",") != PasswordFile+","+SessionsFile {
		t.Errorf("the directory holds %v, want only the two files", names)
	}
}

func TestAFailedWriteLeavesThePasswordAsItWas(t *testing.T) {
	if runtime.GOOS == "windows" || os.Geteuid() == 0 {
		t.Skip("a directory cannot be made unwritable here")
	}
	dir := t.TempDir()
	b := open(t, dir, newClock(), nil)
	_, _, _ = b.SetPassword(goodPassword, "", "")
	before, _ := os.ReadFile(filepath.Join(dir, PasswordFile))

	if err := os.Chmod(dir, 0o500); err != nil {
		t.Fatal(err)
	}
	defer func() { _ = os.Chmod(dir, 0o700) }()

	if _, _, err := b.SetPassword("another good password", goodPassword, ""); err == nil {
		t.Fatal("a write that could not happen was reported as done")
	}
	after, _ := os.ReadFile(filepath.Join(dir, PasswordFile))
	if !bytes.Equal(before, after) {
		t.Error("the file changed")
	}
	if _, _, err := b.Login(goodPassword); err != nil {
		t.Errorf("the old password no longer works: %v", err)
	}
}

func TestAPasswordFileOtherAccountsCanReadIsNotUsed(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("modes are not how Windows says it")
	}
	dir := t.TempDir()
	first := open(t, dir, newClock(), nil)
	_, _, _ = first.SetPassword(goodPassword, "", "")
	if err := os.Chmod(filepath.Join(dir, PasswordFile), 0o644); err != nil {
		t.Fatal(err)
	}

	var logs bytes.Buffer
	second := open(t, dir, newClock(), &logs)
	if !second.Initial() {
		t.Error("a password from a file anybody could read was used")
	}
	if !strings.Contains(logs.String(), "chmod 600") {
		t.Errorf("nothing said why: %q", logs.String())
	}
}

func TestSessionsSurviveARestartAndExpiredOnesAreDropped(t *testing.T) {
	dir := t.TempDir()
	clock := newClock()
	b := open(t, dir, clock, nil)
	_, _, _ = b.SetPassword(goodPassword, "", "")

	old, _, _ := b.Login(goodPassword)
	clock.at = clock.at.Add(sessionTTL - time.Hour)
	fresh, _, _ := b.Login(goodPassword)
	code, _, _ := b.Pair()

	raw, _ := os.ReadFile(filepath.Join(dir, SessionsFile))
	if strings.Contains(string(raw), old.Value) || strings.Contains(string(raw), fresh.Value) || strings.Contains(string(raw), code) {
		t.Fatal("a token or a code is on disk")
	}

	clock.at = clock.at.Add(2 * time.Hour) // the first has run out, the second has not
	restarted := open(t, dir, clock, nil)
	if _, ok := restarted.Lookup(old.Value); ok {
		t.Error("a session that ran out came back")
	}
	if scope, ok := restarted.Lookup(fresh.Value); !ok || scope != ScopeControl {
		t.Error("a live session did not survive the restart")
	}
	if _, _, err := restarted.Redeem(code); !errors.Is(err, ErrNotPaired) {
		t.Errorf("a pairing code survived a restart: %v", err)
	}
	if restarted.Initial() {
		t.Error("the password did not survive the restart")
	}
}

func TestTheSessionCapHoldsAcrossARestartAndTheOldestGivesWay(t *testing.T) {
	dir := t.TempDir()
	clock := newClock()
	b := open(t, dir, clock, nil)
	_, _, _ = b.SetPassword(goodPassword, "", "")

	var tokens []Token
	for i := 0; i < maxLive+3; i++ {
		clock.at = clock.at.Add(time.Second)
		token, _, err := b.Login(goodPassword)
		if err != nil {
			t.Fatal(err)
		}
		tokens = append(tokens, token)
	}

	restarted := open(t, dir, clock, nil)
	for i, token := range tokens {
		_, ok := restarted.Lookup(token.Value)
		if want := i >= 3; ok != want {
			t.Errorf("session %d is in: %v, want %v", i, ok, want)
		}
	}
}

func TestASessionsFileThatIsNotOneStartsEmptyAndSaysSo(t *testing.T) {
	dir := t.TempDir()
	if err := writeOwnerOnly(filepath.Join(dir, SessionsFile), []byte("not json")); err != nil {
		t.Fatal(err)
	}
	var logs bytes.Buffer
	b := open(t, dir, newClock(), &logs)
	if len(b.sessions) != 0 || !strings.Contains(logs.String(), "browsers sign in again") {
		t.Errorf("sessions %d, log %q", len(b.sessions), logs.String())
	}
}

func TestThePasswordCanBeSetAndClearedThroughTheService(t *testing.T) {
	b := open(t, t.TempDir(), newClock(), nil)
	server := NewBrowserServer(b)

	set := func(newPassword, current, bearer string) error {
		req := connect.NewRequest(&controlv1.SetDashboardPasswordRequest{NewPassword: newPassword, CurrentPassword: current})
		if bearer != "" {
			req.Header().Set("Authorization", "Bearer "+bearer)
		}
		_, err := server.SetDashboardPassword(context.Background(), req)
		return err
	}

	if code := connect.CodeOf(set("short", "", "")); code != connect.CodeInvalidArgument {
		t.Errorf("a short password: %v", code)
	}
	if err := set(goodPassword, "", ""); err != nil {
		t.Fatal(err)
	}
	if code := connect.CodeOf(set("another good password", "wrong", "")); code != connect.CodePermissionDenied {
		t.Errorf("a wrong current password: %v", code)
	}

	for i := 0; i < 5; i++ {
		_ = set("another good password", "wrong", "")
	}
	if code := connect.CodeOf(set("another good password", goodPassword, "")); code != connect.CodeResourceExhausted {
		t.Errorf("after failures: %v, want the throttle to apply here too", code)
	}

	_, err := server.ClearDashboardPassword(context.Background(),
		connect.NewRequest(&controlv1.ClearDashboardPasswordRequest{CurrentPassword: "wrong"}))
	if connect.CodeOf(err) != connect.CodeResourceExhausted {
		t.Errorf("clearing during the throttle: %v", connect.CodeOf(err))
	}
}
