// Package gateway carries traffic between a messaging platform and the
// daemon.
//
// It is a separate process on purpose. It holds somebody else's bot token and
// runs a library that keeps a socket open to the internet; if that library
// panics or its connection loop misbehaves, the process that owns the shell,
// the workspace and the event log must not go down with it. The credential it
// uses reaches the ingress and nothing else, so being compromised means "can
// deliver messages inward", not "can execute tools".
package gateway

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"sync"
	"syscall"
	"time"

	"connectrpc.com/connect"
	"golang.org/x/sync/errgroup"

	controlv1 "github.com/KoukeNeko/JingClaw/core/gen/go/jingclaw/control/v1"
	"github.com/KoukeNeko/JingClaw/core/gen/go/jingclaw/control/v1/controlv1connect"
	"github.com/KoukeNeko/JingClaw/core/internal/cli/client"
	"github.com/KoukeNeko/JingClaw/core/internal/config"
	"github.com/KoukeNeko/JingClaw/core/internal/discovery"
	"github.com/KoukeNeko/JingClaw/core/internal/gateway"
)

const clientName = "jingclaw-gateway"

// Main runs the gateway. Args are the arguments after the subcommand name.
func Main(args []string) error {
	if err := run(args); err != nil && !errors.Is(err, context.Canceled) {
		return err
	}
	return nil
}

func run(args []string) error {
	flags := flag.NewFlagSet("jingclaw gateway", flag.ContinueOnError)
	var (
		configPath = flags.String("config", "", "configuration file; defaults to the one in the config directory")
		platform   = flags.String("platform", "", "messaging platform to serve; overrides the configuration")
		accountID  = flags.String("account", "", "bot account name within JingClaw; overrides the configuration")
	)
	if err := flags.Parse(args); err != nil {
		return err
	}

	// The gateway reads the same file as the daemon, so a deployment is
	// described in one place rather than in two that can disagree.
	cfg, configFile, err := config.Load(*configPath)
	if err != nil {
		return err
	}
	if err := cfg.Validate(); err != nil {
		// The same report the daemon gives. One mistake explained two ways by
		// two processes is how an operator learns to distrust both.
		if config.Report(os.Stderr, err, configFile) {
			os.Exit(1)
		}
		return err
	}

	flags.Visit(func(f *flag.Flag) {
		switch f.Name {
		case "platform":
			cfg.Gateway.Platform = *platform
		case "account":
			cfg.Gateway.SetAccountID(*accountID)
		}
	})

	logger := slog.New(slog.NewJSONHandler(os.Stderr, &slog.HandlerOptions{Level: cfg.LogLevel()}))
	slog.SetDefault(logger)

	rootCtx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	client, err := dialAgent(cfg.Server.RuntimeDir)
	if err != nil {
		return err
	}

	relay := &relay{
		client:    client,
		accountID: cfg.Gateway.Selected().AccountID,
		logger:    logger,
	}

	platformAdapter, err := newAdapter(cfg, relay, relay, logger)
	if err != nil {
		return err
	}
	relay.poster = platformAdapter

	logger.Info("gateway starting",
		"platform", cfg.Gateway.Platform, "account_id", cfg.Gateway.Selected().AccountID)
	fmt.Printf("JingClaw gateway\nConfig:   %s\nPlatform: %s\nAccount:  %s\n",
		configFile, cfg.Gateway.Platform, cfg.Gateway.Selected().AccountID)

	group, ctx := errgroup.WithContext(rootCtx)

	// The connection and the delivery loop are independent: an outage in one
	// must not silently stop the other, and errgroup makes either failing
	// bring the process down rather than leaving it half working.
	group.Go(func() error { return platformAdapter.Run(ctx) })
	group.Go(func() error { return relay.deliver(ctx) })

	if err := group.Wait(); err != nil && !errors.Is(err, context.Canceled) {
		return err
	}

	logger.Info("gateway stopped")
	return nil
}

