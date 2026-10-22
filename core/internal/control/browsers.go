package control

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"sync"
	"time"

	"connectrpc.com/connect"
	"google.golang.org/protobuf/types/known/timestamppb"

	controlv1 "github.com/KoukeNeko/JingClaw/core/gen/go/jingclaw/control/v1"
	"github.com/KoukeNeko/JingClaw/core/gen/go/jingclaw/control/v1/controlv1connect"
	"github.com/KoukeNeko/JingClaw/core/internal/fsperm"
)

const (
	// pairingTTL is how long a code lasts. Long enough to open a browser, short
	// enough that one left in a terminal's scrollback is already dead.
	pairingTTL = time.Minute

	// sessionTTL is how long a browser stays in. A day is a working day plus
	// the evening somebody comes back to it; past that, pairing again is one
	// command.
	sessionTTL = 12 * time.Hour

	// maxLive bounds what is held. Pairing is rare and by hand, so reaching
	// the limit on waiting codes means something is calling it in a loop. The
	// limit on sessions is not that: nothing signs a browser out, a closed tab
	// keeps its place for twelve hours, and so the oldest gives way.
	maxLive = 16

	maxPairBody = 1 << 10
)

// ErrNotPaired is a code that was never issued, is spent, or has run out.
// It does not say which: whoever is guessing learns nothing from the answer.
var ErrNotPaired = errors.New("control: that pairing code is not valid")

// Browsers admits browsers to the control surface.
//
// A browser gets a credential of its own, never the operator's. Both kinds of
// secret are held only as their hash: what is stored cannot be presented.
//
// Pairing codes are held in memory and gone when the daemon stops. Sessions
// are kept in dashboard-sessions.json (their hashes and expiry, never a token)
// so that a restart does not sign a browser out; with no directory they are
// memory only. The password's salted hash is in dashboard-auth.json; with no
// such file the password is the initial one (see EnvInitialPassword), and the
// only session that gives is a setup session, held in memory.
type Browsers struct {
	now    func() time.Time
	dir    string
	logger *slog.Logger

	// iterations is the cost of a new password hash; a test lowers it.
	iterations int

	// attempt holds password checks one at a time. Taken before mu.
	attempt sync.Mutex

	mu       sync.Mutex
	codes    map[[sha256.Size]byte]time.Time
	sessions map[[sha256.Size]byte]time.Time
	password *passwordRecord
	failed   throttle

	// setup holds the sessions the initial password gives. Memory only, and
	// never written anywhere: a restart ends them.
	setup map[[sha256.Size]byte]time.Time

	// initial is the password while none has been set; initialFrom says where
	// it came from, for the log. Fixed once made.
	initial, initialFrom string
}

// NewBrowsers keeps everything in memory.
func NewBrowsers(now func() time.Time) *Browsers {
	b := newBrowsers(now, slog.Default())
	b.logInitial()
	return b
}

func newBrowsers(now func() time.Time, logger *slog.Logger) *Browsers {
	b := &Browsers{
		now:        now,
		logger:     logger,
		iterations: pbkdf2Iterations,
		codes:      make(map[[sha256.Size]byte]time.Time),
		sessions:   make(map[[sha256.Size]byte]time.Time),
		setup:      make(map[[sha256.Size]byte]time.Time),
	}
	b.resolveInitial()
	return b
}

// OpenBrowsers keeps the password and the sessions in dir, and loads what is
// there. What cannot be read is said and left, and starts empty: a daemon that
// will not start over a sign-in is worse than one that asks for it again.
func OpenBrowsers(now func() time.Time, dir string, logger *slog.Logger) *Browsers {
	b := newBrowsers(now, logger)
	b.dir = dir

	record, err := loadPassword(filepath.Join(dir, PasswordFile))
	if err != nil {
		logger.Warn("the saved dashboard password cannot be used, so the password is the initial one", "error", err)
	}
	b.password = record

	b.loadSessions(filepath.Join(dir, SessionsFile))
	b.logInitial()
	return b
}

type sessionFile struct {
	Version  int `json:"version"`
	Sessions []struct {
		Hash    string    `json:"hash"`
		Expires time.Time `json:"expires_at"`
	} `json:"sessions"`
}

func (b *Browsers) loadSessions(path string) {
	raw, err := os.ReadFile(path)
	if os.IsNotExist(err) {
		return
	}
	var stored sessionFile
	if err == nil {
		if ownerOnly, _, statErr := fsperm.EnsureOwnerOnly(path); statErr != nil || !ownerOnly {
			err = errors.New("the file is readable by other accounts")
		} else {
			err = json.Unmarshal(raw, &stored)
		}
	}
	if err != nil {
		b.logger.Warn("the saved browser sessions cannot be used, so browsers sign in again", "error", err)
		return
	}

	now := b.now()
	for _, one := range stored.Sessions {
		decoded, err := hex.DecodeString(one.Hash)
		if err != nil || len(decoded) != sha256.Size || !now.Before(one.Expires) {
			continue
		}
		b.sessions[[sha256.Size]byte(decoded)] = one.Expires
	}
	for len(b.sessions) > maxLive {
		dropOldest(b.sessions)
	}
}

