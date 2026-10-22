package control

import (
	"crypto/pbkdf2"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"mime"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/KoukeNeko/JingClaw/core/internal/fsperm"
)

const (
	// PasswordFile and SessionsFile are the two files this keeps in the
	// deployment directory. The names are reserved in the config package, which
	// cannot import this one, so that no credential setting can point at them.
	PasswordFile = "dashboard-auth.json"
	SessionsFile = "dashboard-sessions.json"

	minPassword = 10
	maxPassword = 256

	// EnvInitialPassword names the environment variable that gives the initial
	// password, and DefaultInitialPassword is what it is when that is not set
	// (or not valid). It is only a starting value: once the password has been
	// changed, the environment is not looked at again.
	EnvInitialPassword     = "JINGCLAW_DASHBOARD_PASSWORD"
	DefaultInitialPassword = "jingclaw-initial"

	// setupTTL is how long a sign-in with the initial password lasts. It can do
	// one thing, change the password, so there is no reason for it to last.
	setupTTL = 15 * time.Minute

	// pbkdf2Iterations is OWASP's figure for PBKDF2-HMAC-SHA256. It is stored
	// with the hash, so raising it later does not lock anybody out.
	pbkdf2Iterations = 600_000
	derivedBytes     = 32
	saltBytes        = 16

	// maxLoginBody holds a 256-character password in the worst UTF-8 and JSON
	// escaping, with room to spare.
	maxLoginBody = 4 << 10

	// Failures before the delay starts, the window they have to fall in, how
	// long a quiet spell forgives them, and the longest delay.
	freeFailures = 5
	failureSpan  = time.Minute
	forgiveAfter = 10 * time.Minute
	longestDelay = 60 * time.Second
)

var (
	// ErrWrongPassword is the answer to a wrong password.
	ErrWrongPassword = errors.New("control: that password is not right")

	// ErrPasswordPolicy is a new password that is too short or too long.
	ErrPasswordPolicy = fmt.Errorf("control: a password is %d to %d characters", minPassword, maxPassword)

	// ErrPasswordInitial is a new password that is the initial one, which is
	// known to whoever can read the documentation.
	ErrPasswordInitial = errors.New("control: the new password must differ from the initial one")
)

// ThrottledError is an attempt refused without being looked at, because too
// many have failed. It comes with how long to wait.
type ThrottledError struct{ Wait time.Duration }

func (e *ThrottledError) Error() string {
	return fmt.Sprintf("control: too many failed attempts; try again in %s", e.Wait.Round(time.Second))
}

// passwordRecord is the whole of dashboard-auth.json: the salted hash and what
// it takes to check a password against it. Never the password.
type passwordRecord struct {
	Version    int    `json:"version"`
	Algorithm  string `json:"algorithm"`
	Iterations int    `json:"iterations"`
	Salt       string `json:"salt"`
	Hash       string `json:"hash"`
}

const algorithm = "pbkdf2-sha256"

func (r passwordRecord) matches(password string) (bool, error) {
	salt, err := base64.StdEncoding.DecodeString(r.Salt)
	if err != nil {
		return false, err
	}
	want, err := base64.StdEncoding.DecodeString(r.Hash)
	if err != nil {
		return false, err
	}
	got, err := pbkdf2.Key(sha256.New, password, salt, r.Iterations, len(want))
	if err != nil {
		return false, err
	}
	return subtle.ConstantTimeCompare(got, want) == 1, nil
}

func newPasswordRecord(password string, iterations int) (passwordRecord, error) {
	salt := make([]byte, saltBytes)
	if _, err := rand.Read(salt); err != nil {
		return passwordRecord{}, fmt.Errorf("control: generate a salt: %w", err)
	}
	derived, err := pbkdf2.Key(sha256.New, password, salt, iterations, derivedBytes)
	if err != nil {
		return passwordRecord{}, err
	}
	return passwordRecord{
		Version: 1, Algorithm: algorithm, Iterations: iterations,
		Salt: base64.StdEncoding.EncodeToString(salt), Hash: base64.StdEncoding.EncodeToString(derived),
	}, nil
}

// throttle slows down guessing. A loopback caller is always 127.0.0.1 or a
// tunnel, so there is no address to count by: the count is global.
type throttle struct {
	failures    int
	first, last time.Time
	until       time.Time
}

func (t *throttle) wait(now time.Time) time.Duration {
	if now.Before(t.until) {
		return t.until.Sub(now)
	}
	return 0
}

func (t *throttle) fail(now time.Time) {
	if t.failures > 0 &&
		(now.Sub(t.last) > forgiveAfter || (t.failures < freeFailures && now.Sub(t.first) > failureSpan)) {
		*t = throttle{}
	}
	if t.failures == 0 {
		t.first = now
	}
	t.failures++
	t.last = now
	if t.failures >= freeFailures {
		delay := longestDelay
		if step := t.failures - freeFailures; step < 6 {
			delay = time.Second << step
		}
		t.until = now.Add(min(delay, longestDelay))
	}
}

