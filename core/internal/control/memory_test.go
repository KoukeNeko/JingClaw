package control_test

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"connectrpc.com/connect"

	controlv1 "github.com/KoukeNeko/JingClaw/core/gen/go/jingclaw/control/v1"
	"github.com/KoukeNeko/JingClaw/core/gen/go/jingclaw/control/v1/controlv1connect"
	"github.com/KoukeNeko/JingClaw/core/internal/control"
	"github.com/KoukeNeko/JingClaw/core/internal/domain"
	"github.com/KoukeNeko/JingClaw/core/internal/storage/memory"
)

func newMemoryClient(t *testing.T) (controlv1connect.MemoryServiceClient, *memory.Store) {
	t.Helper()

	store := memory.New()

	mux := http.NewServeMux()
	mux.Handle(controlv1connect.NewMemoryServiceHandler(control.NewMemoryServer(store)))

	server := httptest.NewServer(mux)
	t.Cleanup(server.Close)

	return controlv1connect.NewMemoryServiceClient(server.Client(), server.URL), store
}

func storeMemory(t *testing.T, store *memory.Store, id, text string, origin domain.RunOrigin) {
	t.Helper()

	err := store.Remember(context.Background(), domain.Memory{
		ID:            domain.MemoryID(id),
		Scope:         domain.ScopeWorkspace,
		ScopeRef:      "/srv/app",
		Kind:          domain.MemoryFact,
		Text:          text,
		Trust:         domain.TrustUser,
		Origin:        origin,
		SourceSession: "ses_1",
		SourceSeq:     7,
		ApprovedBy:    "operator",
		CreatedAt:     time.Unix(1_700_000_000, 0).UTC(),
	}, "")
	if err != nil {
		t.Fatalf("remember: %v", err)
	}
}

// The operator's view is of everything, including what arrived from outside.
// Hiding some of it here would defeat the only control that has ever worked
// against a poisoned memory.
func TestTheListingShowsEverythingAndWhereItCameFrom(t *testing.T) {
	client, store := newMemoryClient(t)

	storeMemory(t, store, "mem_1", "the project uses buf",
		domain.RunOrigin{Kind: domain.OriginLocalClient, ClientID: "jingclaw-cli"})
	storeMemory(t, store, "mem_2", "somebody on Discord said this",
		domain.RunOrigin{
			Kind: domain.OriginGateway,
			Principal: &domain.ExternalPrincipal{
				Platform: "discord", PrincipalID: "user_1",
			},
		})

	resp, err := client.ListMemories(context.Background(),
		connect.NewRequest(&controlv1.ListMemoriesRequest{}))
	if err != nil {
		t.Fatalf("list: %v", err)
	}

	if len(resp.Msg.GetMemories()) != 2 {
		t.Fatalf("the operator sees %d of 2 memories", len(resp.Msg.GetMemories()))
	}

	byID := map[string]*controlv1.Memory{}
	for _, memory := range resp.Msg.GetMemories() {
		byID[memory.GetId()] = memory
	}

	fromOutside := byID["mem_2"]
	if fromOutside == nil {
		t.Fatal("the memory that arrived from outside is not shown")
	}
	if fromOutside.GetOrigin().GetKind() != controlv1.RunOriginKind_RUN_ORIGIN_KIND_GATEWAY {
		t.Error("the listing does not say it came from outside")
	}
	if fromOutside.GetOrigin().GetPrincipal().GetPrincipalId() != "user_1" {
		t.Error("the listing does not say who caused it")
	}
	if fromOutside.GetSourceSessionId() != "ses_1" || fromOutside.GetSourceSeq() != 7 {
		t.Error("the listing cannot say where in the log it came from")
	}
	if fromOutside.GetApprovedBy() != "operator" {
		t.Error("the listing does not say who let it in")
	}
}