// relay moves messages inward and dispatches outward.
type relay struct {
	client    controlv1connect.GatewayIngressServiceClient
	poster    adapter
	accountID string
	logger    *slog.Logger

	// toldAbout is the rooms already told the agent cannot be reached,
	// cleared for a room the moment something gets through to it again.
	mu        sync.Mutex
	toldAbout map[string]bool
}

// Deliver hands an inbound message to the agent.
//
// Refusals are expected traffic, not failures: a message from an unbound
// channel or an unlisted account means the system is working.
//
// Somebody who was refused is told so, and that is a change from dropping it
// silently. The reason for the silence was that replying to every bystander
// would make the bot a nuisance, and it no longer applies: the adapter
// delivers only messages that address the bot, so everything that reaches
// here was said to it. Being ignored by something you spoke to directly is
// indistinguishable from it being broken — which is how this was found, by
// somebody having to read the log to learn that a refusal had happened at
// all.
//
// Two refusals stay silent, and both are about not speaking where it was not
// asked to. See sayRefused.
func (r *relay) Deliver(ctx context.Context, message gateway.InboundMessage) error {
	resp, err := r.client.DeliverInbound(ctx, connect.NewRequest(&controlv1.DeliverInboundRequest{
		Message: inboundToProto(message),
	}))
	if err != nil {
		switch connect.CodeOf(err) {
		case connect.CodePermissionDenied, connect.CodeFailedPrecondition:
			r.logger.Info("message not accepted",
				"channel_id", message.Conversation.ChannelID,
				"principal", message.Principal.ID,
				"reason", connect.CodeOf(err).String(),
			)
			r.sayRefused(ctx, message, connect.CodeOf(err))
			return nil
		default:
			r.sayTheAgentIsUnreachable(ctx, message)
			return err
		}
	}

	r.reachedTheAgent(message.Conversation.ChannelID)

	if resp.Msg.GetDuplicate() {
		// The platform redelivered after a reconnect. The reply to the
		// original is already on its way.
		return nil
	}

	r.logger.Info("started work from a message",
		"session_id", resp.Msg.GetSessionId(),
		"run_id", resp.Msg.GetRunId(),
	)
	return nil
}

// deliver posts whatever the agent has queued, and keeps trying.
//
// The stream is re-established after a failure rather than ending the process:
// The daemon restarting is ordinary, and anything unacknowledged is still in
// outbox when the connection comes back.
// Decide carries a button press inward.
//
// The daemon settles whether this person may decide; this process only reports
// what Discord told it. A gateway that could answer that question itself would
// be a bot token that can approve.
func (r *relay) Decide(
	ctx context.Context,
	decision gateway.ApprovalDecision,
) (gateway.DecisionOutcome, error) {
	claims := make([]*controlv1.PrincipalClaim, 0, len(decision.Principal.Claims))
	for _, claim := range decision.Principal.Claims {
		claims = append(claims, &controlv1.PrincipalClaim{
			Namespace: claim.Namespace,
			Value:     claim.Value,
		})
	}

	resp, err := r.client.DeliverDecision(ctx, connect.NewRequest(&controlv1.DeliverDecisionRequest{
		Platform:             string(decision.Conversation.Platform),
		AccountId:            decision.Conversation.AccountID,
		TenantId:             decision.Conversation.TenantID,
		ChannelId:            decision.Conversation.ChannelID,
		PrincipalId:          decision.Principal.ID,
		PrincipalDisplayName: decision.Principal.DisplayName,
		PrincipalIsBot:       decision.Principal.IsBot,
		PrincipalClaims:      claims,
		ApprovalId:           string(decision.ApprovalID),
		Allow:                decision.Allow,
	}))
	if err != nil {
		return "", err
	}

	return outcomeFromProto(resp.Msg.GetOutcome()), nil
}

func outcomeFromProto(outcome controlv1.DecisionOutcome) gateway.DecisionOutcome {
	switch outcome {
	case controlv1.DecisionOutcome_DECISION_OUTCOME_RECORDED:
		return gateway.DecisionRecorded
	case controlv1.DecisionOutcome_DECISION_OUTCOME_ALREADY:
		return gateway.DecisionAlready
	case controlv1.DecisionOutcome_DECISION_OUTCOME_UNAVAILABLE:
		return gateway.DecisionUnavailable
	default:
		return gateway.DecisionRefused
	}
}

