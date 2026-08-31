package gateway_test

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"path/filepath"
	"testing"
	"time"

	"github.com/KoukeNeko/JingClaw/core/internal/domain"
	"github.com/KoukeNeko/JingClaw/core/internal/gateway"
	"github.com/KoukeNeko/JingClaw/core/internal/permission"
	"github.com/KoukeNeko/JingClaw/core/internal/storage/sqlite"
	"github.com/KoukeNeko/JingClaw/core/internal/tool"
)

// This is where text written by anyone who can type into a channel meets a
// process that can change files. Every default here is no.

// fakeRuntime records what the ingress asked for without running a model.
//
// Sessions are written to the real store rather than invented, because the
// conversation table references them: a mapping pointing at a session that
// does not exist is corruption, and the foreign key is right to say so.
type fakeRuntime struct {
	store *sqlite.Store

	sessions int
	turns    []turn
}

type turn struct {
	session     domain.SessionID
	text        string
	origin      domain.RunOrigin
	targets     []domain.DeliveryTarget
	attachments []domain.Attachment
}

func (r *fakeRuntime) CreateSession(ctx context.Context, title string) (domain.Session, error) {
	r.sessions++

	session := domain.Session{
		ID:        domain.SessionID(fmt.Sprintf("ses_%d", r.sessions)),
		Title:     title,
		CreatedAt: time.Unix(0, 0).UTC(),
		UpdatedAt: time.Unix(0, 0).UTC(),
	}
	if err := r.store.CreateSession(ctx, session); err != nil {
		return domain.Session{}, err
	}
	return session, nil
}

func (r *fakeRuntime) SendTurnTo(
	_ context.Context,
	session domain.SessionID,
	sent domain.Turn,
) (domain.RunID, domain.MessageID, error) {
	r.turns = append(r.turns, turn{
		session:     session,
		text:        sent.Text,
		origin:      sent.Origin,
		targets:     sent.Targets,
		attachments: sent.Attachments,
	})
	return domain.RunID("run-" + sent.Text), "msg", nil
}

func newIngress(t *testing.T) (*gateway.Ingress, *sqlite.Store, *fakeRuntime, *permission.Engine) {
	t.Helper()

	store, err := sqlite.Open(context.Background(), filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { _ = store.Close() })

	runtime := &fakeRuntime{store: store}
	engine := permission.New(permission.LocalProfile())

	ingress := &gateway.Ingress{
		Store:   store,
		Runtime: runtime,
		Binder:  engine,
		Now:     func() time.Time { return time.Unix(0, 0).UTC() },
		Logger:  slog.New(slog.DiscardHandler),
	}

	return ingress, store, runtime, engine
}

func discordConversation() gateway.ConversationRef {
	return gateway.ConversationRef{
		Platform:  gateway.PlatformDiscord,
		AccountID: "main-bot",
		TenantID:  "guild_1",
		ChannelID: "channel_1",
		ThreadID:  "thread_1",
	}
}

func discordPrincipal(id string) gateway.Principal {
	return gateway.Principal{
		Platform:    gateway.PlatformDiscord,
		AccountID:   "main-bot",
		TenantID:    "guild_1",
		ID:          id,
		DisplayName: "Someone",
	}
}

func message(key, text string, principal gateway.Principal) gateway.InboundMessage {
	return gateway.InboundMessage{
		PlatformMessageID: key,
		IdempotencyKey:    key,
		Principal:         principal,
		Conversation:      discordConversation(),
		Text:              text,
		Trigger:           gateway.TriggerMention,
		OccurredAt:        time.Unix(0, 0).UTC(),
	}
}

func bindChannel(t *testing.T, store *sqlite.Store, allowed ...string) {
	t.Helper()

	conversation := discordConversation()
	err := store.UpsertBinding(context.Background(), gateway.Binding{
		ID:                "binding_1",
		Platform:          conversation.Platform,
		AccountID:         conversation.AccountID,
		TenantID:          conversation.TenantID,
		ChannelID:         conversation.ChannelID,
		WorkspaceID:       "ws_1",
		PermissionProfile: "gateway",
		AllowedPrincipals: allowed,
		CreatedAt:         time.Unix(0, 0).UTC(),
	})
	if err != nil {
		t.Fatalf("bind channel: %v", err)
	}
}