func validPassword(password string) bool {
	n := utf8.RuneCountInString(password)
	return n >= minPassword && n <= maxPassword
}

// checkPassword is whether given is the password, charged to the throttle. The
// caller holds b.attempt, so attempts are one at a time and a burst cannot get
// past the throttle before its failures are counted. While the password is the
// initial one, that is what given is compared with, and it is throttled like
// any other.
func (b *Browsers) checkPassword(given string) error {
	b.mu.Lock()
	if wait := b.failed.wait(b.now()); wait > 0 {
		b.mu.Unlock()
		return &ThrottledError{Wait: wait}
	}
	record := b.password
	b.mu.Unlock()

	var ok bool
	if record == nil {
		// Both sides hashed, so the comparison is over equal lengths.
		got, want := sha256.Sum256([]byte(given)), sha256.Sum256([]byte(b.initial))
		ok = subtle.ConstantTimeCompare(got[:], want[:]) == 1
	} else {
		matched, err := record.matches(given)
		ok = matched && err == nil && utf8.RuneCountInString(given) <= maxPassword
	}

	b.mu.Lock()
	defer b.mu.Unlock()
	if !ok {
		b.failed.fail(b.now())
		return ErrWrongPassword
	}
	b.failed = throttle{}
	return nil
}

// Initial is whether the password is still the initial one, so that a change
// is required.
func (b *Browsers) Initial() bool {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.password == nil
}

// resolveInitial picks the initial password: the environment's when it is a
// valid password, otherwise the built-in one. The value is never logged.
func (b *Browsers) resolveInitial() {
	b.initial, b.initialFrom = DefaultInitialPassword, "built-in default"
	if given, set := os.LookupEnv(EnvInitialPassword); set {
		if validPassword(given) {
			b.initial, b.initialFrom = given, "from environment"
		} else {
			b.logger.Warn(EnvInitialPassword+" is not 10 to 256 characters, so the built-in initial password is used",
				"variable", EnvInitialPassword)
		}
	}
}

func (b *Browsers) logInitial() {
	if b.password == nil {
		b.logger.Info("dashboard password: initial (change it at first login)", "source", b.initialFrom)
	}
}

// Login trades the password for a session. The initial password gets a setup
// session, which can only change the password; any other gets the same session
// a pairing gives.
func (b *Browsers) Login(password string) (Token, time.Time, error) {
	b.attempt.Lock()
	defer b.attempt.Unlock()

	if err := b.checkPassword(password); err != nil {
		return Token{}, time.Time{}, err
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	b.expire()
	if b.password == nil {
		return b.issueSetupLocked()
	}
	return b.issueLocked()
}

// SetPassword replaces the password. presented is the credential the call came
// with: every session but that one is signed out. When it is a setup session,
// that one goes too, and the token returned is a control session to continue
// with (it is empty otherwise).
func (b *Browsers) SetPassword(newPassword, current, presented string) (Token, time.Time, error) {
	if !validPassword(newPassword) {
		return Token{}, time.Time{}, ErrPasswordPolicy
	}
	if newPassword == b.initial {
		return Token{}, time.Time{}, ErrPasswordInitial
	}
	b.attempt.Lock()
	defer b.attempt.Unlock()

	if err := b.requireCurrent(current); err != nil {
		return Token{}, time.Time{}, err
	}
	record, err := newPasswordRecord(newPassword, b.iterations)
	if err != nil {
		return Token{}, time.Time{}, err
	}
	encoded, err := json.MarshalIndent(record, "", "  ")
	if err != nil {
		return Token{}, time.Time{}, err
	}
	if b.dir != "" {
		if err := writeOwnerOnly(filepath.Join(b.dir, PasswordFile), encoded); err != nil {
			return Token{}, time.Time{}, err
		}
	}

	b.mu.Lock()
	defer b.mu.Unlock()
	b.expire()
	_, fromSetup := b.setup[hash(presented)]
	b.password = &record
	clear(b.setup)
	b.logger.Info("dashboard password changed")
	if !fromSetup {
		return Token{}, time.Time{}, b.revokeExceptLocked(presented)
	}
	if err := b.revokeExceptLocked(""); err != nil {
		return Token{}, time.Time{}, err
	}
	return b.issueLocked()
}

// requireCurrent is nothing while the password is the initial one (whoever can
// ask is already let in), and otherwise a check of the one given. None given is
// refused without being counted: it guesses nothing, and a client finds out it
// needs to ask by trying.
func (b *Browsers) requireCurrent(current string) error {
	if b.Initial() {
		return nil
	}
	if current == "" {
		return ErrWrongPassword
	}
	return b.checkPassword(current)
}

// ClearPassword puts the password back to the initial one and signs out every
// session.
func (b *Browsers) ClearPassword(current string) error {
	b.attempt.Lock()
	defer b.attempt.Unlock()

	if err := b.requireCurrent(current); err != nil {
		return err
	}
	if b.dir != "" {
		if err := os.Remove(filepath.Join(b.dir, PasswordFile)); err != nil && !os.IsNotExist(err) {
			return fmt.Errorf("control: remove the password: %w", err)
		}
	}

	b.mu.Lock()
	defer b.mu.Unlock()
	b.password = nil
	b.logger.Info("dashboard password reset to the initial one")
	clear(b.setup)
	return b.revokeExceptLocked("")
}

// loadPassword reads the record, if there is a usable one. A file other
// accounts can read, or one that is not a record, is left alone and reads as no
// password: not letting anybody in is the safe way to be wrong.
func loadPassword(path string) (*passwordRecord, error) {
	if _, err := os.Stat(path); os.IsNotExist(err) {
		return nil, nil
	}
	ownerOnly, exposure, err := fsperm.EnsureOwnerOnly(path)
	if err != nil {
		return nil, err
	}
	if !ownerOnly {
		return nil, fmt.Errorf("%s %s; it must be readable only by its owner (chmod 600)", path, exposure)
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var record passwordRecord
	if json.Unmarshal(raw, &record) != nil || record.Algorithm != algorithm || record.Iterations < 1 ||
		record.Salt == "" || record.Hash == "" {
		return nil, fmt.Errorf("%s is not a password record this can read", path)
	}
	return &record, nil
}

// writeOwnerOnly replaces the file whole: written to a temp file beside it,
// restricted before any byte goes in, flushed, and renamed. A reader sees the
// old file or the new one, never half of either.
func writeOwnerOnly(path string, data []byte) error {
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return fmt.Errorf("control: %w", err)
	}
	temp, err := os.CreateTemp(dir, ".dashboard-*")
	if err != nil {
		return fmt.Errorf("control: %w", err)
	}
	defer os.Remove(temp.Name())

	if err := fsperm.Restrict(temp.Name()); err != nil {
		temp.Close()
		return fmt.Errorf("control: %w", err)
	}
	if _, err := temp.Write(data); err != nil {
		temp.Close()
		return fmt.Errorf("control: %w", err)
	}
	if err := temp.Sync(); err != nil {
		temp.Close()
		return fmt.Errorf("control: %w", err)
	}
	if err := temp.Close(); err != nil {
		return fmt.Errorf("control: %w", err)
	}
	if err := os.Rename(temp.Name(), path); err != nil {
		return fmt.Errorf("control: %w", err)
	}
	return nil
}

// AuthHandler says whether the password is still the initial one, and nothing
// else. passwordSet is kept for the page; there is always a password now.
func AuthHandler(browsers *Browsers) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Cache-Control", "no-store")
		if r.Method != http.MethodGet {
			w.Header().Set("Allow", http.MethodGet)
			http.Error(w, "use GET", http.StatusMethodNotAllowed)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]bool{"passwordSet": true, "initial": browsers.Initial()})
	})
}

