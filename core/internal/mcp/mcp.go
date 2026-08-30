// Package mcp connects the agent to tool servers that speak the Model Context
// Protocol over stdio.
//
// The point of the package is not the protocol, which the official SDK
// handles. It is the boundary: an MCP server is somebody else's program, and
// what it says about itself cannot be allowed to decide what it is permitted
// to do. A server declaring its tool read-only would otherwise walk straight
// past the approval that a server declaring it dangerous would have to stop
// for. So the risk level is taken from this machine's configuration and the
// server's opinion of itself is used only for the name, the description and
// the argument schema — the things a model needs and a policy does not read.
package mcp

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"os/exec"
	"regexp"
	"strings"
	"sync"
	"time"

	sdk "github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/KoukeNeko/JingClaw/core/internal/artifact"
	"github.com/KoukeNeko/JingClaw/core/internal/mcp/mcpauth"
	"github.com/KoukeNeko/JingClaw/core/internal/tool"
)

const (
	clientName    = "jingclaw"
	clientVersion = "0.1.0"

	// nameLimit is the shortest ceiling across the providers we target. A tool
	// the model cannot name is a tool that does not exist.
	nameLimit = 64

	defaultStartTimeout = 30 * time.Second
	defaultCallTimeout  = 2 * time.Minute
	defaultMaxOutput    = 32 * 1024
)

// ServerConfig describes one server to run.
type ServerConfig struct {
	// Name distinguishes this server's tools from every other source of tools.
	Name string

	// Command runs the server as a child of this daemon, speaking over its
	// standard streams. URL reaches one that is already running, over HTTP.
	// Exactly one of them is given.
	Command string
	Args    []string

	// URL is a Streamable HTTP endpoint. A server reached this way is
	// somebody else's process on somebody else's schedule: it is not started,
	// not stopped, and not killed when this daemon exits.
	//
	// It is also a network hop, which the child-process kind is not. What
	// arrives is the same untrusted tool output either way, but the reach is
	// wider: an address here is a place this machine will connect to whenever
	// the agent uses a tool.
	URL string

	// Headers are sent with every request to a URL server, which is where an
	// authorization header goes.
	Headers map[string]string

	// Env are literal values for the child's environment, and PassEnv names
	// variables forwarded from the daemon's own.
	//
	// The daemon's environment is not inherited wholesale. It holds the
	// provider credentials, and handing those to every tool server somebody
	// adds would make installing one an act of trust nobody was asked for.
	Env     map[string]string
	PassEnv []string

	// Level is what this server's tools count as when the policy engine looks
	// at them. It comes from configuration rather than from the server.
	Level tool.Level

	// OAuth says this server authorizes with OAuth, so the daemon should
	// present whatever somebody signed in with.
	//
	// Only meaningful with a URL. MCP is explicit that a server spoken to
	// over a pipe should not use this profile: a child process gets its
	// credentials from the environment it was started with, and there is no
	// address for an authorization server to protect.
	OAuth bool

	// Sessions is where signed-in sessions are kept. Nil means this daemon
	// has nowhere to keep one, which makes an OAuth server unreachable rather
	// than unauthenticated.
	Sessions *mcpauth.Store
}

// transportFor decides how to reach a server, and refuses anything ambiguous.
//
// One of the two, never both. A configuration naming a command and a URL is
// one where somebody meant something specific and this cannot tell which, and
// guessing would silently start a process or open a connection nobody asked
// for.
func transportFor(cfg ServerConfig) (sdk.Transport, error) {
	if cfg.Name == "" {
		return nil, fmt.Errorf("mcp: a server needs a name")
	}

	hasCommand := cfg.Command != ""
	hasURL := cfg.URL != ""

	switch {
	case hasCommand && hasURL:
		return nil, fmt.Errorf("mcp: %s names both a command and a url; it can only be one", cfg.Name)
	case !hasCommand && !hasURL:
		return nil, fmt.Errorf("mcp: %s names neither a command nor a url", cfg.Name)
	}

	if hasURL {
		transport := &sdk.StreamableClientTransport{
			Endpoint:   cfg.URL,
			HTTPClient: &http.Client{Transport: headerTransport(cfg.Headers)},
		}
		if cfg.OAuth {
			if cfg.Sessions == nil {
				return nil, fmt.Errorf(
					"mcp: %s authorizes with oauth but there is nowhere to keep a session", cfg.Name)
			}
			// The daemon's side: it presents what somebody signed in with and
			// refuses to start a flow of its own, because there is nobody
			// here to finish one.
			transport.OAuthHandler = &mcpauth.Stored{Server: cfg.Name, Store: cfg.Sessions}
		}
		return transport, nil
	}

	if cfg.OAuth {
		// Said rather than ignored. A server spoken to over a pipe takes its
		// credentials from the environment it was started with, and quietly
		// dropping the setting would leave somebody waiting for a sign-in
		// prompt that is never coming.
		return nil, fmt.Errorf(
			"mcp: %s runs as a command, which does not authorize with oauth; "+
				"give it a credential through env or pass_env", cfg.Name)
	}

	command := exec.Command(cfg.Command, cfg.Args...)
	command.Env = environment(cfg)
	// Servers log to stderr by convention; letting it through is how an
	// operator finds out why one is misbehaving.
	command.Stderr = os.Stderr

	return &sdk.CommandTransport{Command: command}, nil
}