// A channel nobody configured is not an open door. Without a binding there is
// no workspace to work in and nobody who said this channel may reach one.
func TestUnboundChannelIsRefused(t *testing.T) {
	ingress, _, runtime, _ := newIngress(t)

	_, err := ingress.Accept(context.Background(), message("m1", "do something", discordPrincipal("user_1")))
	if !errors.Is(err, gateway.ErrBindingNotFound) {
		t.Fatalf("got %v, want ErrBindingNotFound", err)
	}
	if runtime.sessions != 0 {
		t.Error("a session was created for an unbound channel")
	}
}

// A binding with nobody on its allowlist defers to the room.
//
// This used to be the opposite, and the reason it was is worth keeping: an
// empty list must not quietly read as "everyone" through carelessness. What
// changed is that it is not careless — a channel already has a membership,
// decided on the platform by the same person who wrote the binding, and
// naming people twice means keeping two lists in step. The way that fails is
// somebody being added to the room and silently ignored, which is what
// prompted this.
//
// What did not change is beside it: TestAnEmptyListNeverMeansAnyoneMayApprove
// in gateway_test.go. Being allowed to ask is not being allowed to permit.
func TestAnEmptyAllowlistDefersToTheRoom(t *testing.T) {
	ingress, store, runtime, _ := newIngress(t)
	bindChannel(t, store)

	if _, err := ingress.Accept(context.Background(),
		message("m1", "do something", discordPrincipal("user_1"))); err != nil {
		t.Fatalf("somebody the room let in was refused: %v", err)
	}
	if len(runtime.turns) != 1 {
		t.Errorf("started %d turns, want one", len(runtime.turns))
	}
}

func TestUnlistedPrincipalIsRefused(t *testing.T) {
	ingress, store, runtime, _ := newIngress(t)
	bindChannel(t, store, "user_allowed")

	_, err := ingress.Accept(context.Background(), message("m1", "do something", discordPrincipal("user_other")))
	if !errors.Is(err, gateway.ErrNotPermitted) {
		t.Fatalf("got %v, want ErrNotPermitted", err)
	}
	if len(runtime.turns) != 0 {
		t.Error("work started for an unlisted principal")
	}
}

// Authorization reads the platform's stable ID. A display name can be changed
// by its owner at any moment, so a rule written against it is one anybody can
// satisfy by renaming themselves.
func TestDisplayNameIsNotIdentity(t *testing.T) {
	ingress, store, _, _ := newIngress(t)
	bindChannel(t, store, "user_allowed")

	impostor := discordPrincipal("user_impostor")
	impostor.DisplayName = "user_allowed"

	_, err := ingress.Accept(context.Background(), message("m1", "do something", impostor))
	if !errors.Is(err, gateway.ErrNotPermitted) {
		t.Fatalf("an impostor with a matching display name got through: %v", err)
	}
}

// Two automations acting on each other's messages is an unbounded loop, so a
// bot is refused regardless of any allowlist.
func TestBotsAreRefusedEvenWhenListed(t *testing.T) {
	ingress, store, _, _ := newIngress(t)
	bindChannel(t, store, "bot_1")

	bot := discordPrincipal("bot_1")
	bot.IsBot = true

	_, err := ingress.Accept(context.Background(), message("m1", "do something", bot))
	if !errors.Is(err, gateway.ErrNotPermitted) {
		t.Fatalf("a bot on the allowlist was accepted: %v", err)
	}
}

// Text overheard in a channel is not a request. Acting on it would mean
// anything typed near the agent could set it working.
func TestAmbientMessagesDoNotTriggerWork(t *testing.T) {
	ingress, store, runtime, _ := newIngress(t)
	bindChannel(t, store, "user_1")

	ambient := message("m1", "we should really fix that bug", discordPrincipal("user_1"))
	ambient.Trigger = gateway.TriggerAmbient

	_, err := ingress.Accept(context.Background(), ambient)
	if !errors.Is(err, gateway.ErrNotExplicit) {
		t.Fatalf("got %v, want ErrNotExplicit", err)
	}
	if len(runtime.turns) != 0 {
		t.Error("overheard text started work")
	}
}

func TestPermittedMessageStartsWorkWithGatewayOrigin(t *testing.T) {
	ingress, store, runtime, _ := newIngress(t)
	bindChannel(t, store, "user_1")

	accepted, err := ingress.Accept(context.Background(),
		message("m1", "please look at the tests", discordPrincipal("user_1")))
	if err != nil {
		t.Fatalf("accept: %v", err)
	}
	if accepted.RunID == "" {
		t.Fatal("no run was started")
	}

	if len(runtime.turns) != 1 {
		t.Fatalf("started %d turns, want 1", len(runtime.turns))
	}

	// The run must be recorded as coming from the platform account, not from
	// whoever happens to be running the daemon.
	origin := runtime.turns[0].origin
	if origin.Kind != domain.OriginGateway {
		t.Errorf("origin kind %q, want %q", origin.Kind, domain.OriginGateway)
	}
	if origin.Principal == nil || origin.Principal.PrincipalID != "user_1" {
		t.Errorf("origin does not identify the platform account: %+v", origin.Principal)
	}
}