// Superseded memories are hidden by default and available on request: what is
// believed now and what changed are different questions.
func TestSupersededMemoriesAreShownOnlyWhenAsked(t *testing.T) {
	client, store := newMemoryClient(t)

	origin := domain.RunOrigin{Kind: domain.OriginLocalClient, ClientID: "jingclaw-cli"}
	storeMemory(t, store, "mem_1", "the API is at example.com", origin)

	corrected := domain.Memory{
		ID: "mem_2", Scope: domain.ScopeWorkspace, ScopeRef: "/srv/app",
		Kind: domain.MemoryFact, Text: "the API is at example.net",
		Trust: domain.TrustUser, Origin: origin,
		SourceSession: "ses_1", SourceSeq: 9, ApprovedBy: "operator",
		CreatedAt: time.Unix(1_700_003_600, 0).UTC(),
	}
	if err := store.Remember(context.Background(), corrected, "mem_1"); err != nil {
		t.Fatalf("correct: %v", err)
	}

	current, err := client.ListMemories(context.Background(),
		connect.NewRequest(&controlv1.ListMemoriesRequest{}))
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(current.Msg.GetMemories()) != 1 {
		t.Errorf("what is believed now has %d entries", len(current.Msg.GetMemories()))
	}

	history, err := client.ListMemories(context.Background(),
		connect.NewRequest(&controlv1.ListMemoriesRequest{IncludeInvalidated: true}))
	if err != nil {
		t.Fatalf("list with history: %v", err)
	}
	if len(history.Msg.GetMemories()) != 2 {
		t.Fatalf("the history has %d entries", len(history.Msg.GetMemories()))
	}

	for _, memory := range history.Msg.GetMemories() {
		if memory.GetId() == "mem_1" {
			if memory.GetInvalidatedAt() == nil {
				t.Error("the superseded memory is not marked")
			}
			if memory.GetSupersededBy() != "mem_2" {
				t.Error("the superseded memory does not say what replaced it")
			}
		}
	}
}

// Somebody who asks the agent to forget something has to be answered by it
// actually being gone.
func TestForgettingRemovesIt(t *testing.T) {
	client, store := newMemoryClient(t)

	storeMemory(t, store, "mem_1", "something regrettable",
		domain.RunOrigin{Kind: domain.OriginLocalClient})

	if _, err := client.ForgetMemory(context.Background(),
		connect.NewRequest(&controlv1.ForgetMemoryRequest{Id: "mem_1"})); err != nil {
		t.Fatalf("forget: %v", err)
	}

	history, err := client.ListMemories(context.Background(),
		connect.NewRequest(&controlv1.ListMemoriesRequest{IncludeInvalidated: true}))
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(history.Msg.GetMemories()) != 0 {
		t.Errorf("a forgotten memory is still listed: %+v", history.Msg.GetMemories())
	}

	// And forgetting nothing says so rather than reporting success.
	_, err = client.ForgetMemory(context.Background(),
		connect.NewRequest(&controlv1.ForgetMemoryRequest{Id: "mem_absent"}))
	if connect.CodeOf(err) != connect.CodeNotFound {
		t.Errorf("forgetting nothing gave %s", connect.CodeOf(err))
	}

	_, err = client.ForgetMemory(context.Background(),
		connect.NewRequest(&controlv1.ForgetMemoryRequest{}))
	if connect.CodeOf(err) != connect.CodeInvalidArgument {
		t.Errorf("forgetting without an id gave %s", connect.CodeOf(err))
	}
}

// Seeing and removing what the agent believes is the operator's own business.
// A page somebody was let into, or a chat gateway, must not reach it.
func TestOnlyTheOperatorReachesMemory(t *testing.T) {
	for scope, want := range map[control.Scope]int{
		control.ScopeControl: http.StatusOK,
		control.ScopeConsole: http.StatusForbidden,
		control.ScopeGateway: http.StatusForbidden,
	} {
		token, err := control.NewToken(scope)
		if err != nil {
			t.Fatalf("token: %v", err)
		}

		guarded := control.AuthMiddleware([]control.Token{token}, "7777",
			http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				w.WriteHeader(http.StatusOK)
			}))

		request := httptest.NewRequest(http.MethodPost,
			"/jingclaw.control.v1.MemoryService/ListMemories", nil)
		request.Host = "127.0.0.1:7777"
		request.Header.Set("Authorization", "Bearer "+token.Value)

		recorder := httptest.NewRecorder()
		guarded.ServeHTTP(recorder, request)

		if recorder.Code != want {
			t.Errorf("%s reached memory with %d, want %d", scope, recorder.Code, want)
		}
	}
}