// headerTransport adds the configured headers to every request.
//
// A round tripper rather than headers set once, because the SDK owns the
// requests it makes and there is nowhere else to put them.
func headerTransport(headers map[string]string) http.RoundTripper {
	if len(headers) == 0 {
		return http.DefaultTransport
	}
	return &withHeaders{headers: headers, base: http.DefaultTransport}
}

type withHeaders struct {
	headers map[string]string
	base    http.RoundTripper
}

func (t *withHeaders) RoundTrip(req *http.Request) (*http.Response, error) {
	// Cloned, because a RoundTripper must not modify the request it is given.
	clone := req.Clone(req.Context())
	for name, value := range t.headers {
		clone.Header.Set(name, value)
	}
	return t.base.RoundTrip(clone)
}

// Limits bound what running a server may cost.
type Limits struct {
	StartTimeout time.Duration
	CallTimeout  time.Duration
	MaxOutput    int
}

func (l Limits) withDefaults() Limits {
	if l.StartTimeout <= 0 {
		l.StartTimeout = defaultStartTimeout
	}
	if l.CallTimeout <= 0 {
		l.CallTimeout = defaultCallTimeout
	}
	if l.MaxOutput <= 0 {
		l.MaxOutput = defaultMaxOutput
	}
	return l
}

// Server is one connected MCP server and the tools it offered.
type Server struct {
	name      string
	session   *sdk.ClientSession
	limits    Limits
	artifacts *artifact.Store
	tools     []tool.Tool
	logger    *slog.Logger

	// closeOnce guards against a double Close from shutdown racing a failure
	// path; the SDK's session is not documented as tolerating one.
	closeOnce sync.Once
}

// Connect starts a server and asks what it can do.
//
// The handshake is given a deadline of its own. A server that never answers
// initialize would otherwise hold up the daemon indefinitely, and a tool
// server that cannot start in half a minute is broken rather than slow.
func Connect(
	ctx context.Context,
	cfg ServerConfig,
	limits Limits,
	artifacts *artifact.Store,
	logger *slog.Logger,
) (*Server, error) {
	limits = limits.withDefaults()

	transport, err := transportFor(cfg)
	if err != nil {
		return nil, err
	}

	startCtx, cancel := context.WithTimeout(ctx, limits.StartTimeout)
	defer cancel()

	client := sdk.NewClient(&sdk.Implementation{Name: clientName, Version: clientVersion}, nil)

	session, err := client.Connect(startCtx, transport, nil)
	if err != nil {
		return nil, fmt.Errorf("mcp: connect to %s: %w", cfg.Name, err)
	}

	server := &Server{
		name:      cfg.Name,
		session:   session,
		limits:    limits,
		artifacts: artifacts,
		logger:    logger,
	}

	listed, err := session.ListTools(startCtx, nil)
	if err != nil {
		_ = server.Close()
		return nil, fmt.Errorf("mcp: list tools from %s: %w", cfg.Name, err)
	}

	for _, offered := range listed.Tools {
		adapted, err := server.adapt(cfg, offered)
		if err != nil {
			// One unusable tool is not a reason to lose the rest of the
			// server, but it has to be said out loud: a tool that silently
			// does not appear looks exactly like one the model chose not to
			// use.
			logger.Error("ignoring a tool", "server", cfg.Name, "tool", offered.Name, "error", err)
			continue
		}
		server.tools = append(server.tools, adapted)
	}

	logger.Info("connected to an mcp server",
		"server", cfg.Name, "tools", len(server.tools), "level", cfg.Level.String())

	return server, nil
}

// Name is what this server is called in configuration and in logs.
func (s *Server) Name() string { return s.name }

// Tools are the adapted tools, ready to register.
func (s *Server) Tools() []tool.Tool { return s.tools }

func (s *Server) Close() error {
	var err error
	s.closeOnce.Do(func() { err = s.session.Close() })
	return err
}