// A session opened from a channel must run under the gateway profile from its
// very first turn; one turn under the wrong profile is one turn answered
// wrongly.
func TestSessionRunsUnderTheGatewayProfile(t *testing.T) {
	ingress, store, _, engine := newIngress(t)
	bindChannel(t, store, "user_1")

	accepted, err := ingress.Accept(context.Background(),
		message("m1", "look at the tests", discordPrincipal("user_1")))
	if err != nil {
		t.Fatalf("accept: %v", err)
	}

	outcome := engine.Evaluate(context.Background(), permission.Request{
		Spec:      tool.Spec{Name: "exec_command", Level: tool.LevelExecute},
		SessionID: accepted.SessionID,
	})
	if outcome.Decision != permission.Deny {
		t.Errorf("execution from a channel was %q, want %q", outcome.Decision, permission.Deny)
	}

	// A local session is unaffected: the two profiles coexist in one daemon.
	local := engine.Evaluate(context.Background(), permission.Request{
		Spec:      tool.Spec{Name: "exec_command", Level: tool.LevelExecute},
		SessionID: "ses_local",
	})
	if local.Decision != permission.Ask {
		t.Errorf("execution locally was %q, want %q", local.Decision, permission.Ask)
	}
}

// Platforms redeliver on reconnect. One dropped connection must not turn a
// single request into several runs, each doing the same work.
func TestRedeliveredMessageDoesNotRunTwice(t *testing.T) {
	ingress, store, runtime, _ := newIngress(t)
	bindChannel(t, store, "user_1")

	original := message("m1", "fix the tests", discordPrincipal("user_1"))

	first, err := ingress.Accept(context.Background(), original)
	if err != nil {
		t.Fatalf("first accept: %v", err)
	}
	if first.Duplicate {
		t.Error("the first delivery was reported as a duplicate")
	}

	second, err := ingress.Accept(context.Background(), original)
	if err != nil {
		t.Fatalf("redelivery: %v", err)
	}
	if !second.Duplicate {
		t.Error("a redelivered message was not recognised")
	}
	if second.SessionID != first.SessionID {
		t.Errorf("the redelivery landed in a different session")
	}

	if len(runtime.turns) != 1 {
		t.Errorf("a redelivered message produced %d runs, want 1", len(runtime.turns))
	}
}

// A thread is one conversation, so a follow-up continues it rather than
// starting a session that has forgotten everything said before.
func TestFollowUpContinuesTheSameSession(t *testing.T) {
	ingress, store, runtime, _ := newIngress(t)
	bindChannel(t, store, "user_1")

	first, err := ingress.Accept(context.Background(), message("m1", "look at the tests", discordPrincipal("user_1")))
	if err != nil {
		t.Fatalf("first: %v", err)
	}
	second, err := ingress.Accept(context.Background(), message("m2", "and the build", discordPrincipal("user_1")))
	if err != nil {
		t.Fatalf("second: %v", err)
	}

	if first.SessionID != second.SessionID {
		t.Errorf("a follow-up opened a new session: %s then %s", first.SessionID, second.SessionID)
	}
	if runtime.sessions != 1 {
		t.Errorf("created %d sessions for one thread, want 1", runtime.sessions)
	}
	if len(runtime.turns) != 2 {
		t.Errorf("started %d turns, want 2", len(runtime.turns))
	}
}

// Separate threads are separate work and must not share a history.
func TestDifferentThreadsGetDifferentSessions(t *testing.T) {
	ingress, store, _, _ := newIngress(t)
	bindChannel(t, store, "user_1")

	first, err := ingress.Accept(context.Background(), message("m1", "task one", discordPrincipal("user_1")))
	if err != nil {
		t.Fatalf("first: %v", err)
	}

	other := message("m2", "task two", discordPrincipal("user_1"))
	other.Conversation.ThreadID = "thread_2"

	second, err := ingress.Accept(context.Background(), other)
	if err != nil {
		t.Fatalf("second: %v", err)
	}

	if first.SessionID == second.SessionID {
		t.Error("two threads shared one session")
	}
}

