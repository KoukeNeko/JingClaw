package event

import (
	"testing"
	"time"
)

// woke reports whether a subscription was signalled, without waiting on a
// channel that may never fire.
func woke(sub *Subscription) bool {
	select {
	case <-sub.Notify():
		return true
	case <-time.After(50 * time.Millisecond):
		return false
	}
}

// drain clears the signal every subscription is armed with when it opens.
func drain(sub *Subscription) { <-sub.Notify() }

func TestASessionSubscriberWakesOnItsOwnSession(t *testing.T) {
	hub := NewHub()
	sub := hub.Subscribe("a")
	defer sub.Close()
	drain(sub)

	hub.Publish("a")
	if !woke(sub) {
		t.Error("a subscriber to a did not wake on a")
	}
}

func TestASessionSubscriberIgnoresOtherSessions(t *testing.T) {
	hub := NewHub()
	sub := hub.Subscribe("a")
	defer sub.Close()
	drain(sub)

	hub.Publish("b")
	if woke(sub) {
		t.Error("a subscriber to a woke on b")
	}
}

// The console watches every conversation, so it has to wake on all of them.
func TestWatchingEverythingWakesOnAnySession(t *testing.T) {
	hub := NewHub()
	all := hub.SubscribeAll()
	defer all.Close()
	drain(all)

	hub.Publish("a")
	if !woke(all) {
		t.Fatal("watching everything did not wake on a")
	}

	hub.Publish("b")
	if !woke(all) {
		t.Error("watching everything did not wake on b")
	}
}

// One publish reaches both kinds. A console and a client watching the same
// session must not be able to starve one another.
func TestOnePublishReachesBothKinds(t *testing.T) {
	hub := NewHub()
	one := hub.Subscribe("a")
	all := hub.SubscribeAll()
	defer one.Close()
	defer all.Close()
	drain(one)
	drain(all)

	hub.Publish("a")

	if !woke(one) {
		t.Error("the session subscriber did not wake")
	}
	if !woke(all) {
		t.Error("the whole-log subscriber did not wake")
	}
}

// Closing one must not leave the other subscribed, and must not remove it.
func TestClosingOneLeavesTheOther(t *testing.T) {
	hub := NewHub()
	one := hub.Subscribe("a")
	all := hub.SubscribeAll()
	defer all.Close()
	drain(one)
	drain(all)

	one.Close()
	hub.Publish("a")

	if !woke(all) {
		t.Error("closing a session subscriber silenced the whole-log one")
	}

	hub.mu.Lock()
	remaining := len(hub.subscribers)
	hub.mu.Unlock()
	if remaining != 0 {
		t.Errorf("%d session subscribers remain after closing the only one", remaining)
	}
}

// Closing a whole-log subscription must remove it from its own set rather
// than looking for it under an empty session id.
func TestClosingAWholeLogSubscriptionRemovesIt(t *testing.T) {
	hub := NewHub()
	all := hub.SubscribeAll()
	drain(all)
	all.Close()

	hub.mu.Lock()
	remaining := len(hub.everything)
	hub.mu.Unlock()
	if remaining != 0 {
		t.Errorf("%d whole-log subscribers remain after closing the only one", remaining)
	}
}