func (s *Server) adapt(cfg ServerConfig, offered *sdk.Tool) (tool.Tool, error) {
	name, err := toolName(cfg.Name, offered.Name)
	if err != nil {
		return nil, err
	}

	schema, err := inputSchema(offered)
	if err != nil {
		return nil, err
	}

	return &remoteTool{
		server: s,
		remote: offered.Name,
		spec: tool.Spec{
			Name: name,
			// The server's own description reaches the model unchanged. It is
			// the one thing the server genuinely knows better than we do.
			Description: describe(cfg.Name, offered),
			InputSchema: schema,
			Level:       cfg.Level,
			// Assumed, not asked. What runs behind an MCP call is another
			// program on this machine; claiming it cannot reach the network or
			// cannot destroy anything would be a guess in the direction that
			// costs the most when it is wrong.
			Capabilities: tool.Capabilities{
				ReadFS:      true,
				WriteFS:     true,
				Execute:     true,
				Network:     true,
				Destructive: true,

				// Whatever comes back was written by somebody else's program,
				// which may itself be relaying somebody else's words. Assumed
				// for the same reason as the rest: guessing the other way is
				// what costs when it is wrong.
				ForeignContent: true,
			},
		},
	}, nil
}

// remoteTool is one tool on a connected server, wearing the same interface as
// a built-in one.
//
// That it fits at all is the point of this milestone: if an external tool
// needed a second registry, a second permission path or a second result shape,
// the abstraction would not have been carrying its weight.
type remoteTool struct {
	server *Server
	remote string
	spec   tool.Spec
}

func (t *remoteTool) Spec() tool.Spec { return t.spec }

func (t *remoteTool) Execute(ctx context.Context, call tool.Call) (tool.Result, error) {
	callCtx, cancel := context.WithTimeout(ctx, t.server.limits.CallTimeout)
	defer cancel()

	// The arguments go across as the model produced them. The registry has
	// already validated them against the schema the server published, so
	// re-encoding here would only be a chance to change them.
	arguments := json.RawMessage(call.Arguments)
	if len(arguments) == 0 {
		arguments = json.RawMessage("{}")
	}

	result, err := t.server.session.CallTool(callCtx, &sdk.CallToolParams{
		Name:      t.remote,
		Arguments: arguments,
	})
	if err != nil {
		return tool.Result{}, t.transportFailure(callCtx, err)
	}

	whole := renderContent(result)
	content, truncated := boundText(whole, t.server.limits.MaxOutput)

	// A server that answers with more than fits is answering a question
	// somebody asked; keeping the whole of it and telling the model where is
	// better than deciding on its behalf that the middle did not matter.
	var stored *tool.Artifact
	if truncated {
		ref, err := t.archive(callCtx, whole)
		stored = ref
		content += noteArtifact(ref, err)
	}

	if strings.TrimSpace(content) == "" {
		content = "(the server returned no content)"
	}

	return tool.Result{
		Content:       content,
		Summary:       t.summarise(result),
		IsError:       result.IsError,
		Truncated:     truncated,
		Artifact:      stored,
		OriginalBytes: int64(len(whole)),
	}, nil
}

// transportFailure turns a broken connection into something the model can act
// on rather than an error that ends the run.
//
// A server that has died is a fact about this machine, not a mistake the model
// made; telling it so, and that retrying this tool will not help, is what stops
// it calling the same dead tool until it runs out of iterations.
func (t *remoteTool) transportFailure(ctx context.Context, err error) *tool.Error {
	if ctx.Err() != nil {
		return &tool.Error{
			Code:            tool.CodeTimeout,
			Message:         fmt.Sprintf("%s did not answer within %s", t.spec.Name, t.server.limits.CallTimeout),
			SuggestedAction: "Try a narrower request, or use a different tool.",
			Retryable:       true,
		}
	}

	return &tool.Error{
		Code:            tool.CodeUnsupported,
		Message:         fmt.Sprintf("the %s server is not answering: %v", t.server.name, err),
		SuggestedAction: "This tool is unavailable for the rest of the session; do the work another way.",
	}
}

// archive keeps the whole of an answer the model only saw part of.
func (t *remoteTool) archive(ctx context.Context, content string) (*tool.Artifact, error) {
	if t.server.artifacts == nil {
		return nil, nil
	}

	ref, err := t.server.artifacts.PutBytes(ctx, []byte(content), "text/plain")
	if err != nil {
		return nil, err
	}
	return &tool.Artifact{ID: ref.ID, Size: ref.Size, MediaType: ref.MediaType}, nil
}

// noteArtifact tells the model where the rest went. Saying only that bytes
// were omitted invites it to guess at what they contained.
func noteArtifact(ref *tool.Artifact, err error) string {
	if err != nil {
		return fmt.Sprintf("\n[the rest could not be stored (%v) and is not recoverable]", err)
	}
	if ref == nil {
		return "\n[the rest was not stored and is not recoverable]"
	}
	return fmt.Sprintf("\n[the whole answer is %d bytes; read it with read_artifact on %s]",
		ref.Size, ref.ID)
}