// A binding naming a profile that does not exist must stop the message, not
// quietly fall back to a more permissive one.
func TestUnknownProfileInABindingIsFatal(t *testing.T) {
	ingress, store, runtime, _ := newIngress(t)

	conversation := discordConversation()
	if err := store.UpsertBinding(context.Background(), gateway.Binding{
		ID:                "binding_1",
		Platform:          conversation.Platform,
		AccountID:         conversation.AccountID,
		TenantID:          conversation.TenantID,
		ChannelID:         conversation.ChannelID,
		WorkspaceID:       "ws_1",
		PermissionProfile: "not-a-real-profile",
		AllowedPrincipals: []string{"user_1"},
		CreatedAt:         time.Unix(0, 0).UTC(),
	}); err != nil {
		t.Fatalf("bind: %v", err)
	}

	if _, err := ingress.Accept(context.Background(),
		message("m1", "do something", discordPrincipal("user_1"))); err == nil {
		t.Fatal("a binding with an unknown profile was accepted")
	}
	if len(runtime.turns) != 0 {
		t.Error("work started under an unresolved profile")
	}
}

// Role-based access has to work from opaque claims, because a Discord role and
// a GitHub collaborator status are not the same kind of thing.
func TestClaimBasedAccess(t *testing.T) {
	ingress, store, _, _ := newIngress(t)

	conversation := discordConversation()
	if err := store.UpsertBinding(context.Background(), gateway.Binding{
		ID:                "binding_1",
		Platform:          conversation.Platform,
		AccountID:         conversation.AccountID,
		TenantID:          conversation.TenantID,
		ChannelID:         conversation.ChannelID,
		WorkspaceID:       "ws_1",
		PermissionProfile: "gateway",
		AllowedClaims:     []gateway.Claim{{Namespace: "discord.role", Value: "role_maintainer"}},
		CreatedAt:         time.Unix(0, 0).UTC(),
	}); err != nil {
		t.Fatalf("bind: %v", err)
	}

	withRole := discordPrincipal("user_1")
	withRole.Claims = []gateway.Claim{{Namespace: "discord.role", Value: "role_maintainer"}}
	if _, err := ingress.Accept(context.Background(), message("m1", "go", withRole)); err != nil {
		t.Fatalf("a principal with the required role was refused: %v", err)
	}

	withoutRole := discordPrincipal("user_2")
	withoutRole.Claims = []gateway.Claim{{Namespace: "discord.role", Value: "role_everyone"}}
	if _, err := ingress.Accept(context.Background(), message("m2", "go", withoutRole)); !errors.Is(err, gateway.ErrNotPermitted) {
		t.Fatalf("a principal without the role was accepted: %v", err)
	}
}

// inChannel is a message sent in a channel rather than in a thread, which is
// where the conversation key used to come apart.
func inChannel(key, text string) gateway.InboundMessage {
	message := message(key, text, discordPrincipal("user_1"))
	message.Conversation.ThreadID = ""
	return message
}

// Two mentions in one channel continue one session.
//
// This is what "the agent has no memory" looked like from a channel: each
// message keyed a conversation of its own, so asking something and then saying
// "go ahead" reached an agent that had never heard of the first message.
func TestTwoMessagesInAChannelContinueOneSession(t *testing.T) {
	ingress, store, _, _ := newIngress(t)
	bindChannel(t, store, "user_1")

	first, err := ingress.Accept(context.Background(), inChannel("m1", "開始吧"))
	if err != nil {
		t.Fatalf("first message: %v", err)
	}

	second, err := ingress.Accept(context.Background(), inChannel("m2", "繼續"))
	if err != nil {
		t.Fatalf("second message: %v", err)
	}

	if first.SessionID != second.SessionID {
		t.Errorf("two messages in one channel produced sessions %s and %s",
			first.SessionID, second.SessionID)
	}
	if first.RunID == second.RunID {
		t.Error("the second message did not start a run of its own")
	}
}

// A thread is how somebody says they want to start something else, so it has
// to be a session of its own.
func TestAThreadGetsItsOwnSession(t *testing.T) {
	ingress, store, _, _ := newIngress(t)
	bindChannel(t, store, "user_1")

	channel, err := ingress.Accept(context.Background(), inChannel("m1", "開始吧"))
	if err != nil {
		t.Fatalf("channel message: %v", err)
	}

	threaded := inChannel("m2", "另一件事")
	threaded.Conversation.ThreadID = "thread_9"

	thread, err := ingress.Accept(context.Background(), threaded)
	if err != nil {
		t.Fatalf("thread message: %v", err)
	}

	if channel.SessionID == thread.SessionID {
		t.Error("a thread continued the channel's session")
	}
}
