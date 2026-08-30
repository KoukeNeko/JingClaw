// Package event carries live notifications between the runtime and stream
// subscribers. Durable state lives in internal/storage; nothing here survives
// a restart, and nothing here needs to.
package event

import (
	"sync"

	"github.com/KoukeNeko/JingClaw/core/internal/domain"
)

// Hub wakes subscribers when a session's log grows.
//
// It deliberately carries no events. Each subscriber gets a capacity-1
// struct{} channel and publishers drop the signal when it is already full,
// because a pending wake-up and ten pending wake-ups mean the same thing:
// "go read the store". The store is the source of truth, so a dropped
// notification loses nothing.
//
// Fanning actual events out over channels would fail three ways at once: a
// slow client would block the runtime, a reconnecting client could not be
// backfilled, and there would be no way to apply backpressure without
// silently discarding history.
type Hub struct {
	mu          sync.Mutex
	subscribers map[domain.SessionID]map[*Subscription]struct{}

	// everything holds the subscribers watching the whole log rather than one
	// session. A separate set rather than a session id nobody uses: a
	// sentinel key would make "" mean two things, and every later reader of
	// this file would have to be told which.
	everything map[*Subscription]struct{}
}

func NewHub() *Hub {
	return &Hub{
		subscribers: make(map[domain.SessionID]map[*Subscription]struct{}),
		everything:  make(map[*Subscription]struct{}),
	}
}

// Subscription is a wake-up channel plus the means to release it.
type Subscription struct {
	hub     *Hub
	session domain.SessionID
	notify  chan struct{}

	// all marks a subscription to the whole log. Its session is empty, and
	// that is not what distinguishes it.
	all bool

	closeOnce sync.Once
}

// Notify fires whenever the session's log may have grown.
func (s *Subscription) Notify() <-chan struct{} { return s.notify }

// Close removes the subscription. Safe to call more than once.
func (s *Subscription) Close() {
	s.closeOnce.Do(func() { s.hub.unsubscribe(s) })
}

func (h *Hub) Subscribe(id domain.SessionID) *Subscription {
	sub := &Subscription{
		hub:     h,
		session: id,
		notify:  make(chan struct{}, 1),
	}

	h.mu.Lock()
	defer h.mu.Unlock()

	subs, ok := h.subscribers[id]
	if !ok {
		subs = make(map[*Subscription]struct{})
		h.subscribers[id] = subs
	}
	subs[sub] = struct{}{}

	// Arm it once so a subscriber that starts behind reads the backlog without
	// waiting for the next append.
	sub.signal()

	return sub
}

// SubscribeAll wakes on an append to any session.
//
// For a console showing every conversation at once. It gets the same
// capacity-1 signal as everyone else, so a burst across several sessions is
// one wake-up and one read of the store.
func (h *Hub) SubscribeAll() *Subscription {
	sub := &Subscription{hub: h, notify: make(chan struct{}, 1), all: true}

	h.mu.Lock()
	h.everything[sub] = struct{}{}
	h.mu.Unlock()

	// Armed once, so a subscriber that starts behind reads the backlog
	// without waiting for the next append.
	sub.signal()

	return sub
}

func (h *Hub) unsubscribe(sub *Subscription) {
	h.mu.Lock()
	defer h.mu.Unlock()

	if sub.all {
		delete(h.everything, sub)
		return
	}

	subs, ok := h.subscribers[sub.session]
	if !ok {
		return
	}
	delete(subs, sub)
	if len(subs) == 0 {
		delete(h.subscribers, sub.session)
	}
}

// Publish signals every subscriber of the session. It never blocks, so a slow
// or stalled reader cannot hold up the runtime.
func (h *Hub) Publish(id domain.SessionID) {
	h.mu.Lock()
	subs := make([]*Subscription, 0, len(h.subscribers[id])+len(h.everything))
	for sub := range h.subscribers[id] {
		subs = append(subs, sub)
	}
	for sub := range h.everything {
		subs = append(subs, sub)
	}
	h.mu.Unlock()

	for _, sub := range subs {
		sub.signal()
	}
}

func (s *Subscription) signal() {
	select {
	case s.notify <- struct{}{}:
	default:
		// Already armed; coalescing is the point.
	}
}
