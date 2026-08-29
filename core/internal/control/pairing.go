package control

import (
	"crypto/rand"
	"crypto/subtle"
	"encoding/base32"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"sync"
	"time"
)

// RedeemPath is where a browser exchanges a code for its credential.
//
// It is a plain endpoint rather than an RPC because it is the one request a
// browser makes before it has anything to authenticate with, and the control
// protocol should not have to contain a hole for it.
const RedeemPath = "/pair"

const (
	// codeBytes is 80 bits, which is far more than a code that lives for
	// minutes and works once needs, and still short enough to type.
	codeBytes = 10

	// outstandingLimit stops repeated minting growing without bound. Nobody
	// needs sixteen unredeemed codes; the oldest going first is the one least
	// likely to still be wanted.
	outstandingLimit = 16
)

var (
	// ErrNoSuchCode covers a code that was never issued, has been used, or has
	// expired. They are one answer on purpose: telling somebody which of the
	// three it was tells them whether they are close.
	ErrNoSuchCode = errors.New("control: that code is not valid")

	codeEncoding = base32.StdEncoding.WithPadding(base32.NoPadding)
)

// Pairing hands a browser a credential in exchange for a code.
//
// The code is what travels — in a terminal's scrollback, in a screenshot, over
// somebody's shoulder — and it works once and expires. The credential it buys
// is the thing worth having, and it never appears anywhere a person can read
// it by accident.
type Pairing struct {
	// grants mints and records a credential per pairing.
	//
	// Per pairing rather than one shared credential, which is what this used
	// to hand out. Shared, a page paired last week still worked, there was no
	// way to see what had been let in, and revoking one browser meant
	// revoking all of them.
	grants *Grants

	ttl time.Duration
	now func() time.Time

	mu          sync.Mutex
	outstanding map[string]time.Time
}

func NewPairing(grants *Grants, ttl time.Duration, now func() time.Time) *Pairing {
	return &Pairing{
		grants:      grants,
		ttl:         ttl,
		now:         now,
		outstanding: make(map[string]time.Time),
	}
}

// Issue mints a code, in the shape a person can read off a screen.
func (p *Pairing) Issue() (code string, expires time.Time, err error) {
	raw := make([]byte, codeBytes)
	if _, err := rand.Read(raw); err != nil {
		return "", time.Time{}, fmt.Errorf("control: generate a pairing code: %w", err)
	}

	code = group(codeEncoding.EncodeToString(raw))
	expires = p.now().Add(p.ttl)

	p.mu.Lock()
	defer p.mu.Unlock()

	p.pruneLocked()
	for len(p.outstanding) >= outstandingLimit {
		p.dropOldestLocked()
	}
	p.outstanding[code] = expires

	return code, expires, nil
}

// Redeem exchanges a code for the console's credential, once.
func (p *Pairing) Redeem(offered, label string) (Token, error) {
	normalised := normalise(offered)

	p.mu.Lock()
	defer p.mu.Unlock()

	p.pruneLocked()

	// Compared against every outstanding code rather than looked up, so the
	// time taken does not say whether a prefix was right.
	matched := ""
	for code := range p.outstanding {
		if subtle.ConstantTimeCompare([]byte(normalise(code)), []byte(normalised)) == 1 {
			matched = code
		}
	}
	if matched == "" {
		return Token{}, ErrNoSuchCode
	}

	// Once. A code that still works after it has been used is a code that is
	// still worth stealing out of a screenshot an hour later.
	delete(p.outstanding, matched)

	token, _, err := p.grants.Issue(label)
	if err != nil {
		return Token{}, err
	}
	return token, nil
}

func (p *Pairing) pruneLocked() {
	now := p.now()
	for code, expires := range p.outstanding {
		if !expires.After(now) {
			delete(p.outstanding, code)
		}
	}
}

func (p *Pairing) dropOldestLocked() {
	oldest := ""
	var at time.Time

	for code, expires := range p.outstanding {
		if oldest == "" || expires.Before(at) {
			oldest, at = code, expires
		}
	}
	if oldest != "" {
		delete(p.outstanding, oldest)
	}
}

// RedeemHandler serves the exchange.
//
// It answers without a credential, which is the whole point, and it is the
// only thing on this daemon that does anything on that basis. What protects it
// is that a code works once, expires in minutes, and is eighty bits wide.
func (p *Pairing) RedeemHandler() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			http.Error(w, "use POST", http.StatusMethodNotAllowed)
			return
		}

		var request struct {
			Code string `json:"code"`
		}
		// A pairing request is a few dozen bytes. Reading whatever is offered
		// would let an unauthenticated caller decide how much memory to use.
		if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<10)).Decode(&request); err != nil {
			http.Error(w, "malformed request", http.StatusBadRequest)
			return
		}

		// The browser's own description of itself, so an operator listing what
		// is paired can tell two rows apart. Untrusted, and bounded before it
		// is shown to anybody.
		token, err := p.Redeem(request.Code, r.Header.Get("User-Agent"))
		if err != nil {
			// One answer for every way a code can fail to work, so a caller
			// cannot learn whether it was close.
			http.Error(w, "that code is not valid", http.StatusForbidden)
			return
		}

		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("Cache-Control", "no-store")
		_ = json.NewEncoder(w).Encode(struct {
			Token string `json:"token"`
		}{token.Value})
	})
}

// group breaks a code into fours, because a person reading one off a screen
// reads it in fours whatever we do.
func group(code string) string {
	var grouped strings.Builder

	for index, letter := range code {
		if index > 0 && index%4 == 0 {
			grouped.WriteByte('-')
		}
		grouped.WriteRune(letter)
	}
	return grouped.String()
}

// normalise accepts a code the way somebody would actually type it.
func normalise(code string) string {
	return strings.ToUpper(strings.NewReplacer("-", "", " ", "", "\t", "").Replace(strings.TrimSpace(code)))
}