// Withdraw carries a message being taken back inward.
//
// Nothing to take back is not a failure and is not answered: the message had
// started being answered, or was never one the agent took, and a room where
// every stray press is answered is a room the bot is a nuisance in.
func (r *relay) Withdraw(ctx context.Context, withdrawal gateway.Withdrawal) error {
	resp, err := r.client.WithdrawInbound(ctx, connect.NewRequest(&controlv1.WithdrawInboundRequest{
		Platform:          string(withdrawal.Principal.Platform),
		AccountId:         withdrawal.Principal.AccountID,
		TenantId:          withdrawal.Principal.TenantID,
		PrincipalId:       withdrawal.Principal.ID,
		IdempotencyKey:    withdrawal.InboundKey,
		PlatformMessageId: withdrawal.MessageID,
	}))
	if err != nil {
		return err
	}
	if resp.Msg.GetWithdrawn() {
		r.logger.Info("took a waiting message back",
			"message_id", withdrawal.MessageID, "principal", withdrawal.Principal.ID)
	}
	return nil
}

func (r *relay) deliver(ctx context.Context) error {
	const retryDelay = 2 * time.Second

	for {
		if err := r.deliverOnce(ctx); err != nil {
			if ctx.Err() != nil {
				return ctx.Err()
			}
			r.logger.Warn("delivery stream ended, reconnecting", "error", err)
		}

		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(retryDelay):
		}
	}
}

func (r *relay) deliverOnce(ctx context.Context) error {
	// Always from zero. Undelivered dispatches are the ones that come back, so
	// starting at the beginning of what is outstanding is correct and needs no
	// cursor kept across restarts.
	stream, err := r.client.SubscribeDispatches(ctx,
		connect.NewRequest(&controlv1.SubscribeDispatchesRequest{AccountId: r.accountID}))
	if err != nil {
		return err
	}
	defer func() { _ = stream.Close() }()

	for stream.Receive() {
		dispatch := stream.Msg().GetDispatch()
		if dispatch == nil {
			continue
		}
		if err := r.post(ctx, dispatch); err != nil {
			r.logger.Error("could not post a dispatch",
				"dispatch_id", dispatch.GetId(), "error", err)
			// Left unacknowledged on purpose, so it is offered again.
			continue
		}
	}

	return stream.Err()
}

func (r *relay) post(ctx context.Context, dispatch *controlv1.Dispatch) error {
	converted, err := dispatchFromProto(dispatch)
	if err != nil {
		return err
	}

	var messageIDs []string
	if staleLog(converted, time.Now()) {
		// Acknowledged without being posted. A log line is context for what
		// is happening now; one from an hour ago, delivered after an outage,
		// is a wall of stale subtext under an answer somebody already read.
		// Two hundred of them arrived at once the first time this ran.
		r.logger.Info("skipped a stale log line", "dispatch_id", converted.ID,
			"age", time.Since(converted.CreatedAt).Round(time.Minute).String())
	} else if messageIDs, err = r.poster.Post(ctx, converted); err != nil {
		return err
	}

	// Acknowledged only after the platform accepted it. A dispatch marked
	// delivered before the post would be lost if the post then failed.
	_, err = r.client.AcknowledgeDispatch(ctx, connect.NewRequest(&controlv1.AcknowledgeDispatchRequest{
		Meta:               &controlv1.RequestMeta{ClientId: clientName},
		DispatchId:         dispatch.GetId(),
		PlatformMessageIds: messageIDs,
	}))
	return err
}