func (t *remoteTool) summarise(result *sdk.CallToolResult) string {
	if result.IsError {
		return t.spec.Name + ": failed"
	}
	return t.spec.Name + ": ok"
}

// renderContent flattens the server's content blocks into text.
//
// Only text survives. Images and embedded resources are named rather than
// carried, because the conversation this feeds is text today and pasting a
// base64 image into it would be a very expensive way to say nothing.
func renderContent(result *sdk.CallToolResult) string {
	var rendered strings.Builder

	for _, block := range result.Content {
		switch content := block.(type) {
		case *sdk.TextContent:
			rendered.WriteString(content.Text)
			rendered.WriteString("\n")
		case *sdk.ImageContent:
			fmt.Fprintf(&rendered, "[an image of type %s, which cannot be shown here]\n", content.MIMEType)
		case *sdk.AudioContent:
			fmt.Fprintf(&rendered, "[audio of type %s, which cannot be shown here]\n", content.MIMEType)
		case *sdk.ResourceLink:
			fmt.Fprintf(&rendered, "[a resource: %s]\n", content.URI)
		default:
			fmt.Fprintf(&rendered, "[unsupported content of type %T]\n", block)
		}
	}

	// A structured result is the same information in a form the model can
	// parse, and some servers send only that.
	if result.StructuredContent != nil && rendered.Len() == 0 {
		if encoded, err := json.Marshal(result.StructuredContent); err == nil {
			rendered.Write(encoded)
		}
	}

	return strings.TrimRight(rendered.String(), "\n")
}

func boundText(text string, maxBytes int) (string, bool) {
	if len(text) <= maxBytes {
		return text, false
	}

	half := maxBytes / 2
	return fmt.Sprintf("%s\n\n[... %d bytes omitted ...]\n\n%s",
		text[:half], len(text)-2*half, text[len(text)-half:]), true
}

func describe(server string, offered *sdk.Tool) string {
	description := strings.TrimSpace(offered.Description)
	if description == "" {
		description = "No description was provided."
	}
	// Saying where a tool came from is not decoration: a model choosing
	// between two tools that do similar things should know one of them is
	// somebody else's server.
	return fmt.Sprintf("%s (from the %s server)", description, server)
}

// inputSchema takes the server's schema through unchanged.
//
// It is what the model is shown and what its arguments are validated against,
// so rewriting it here would be a way for the two to disagree.
func inputSchema(offered *sdk.Tool) (json.RawMessage, error) {
	if offered.InputSchema == nil {
		return json.RawMessage(`{"type":"object"}`), nil
	}

	encoded, err := json.Marshal(offered.InputSchema)
	if err != nil {
		return nil, fmt.Errorf("its input schema cannot be encoded: %w", err)
	}
	return encoded, nil
}

var unsafeInName = regexp.MustCompile(`[^a-zA-Z0-9_]+`)

// toolName builds the name the model calls.
//
// It is prefixed rather than passed through so that installing a server can
// never shadow a built-in tool: read_file has to keep meaning the one that
// respects the workspace boundary.
func toolName(server, remote string) (string, error) {
	name := "mcp_" + sanitise(server) + "_" + sanitise(remote)

	if len(name) > nameLimit {
		// Truncating would invent collisions between tools that differ only in
		// the part that was cut off, and a collision here means calling the
		// wrong tool.
		return "", fmt.Errorf(
			"the name %q is %d characters, past the %d a model can call; shorten the server name",
			name, len(name), nameLimit)
	}
	return name, nil
}

func sanitise(name string) string {
	return strings.Trim(unsafeInName.ReplaceAllString(name, "_"), "_")
}

// environment builds the child's environment.
//
// Nothing is inherited that was not named. The daemon's environment holds the
// provider credentials, and a tool server is exactly the kind of program that
// should not be handed them by default.
func environment(cfg ServerConfig) []string {
	env := make([]string, 0, len(cfg.Env)+len(cfg.PassEnv)+6)

	// Enough for a program to find its own interpreter and a temporary
	// directory. Less than this and most servers cannot start at all.
	for _, name := range []string{"PATH", "HOME", "USERPROFILE", "TMPDIR", "TEMP", "TMP", "SystemRoot", "ComSpec", "PATHEXT"} {
		if value, ok := os.LookupEnv(name); ok {
			env = append(env, name+"="+value)
		}
	}

	for _, name := range cfg.PassEnv {
		if value, ok := os.LookupEnv(name); ok {
			env = append(env, name+"="+value)
		}
	}

	for name, value := range cfg.Env {
		env = append(env, name+"="+value)
	}

	return env
}
