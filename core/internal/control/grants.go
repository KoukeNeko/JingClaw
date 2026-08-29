package control

import (
	"crypto/subtle"
	"fmt"
	"sync"
	"time"
)

// Grants are the console credentials currently in force.
//
// Pairing used to hand every browser the same credential, which meant three
// things nobody wanted: a page paired last week still worked, there was no way
// to see what had been let in, and revoking one browser meant revoking every
// one of them and the operator's own session with it.
//
// Each pairing now mints its own, recorded here. That is what makes "which
// browsers can reach this agent" a question with an answer, and "not that one"
// an instruction that can be carried out.
//
// In memory, so a restart ends every browser session. That is correct rather
// than a limitation: the daemon's whole identity — its port, its credentials,
// its discovery file — is new after a restart, and a browser holding a
// credential from the previous one has nothing to talk to anyway.
type Grants struct {
	// TTL is how long a credential lasts without being used. Zero means it
	// lasts as long as the daemon does.
	//
	// Counted from last use rather than from issue: a console somebody works
	// in all day should not stop halfway through the afternoon, and one nobody
	// has touched since Tuesday should not still be open.
	TTL time.Duration

	// Now is the clock, injected so a test can move it.
	Now func() time.Time

	// NewID names a grant, for an operator revoking one.
	NewID func() string

	mu    sync.Mutex
	items map[string]*grant
}

type grant struct {
	id       string
	token    string
	issuedAt time.Time
	lastUsed time.Time

	// Label is whatever the browser said about itself. Untrusted: it comes
	// from a User-Agent header and its only job is to help a person tell two
	// rows apart.
	label string
}

// Grant is one console credential, as an operator sees it.
type Grant struct {
	ID       string
	IssuedAt time.Time
	LastUsed time.Time
	Label    string
}

func NewGrants(ttl time.Duration, now func() time.Time, newID func() string) *Grants {
	if now == nil {
		now = time.Now
	}
	return &Grants{TTL: ttl, Now: now, NewID: newID, items: map[string]*grant{}}
}

// Issue records a fresh credential and returns it.
func (g *Grants) Issue(label string) (Token, string, error) {
	token, err := NewToken(ScopeConsole)
	if err != nil {
		return Token{}, "", err
	}

	at := g.Now()
	id := g.newID()

	g.mu.Lock()
	defer g.mu.Unlock()

	g.expireLocked(at)
	g.items[id] = &grant{
		id:       id,
		token:    token.Value,
		issuedAt: at,
		lastUsed: at,
		label:    boundLabel(label),
	}

	return token, id, nil
}

// Verify reports whether a credential is one of these, and marks it used.
//
// Compared against every grant rather than looked up, so the time taken does
// not say whether a prefix was right.
func (g *Grants) Verify(provided string) bool {
	at := g.Now()

	g.mu.Lock()
	defer g.mu.Unlock()

	g.expireLocked(at)

	var matched *grant
	for _, one := range g.items {
		if subtle.ConstantTimeCompare([]byte(one.token), []byte(provided)) == 1 {
			matched = one
		}
	}
	if matched == nil {
		return false
	}

	matched.lastUsed = at
	return true
}

// List is what is in force, for an operator deciding what to revoke.
//
// Without the credentials. The whole point of them is that they never appear
// anywhere a person can read them by accident, and a listing is exactly the
// sort of thing that ends up in a screenshot.
func (g *Grants) List() []Grant {
	at := g.Now()

	g.mu.Lock()
	defer g.mu.Unlock()

	g.expireLocked(at)

	listed := make([]Grant, 0, len(g.items))
	for _, one := range g.items {
		listed = append(listed, Grant{
			ID:       one.id,
			IssuedAt: one.issuedAt,
			LastUsed: one.lastUsed,
			Label:    one.label,
		})
	}
	return listed
}

// Revoke ends one credential. It reports whether there was one to end, so a
// mistyped id is not answered as success.
func (g *Grants) Revoke(id string) bool {
	g.mu.Lock()
	defer g.mu.Unlock()

	if _, held := g.items[id]; !held {
		return false
	}
	delete(g.items, id)
	return true
}

// RevokeAll ends every console session at once, for somebody who has decided
// that whatever is out there should not be.
func (g *Grants) RevokeAll() int {
	g.mu.Lock()
	defer g.mu.Unlock()

	ended := len(g.items)
	g.items = map[string]*grant{}
	return ended
}

func (g *Grants) expireLocked(at time.Time) {
	if g.TTL <= 0 {
		return
	}
	for id, one := range g.items {
		if at.Sub(one.lastUsed) >= g.TTL {
			delete(g.items, id)
		}
	}
}

func (g *Grants) newID() string {
	if g.NewID != nil {
		return g.NewID()
	}
	return fmt.Sprintf("con_%d", time.Now().UnixNano())
}

// boundLabel keeps a browser's own description of itself short and harmless.
//
// It is a User-Agent header, which is whatever the other end felt like
// sending. It is shown to a person, so it must not be able to fill a terminal
// or carry control characters into one.
func boundLabel(label string) string {
	const maxLabelLength = 80

	kept := make([]rune, 0, maxLabelLength)
	for _, r := range label {
		if r < 0x20 || r == 0x7f {
			continue
		}
		kept = append(kept, r)
		if len(kept) == maxLabelLength {
			break
		}
	}
	return string(kept)
}