// saveSessionsLocked writes the live sessions. Called with mu held, after any
// change to them.
func (b *Browsers) saveSessionsLocked() error {
	if b.dir == "" {
		return nil
	}
	var stored sessionFile
	stored.Version = 1
	for key, expires := range b.sessions {
		stored.Sessions = append(stored.Sessions, struct {
			Hash    string    `json:"hash"`
			Expires time.Time `json:"expires_at"`
		}{hex.EncodeToString(key[:]), expires})
	}
	sort.Slice(stored.Sessions, func(i, j int) bool { return stored.Sessions[i].Hash < stored.Sessions[j].Hash })
	encoded, err := json.MarshalIndent(stored, "", "  ")
	if err != nil {
		return err
	}
	return writeOwnerOnly(filepath.Join(b.dir, SessionsFile), encoded)
}

// revokeExceptLocked signs out every session but the one presented as keep.
// Setup sessions are never kept.
func (b *Browsers) revokeExceptLocked(keep string) error {
	var kept time.Time
	var keptKey [sha256.Size]byte
	if keep != "" {
		keptKey = hash(keep)
		kept = b.sessions[keptKey]
	}
	clear(b.sessions)
	clear(b.setup)
	if !kept.IsZero() {
		b.sessions[keptKey] = kept
	}
	return b.saveSessionsLocked()
}

// Pair makes a code that one browser can trade for a session, once.
func (b *Browsers) Pair() (string, time.Time, error) {
	secret, err := newSecret()
	if err != nil {
		return "", time.Time{}, err
	}

	b.mu.Lock()
	defer b.mu.Unlock()
	b.expire()

	if len(b.codes) >= maxLive {
		return "", time.Time{}, errors.New("control: too many pairing codes are waiting to be used")
	}

	expires := b.now().Add(pairingTTL)
	b.codes[hash(secret)] = expires
	return secret, expires, nil
}

// Redeem trades a code for a session. The code is spent whether or not
// anything goes wrong afterwards: presenting one is using it.
func (b *Browsers) Redeem(code string) (Token, time.Time, error) {
	key := hash(code)

	b.mu.Lock()
	defer b.mu.Unlock()
	b.expire()

	if _, ok := b.codes[key]; !ok {
		return Token{}, time.Time{}, ErrNotPaired
	}
	delete(b.codes, key)

	return b.issueLocked()
}

// issueLocked makes a session. Called with mu held.
func (b *Browsers) issueLocked() (Token, time.Time, error) {
	if len(b.sessions) >= maxLive {
		dropOldest(b.sessions)
	}

	secret, err := newSecret()
	if err != nil {
		return Token{}, time.Time{}, err
	}

	expires := b.now().Add(sessionTTL)
	b.sessions[hash(secret)] = expires
	if err := b.saveSessionsLocked(); err != nil {
		// Held for now, and lost to a restart. Not worth refusing to let the
		// browser in over.
		b.logger.Warn("could not save the browser sessions", "error", err)
	}

	// The operator's own scope: this browser is going to be asked to approve
	// commands, and a narrower one would be a page that cannot do its job.
	return Token{Value: secret, Scope: ScopeControl}, expires, nil
}

// issueSetupLocked makes a setup session: held in memory, short, and good for
// nothing but changing the password. Called with mu held.
func (b *Browsers) issueSetupLocked() (Token, time.Time, error) {
	if len(b.setup) >= maxLive {
		dropOldest(b.setup)
	}
	secret, err := newSecret()
	if err != nil {
		return Token{}, time.Time{}, err
	}
	expires := b.now().Add(setupTTL)
	b.setup[hash(secret)] = expires
	return Token{Value: secret, Scope: ScopeSetup}, expires, nil
}

// Lookup says what a presented credential may reach, if it is a live session.
func (b *Browsers) Lookup(value string) (Scope, bool) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.expire()

	if _, ok := b.sessions[hash(value)]; ok {
		return ScopeControl, true
	}
	if _, ok := b.setup[hash(value)]; ok {
		return ScopeSetup, true
	}
	return "", false
}

// expire drops what has run out. Called under the lock, on every use, so the
// maps stay as small as the activity and there is nothing to schedule.
func (b *Browsers) expire() {
	now := b.now()
	for key, expires := range b.codes {
		if !now.Before(expires) {
			delete(b.codes, key)
		}
	}
	for _, held := range []map[[sha256.Size]byte]time.Time{b.sessions, b.setup} {
		for key, expires := range held {
			if !now.Before(expires) {
				delete(held, key)
			}
		}
	}
}

