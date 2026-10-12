package control

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"sync"
	"time"

	"connectrpc.com/connect"
	"google.golang.org/protobuf/types/known/timestamppb"

	controlv1 "github.com/KoukeNeko/JingClaw/core/gen/go/jingclaw/control/v1"
	"github.com/KoukeNeko/JingClaw/core/gen/go/jingclaw/control/v1/controlv1connect"
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
	// either limit means something is calling it in a loop.
	maxLive = 16

	maxPairBody = 1 << 10
)

// ErrNotPaired is a code that was never issued, is spent, or has run out.
// It does not say which: whoever is guessing learns nothing from the answer.
var ErrNotPaired = errors.New("control: that pairing code is not valid")

// Browsers admits browsers to the control surface.
//
// A browser gets a credential of its own, never the operator's. Held in
// memory and gone when the daemon stops, so a restart signs every browser out
// and nothing sits on disk waiting to be read. Both kinds of secret are held
// only as their hash: what is stored cannot be presented.
type Browsers struct {
	now func() time.Time

	mu       sync.Mutex
	codes    map[[sha256.Size]byte]time.Time
	sessions map[[sha256.Size]byte]time.Time
}

func NewBrowsers(now func() time.Time) *Browsers {
	return &Browsers{
		now:      now,
		codes:    make(map[[sha256.Size]byte]time.Time),
		sessions: make(map[[sha256.Size]byte]time.Time),
	}
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

	if len(b.sessions) >= maxLive {
		return Token{}, time.Time{}, errors.New("control: too many browsers are signed in")
	}

	secret, err := newSecret()
	if err != nil {
		return Token{}, time.Time{}, err
	}

	expires := b.now().Add(sessionTTL)
	b.sessions[hash(secret)] = expires

	// The operator's own scope: this browser is going to be asked to approve
	// commands, and a narrower one would be a page that cannot do its job.
	return Token{Value: secret, Scope: ScopeControl}, expires, nil
}

// Lookup says what a presented credential may reach, if it is a live session.
func (b *Browsers) Lookup(value string) (Scope, bool) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.expire()

	if _, ok := b.sessions[hash(value)]; ok {
		return ScopeControl, true
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
	for key, expires := range b.sessions {
		if !now.Before(expires) {
			delete(b.sessions, key)
		}
	}
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