// LoginHandler is where a browser trades the password for a session.
//
// Open to anyone who can reach the daemon, behind the host check, because the
// password is the credential. What stands in for the rest: a post from another
// origin is refused, so a page elsewhere cannot make the browser guess, the
// body must be JSON and is bounded, and failures are throttled for everybody.
func LoginHandler(browsers *Browsers) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Cache-Control", "no-store")

		if r.Method != http.MethodPost {
			w.Header().Set("Allow", http.MethodPost)
			http.Error(w, "use POST", http.StatusMethodNotAllowed)
			return
		}
		if !sameOrigin(r) {
			http.Error(w, "cross-origin request refused", http.StatusForbidden)
			return
		}
		if kind, _, err := mime.ParseMediaType(r.Header.Get("Content-Type")); err != nil || kind != "application/json" {
			http.Error(w, "send application/json", http.StatusUnsupportedMediaType)
			return
		}

		var given struct {
			Password string `json:"password"`
		}
		if json.NewDecoder(http.MaxBytesReader(w, r.Body, maxLoginBody)).Decode(&given) != nil {
			http.Error(w, "send {\"password\": \"...\"}", http.StatusBadRequest)
			return
		}

		token, expires, err := browsers.Login(given.Password)
		var throttled *ThrottledError
		switch {
		case errors.As(err, &throttled):
			w.Header().Set("Retry-After", strconv.Itoa(int((throttled.Wait+time.Second-1)/time.Second)))
			http.Error(w, "too many attempts", http.StatusTooManyRequests)
			return
		case errors.Is(err, ErrWrongPassword):
			http.Error(w, "login refused", http.StatusUnauthorized)
			return
		case err != nil:
			http.Error(w, "login refused", http.StatusServiceUnavailable)
			return
		}

		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]string{
			"token":      token.Value,
			"expires_at": expires.UTC().Format(time.RFC3339),
			"scope":      string(token.Scope),
		})
	})
}

// sameOrigin is whether a post came from this page. A request with no Origin
// is not from a browser's cross-site machinery (curl, the CLI) and is let by.
func sameOrigin(r *http.Request) bool {
	origin := r.Header.Get("Origin")
	if origin == "" {
		return true
	}
	parsed, err := url.Parse(origin)
	return err == nil && parsed.Host != "" && strings.EqualFold(parsed.Host, r.Host)
}