// dropOldest signs out the browser that has been in the longest. Every session
// lasts the same time, so that is the one that runs out first.
func dropOldest(held map[[sha256.Size]byte]time.Time) {
	var oldest [sha256.Size]byte
	var soonest time.Time
	for key, expires := range held {
		if soonest.IsZero() || expires.Before(soonest) {
			oldest, soonest = key, expires
		}
	}
	delete(held, oldest)
}

func newSecret() (string, error) {
	buf := make([]byte, tokenBytes)
	if _, err := rand.Read(buf); err != nil {
		return "", fmt.Errorf("control: generate a secret: %w", err)
	}
	return base64.RawURLEncoding.EncodeToString(buf), nil
}

func hash(secret string) [sha256.Size]byte { return sha256.Sum256([]byte(secret)) }

// PairHandler is where a browser trades its code for a session.
//
// Open to anyone who can reach the daemon, because the code is the credential:
// the one thing a browser has before it has a session. Only the host check
// stands in front of it, and the code is single-use, so there is nothing here
// to guess at and nothing to replay.
func PairHandler(browsers *Browsers) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Cache-Control", "no-store")

		if r.Method != http.MethodPost {
			w.Header().Set("Allow", http.MethodPost)
			http.Error(w, "use POST", http.StatusMethodNotAllowed)
			return
		}

		body, err := io.ReadAll(io.LimitReader(r.Body, maxPairBody))
		var given struct {
			Code string `json:"code"`
		}
		if err != nil || json.Unmarshal(body, &given) != nil || given.Code == "" {
			http.Error(w, "send {\"code\": \"...\"}", http.StatusBadRequest)
			return
		}

		token, expires, err := browsers.Redeem(given.Code)
		if err != nil {
			status := http.StatusUnauthorized
			if !errors.Is(err, ErrNotPaired) {
				status = http.StatusServiceUnavailable
			}
			http.Error(w, "pairing refused", status)
			return
		}

		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]string{
			"token":      token.Value,
			"expires_at": expires.UTC().Format(time.RFC3339),
		})
	})
}

// BrowserServer is how the operator asks for a pairing code.
type BrowserServer struct {
	browsers *Browsers
}

var _ controlv1connect.BrowserServiceHandler = (*BrowserServer)(nil)

func NewBrowserServer(browsers *Browsers) *BrowserServer {
	return &BrowserServer{browsers: browsers}
}

func (s *BrowserServer) CreatePairing(
	_ context.Context,
	_ *connect.Request[controlv1.CreatePairingRequest],
) (*connect.Response[controlv1.CreatePairingResponse], error) {
	code, expires, err := s.browsers.Pair()
	if err != nil {
		return nil, connect.NewError(connect.CodeResourceExhausted, err)
	}

	return connect.NewResponse(&controlv1.CreatePairingResponse{
		Code:      code,
		ExpiresAt: timestamppb.New(expires),
	}), nil
}

func (s *BrowserServer) SetDashboardPassword(
	_ context.Context,
	req *connect.Request[controlv1.SetDashboardPasswordRequest],
) (*connect.Response[controlv1.SetDashboardPasswordResponse], error) {
	token, expires, err := s.browsers.SetPassword(req.Msg.GetNewPassword(), req.Msg.GetCurrentPassword(), presented(req.Header()))
	if err != nil {
		return nil, passwordError(err)
	}
	reply := &controlv1.SetDashboardPasswordResponse{}
	if token.Value != "" {
		reply.Token, reply.ExpiresAt = token.Value, timestamppb.New(expires)
	}
	return connect.NewResponse(reply), nil
}

func (s *BrowserServer) ClearDashboardPassword(
	_ context.Context,
	req *connect.Request[controlv1.ClearDashboardPasswordRequest],
) (*connect.Response[controlv1.ClearDashboardPasswordResponse], error) {
	if err := s.browsers.ClearPassword(req.Msg.GetCurrentPassword()); err != nil {
		return nil, passwordError(err)
	}
	return connect.NewResponse(&controlv1.ClearDashboardPasswordResponse{}), nil
}

// presented is the credential a call came with, so that a change made from a
// browser does not sign that browser out.
func presented(header http.Header) string {
	token, _ := bearerToken(header.Get("Authorization"))
	return token
}

func passwordError(err error) error {
	var throttled *ThrottledError
	switch {
	case errors.As(err, &throttled):
		return connect.NewError(connect.CodeResourceExhausted, err)
	case errors.Is(err, ErrWrongPassword):
		return connect.NewError(connect.CodePermissionDenied, errors.New("the current password is missing or not right"))
	case errors.Is(err, ErrPasswordInitial):
		return connect.NewError(connect.CodeFailedPrecondition, err)
	case errors.Is(err, ErrPasswordPolicy):
		return connect.NewError(connect.CodeInvalidArgument, err)
	default:
		return connect.NewError(connect.CodeInternal, err)
	}
}
