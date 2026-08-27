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
}

func NewHub() *Hub {
	return &Hub{subscribers: make(map[domain.SessionID]map[*Subscription]struct{})}
}

// Subscription is a wake-up channel plus the means to release it.
type Subscription struct {
	hub     *Hub
	session domain.SessionID
	notify  chan struct{}

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

func (h *Hub) unsubscribe(sub *Subscription) {
	h.mu.Lock()
	defer h.mu.Unlock()

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
	subs := make([]*Subscription, 0, len(h.subscribers[id]))
	for sub := range h.subscribers[id] {
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
