// Command gatewayd carries traffic between a messaging platform and agentd.
//
// It is a separate process on purpose. It holds somebody else's bot token and
// runs a library that keeps a socket open to the internet; if that library
// panics or its connection loop misbehaves, the process that owns the shell,
// the workspace and the event log must not go down with it. The credential it
// uses reaches the ingress and nothing else, so being compromised means "can
// deliver messages inward", not "can execute tools".
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"connectrpc.com/connect"
	"golang.org/x/sync/errgroup"

	controlv1 "github.com/KoukeNeko/JingClaw/core/gen/go/jingclaw/control/v1"
	"github.com/KoukeNeko/JingClaw/core/gen/go/jingclaw/control/v1/controlv1connect"
	"github.com/KoukeNeko/JingClaw/core/internal/config"
	"github.com/KoukeNeko/JingClaw/core/internal/discovery"
	"github.com/KoukeNeko/JingClaw/core/internal/gateway"
)

const clientName = "jingclaw-gatewayd"

func main() {
	if err := run(); err != nil && !errors.Is(err, context.Canceled) {
		slog.Error("gatewayd exited", "error", err)
		os.Exit(1)
	}
}

func run() error {
	var (
		configPath = flag.String("config", "", "configuration file; defaults to the one in the config directory")
		platform   = flag.String("platform", "", "messaging platform to serve; overrides the configuration")
		accountID  = flag.String("account", "", "bot account name within JingClaw; overrides the configuration")
	)
	flag.Parse()

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

	flag.Visit(func(f *flag.Flag) {
		switch f.Name {
		case "platform":
			cfg.Gateway.Platform = *platform
		case "account":
			cfg.Gateway.AccountID = *accountID
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
		accountID: cfg.Gateway.AccountID,
		logger:    logger,
	}

	platformAdapter, err := newAdapter(cfg, relay, logger)
	if err != nil {
		return err
	}
	relay.poster = platformAdapter

	logger.Info("gateway starting",
		"platform", cfg.Gateway.Platform, "account_id", cfg.Gateway.AccountID)
	fmt.Printf("JingClaw gateway\nConfig:   %s\nPlatform: %s\nAccount:  %s\n",
		configFile, cfg.Gateway.Platform, cfg.Gateway.AccountID)

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
}

// Deliver hands an inbound message to the agent.
//
// Refusals are expected traffic, not failures: a message from an unbound
// channel or an unlisted account means the system is working. They are logged
// at info and dropped, because replying "you are not allowed" to every
// bystander would turn the bot into a nuisance.
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
			return nil
		default:
			return err
		}
	}

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
// agentd restarting is ordinary, and anything unacknowledged is still in the
// outbox when the connection comes back.
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

	messageIDs, err := r.poster.Post(ctx, converted)
	if err != nil {
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

// dialAgent connects to agentd with the gateway-scoped credential.
func dialAgent(runtimeDir string) (controlv1connect.GatewayIngressServiceClient, error) {
	path, err := discovery.PathIn(runtimeDir)
	if err != nil {
		return nil, err
	}

	found, err := discovery.Read(path)
	if err != nil {
		return nil, fmt.Errorf("%w (is agentd running?)", err)
	}
	if found.GatewayToken == "" {
		return nil, errors.New("agentd did not publish a gateway credential")
	}

	httpClient := &http.Client{
		Transport: &bearerTransport{token: found.GatewayToken, base: http.DefaultTransport},
	}
	return controlv1connect.NewGatewayIngressServiceClient(httpClient, found.BaseURL), nil
}

type bearerTransport struct {
	token string
	base  http.RoundTripper
}

func (t *bearerTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	clone := req.Clone(req.Context())
	clone.Header.Set("Authorization", "Bearer "+t.token)
	return t.base.RoundTrip(clone)
}