// dialAgent connects to the daemon with the gateway-scoped credential.
//
// Read once here so that starting with no daemon fails immediately and says
// so, rather than after the platform connection is up. Where the requests
// actually go is decided per request, below.
func dialAgent(runtimeDir string) (controlv1connect.GatewayIngressServiceClient, error) {
	path, err := discovery.PathIn(runtimeDir)
	if err != nil {
		return nil, err
	}

	found, err := discovery.Read(path)
	if err != nil {
		return nil, fmt.Errorf("%w (is jingclaw running?)", err)
	}
	if found.GatewayToken == "" {
		return nil, errors.New("agentd did not publish a gateway credential")
	}

	httpClient := &http.Client{
		Transport: &client.AtTheDaemon{Path: path, As: client.AsTheGateway},
	}

	// The base URL is a placeholder: every request is rewritten to wherever
	// the discovery file points when it is made.
	return controlv1connect.NewGatewayIngressServiceClient(httpClient, found.BaseURL), nil
}

// refusedInThisChannel is what somebody sees when the channel will not take
// their message.
//
// It does not name who may: the list is the operator's to share, and a bot
// reading it out to whoever asks is a different decision from the one they
// made when they wrote it down. What it does say is that this is a setting
// rather than a fault, and where to go about it.
const refusedInThisChannel = "I can't take that here — this channel only " +
	"accepts messages from certain accounts, and yours is not one of them. " +
	"Whoever set the bot up can change that."

// cannotReachTheAgent is what somebody sees when their message reached the
// gateway and got no further.
//
// It says the message is gone rather than delayed, because it is: nothing
// queues it and nothing retries it. Telling somebody it will be picked up
// later would be a comfortable thing to say and untrue, and they would find
// out by waiting.
const cannotReachTheAgent = "I'm connected here but I can't reach the agent " +
	"right now, so this message did not get to it. It is not queued — send it " +
	"again once whoever runs the bot has it back up."

// sayTheAgentIsUnreachable tells a room that its message went nowhere, once
// per outage.
//
// Once, because a daemon that is down stays down, and answering every message
// with the same line turns one outage into a room nobody can read. Per room,
// because somebody in a channel that has not been told has no way to know.
//
// The reaction the adapter adds on the way in is what makes this necessary:
// it marks the message as taken before anything is delivered, so a gateway
// that cannot reach the daemon produces exactly what a working one produces.
func (r *relay) sayTheAgentIsUnreachable(ctx context.Context, message gateway.InboundMessage) {
	channel := message.Conversation.ChannelID

	r.mu.Lock()
	if r.toldAbout == nil {
		r.toldAbout = map[string]bool{}
	}
	already := r.toldAbout[channel]
	r.toldAbout[channel] = true
	r.mu.Unlock()

	if already {
		return
	}

	if _, err := r.poster.Post(ctx, gateway.Dispatch{
		AccountID: r.accountID,
		Target:    message.Conversation,
		Kind:      gateway.DispatchMessage,
		Payload:   cannotReachTheAgent,
	}); err != nil {
		// Logged and dropped, like a refusal. The message was undeliverable
		// either way, and the caller's error is about that rather than about
		// this.
		r.logger.Warn("could not say that the agent is unreachable",
			"channel_id", channel, "error", err)
	}
}

// reachedTheAgent forgets that a room was told, so the next outage is said
// again rather than passing in the silence this exists to end.
func (r *relay) reachedTheAgent(channel string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	delete(r.toldAbout, channel)
}

// sayRefused tells somebody their message was not accepted, when saying so is
// the right thing to do.
//
// Only for a refusal that is about them. A channel nobody bound gets silence:
// the bot has no business announcing itself in every room it can see, and a
// message there was addressed to something that has not been asked to be
// present. That is the difference between "you may not" and "not here".
//
// A failure to say it is logged and dropped. The message was refused either
// way, and turning the explanation's failure into the caller's error would
// make a working refusal look like a broken gateway.
func (r *relay) sayRefused(
	ctx context.Context, message gateway.InboundMessage, code connect.Code,
) {
	if code != connect.CodePermissionDenied {
		return
	}

	_, err := r.poster.Post(ctx, gateway.Dispatch{
		AccountID: r.accountID,
		Target:    message.Conversation,
		Kind:      gateway.DispatchMessage,
		Payload:   refusedInThisChannel,
	})
	if err != nil {
		r.logger.Warn("could not say why a message was refused",
			"channel_id", message.Conversation.ChannelID,
			"error", err,
		)
	}
}
