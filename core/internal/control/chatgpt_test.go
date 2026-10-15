package control_test

import (
	"bytes"
	"context"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"connectrpc.com/connect"
	"google.golang.org/protobuf/encoding/protojson"

	controlv1 "github.com/KoukeNeko/JingClaw/core/gen/go/jingclaw/control/v1"
	"github.com/KoukeNeko/JingClaw/core/internal/chatgptauth"
	"github.com/KoukeNeko/JingClaw/core/internal/control"
	"github.com/KoukeNeko/JingClaw/core/internal/provider"
)

const (
	signedInAccess  = "access-canary-Qx7Lm2"
	signedInRefresh = "refresh-canary-Vb9Tn4"
	signedInClient  = "oaiapp_canary-client-Zk3Hs8"
	signedInHost    = "urn:uuid:11111111-2222-4333-8444-555555555555"
	pastedCanary    = "pasted-address-canary-Rw6Yp1"
)

type listing struct{ models []provider.ModelInfo }

func (l listing) Models(context.Context) ([]provider.ModelInfo, error) { return l.models, nil }

// aSignedInDeployment writes the file a previous run would have left, with
// tokens nothing else would say, and serves it.
func aSignedInDeployment(t *testing.T, selected bool) (*control.ChatGPTServer, *chatgptauth.Auth, string, *bytes.Buffer) {
	t.Helper()
	path := filepath.Join(t.TempDir(), chatgptauth.FileName)
	file := `{"version":1,"host_id":"` + signedInHost + `","client_id":"` + signedInClient + `",
	  "subject":"sub-1","email":"person@example.test","access_token":"` + signedInAccess + `",
	  "refresh_token":"` + signedInRefresh + `","id_token":"id-token-canary","expires_at":"2099-01-01T00:00:00Z",
	  "scopes":["openid","chatgpt.tokens.use.direct"]}`
	if err := os.WriteFile(path, []byte(file), 0o600); err != nil {
		t.Fatal(err)
	}

	// Nothing answers here, so a sign-out cannot confirm its revocation.
	auth := chatgptauth.New(path, chatgptauth.Endpoints{Auth: "http://127.0.0.1:1", API: "http://127.0.0.1:1/v1"})
	logs := &bytes.Buffer{}
	server := control.NewChatGPTServer(auth,
		listing{[]provider.ModelInfo{{ID: "gpt-a", DisplayName: "GPT A"}}}, selected,
		slog.New(slog.NewTextHandler(logs, nil)))
	return server, auth, path, logs
}

func TestTheChatGPTAccountSaysWhoIsSignedInAndWhatTheyGrantedAndNothingSecret(t *testing.T) {
	server, _, _, logs := aSignedInDeployment(t, true)

	got, err := server.GetChatGPTAccount(t.Context(), connect.NewRequest(&controlv1.GetChatGPTAccountRequest{}))
	if err != nil {
		t.Fatal(err)
	}
	account := got.Msg
	if !account.SignedIn || account.Label != "person@example.test" || !account.PlanUsageGranted ||
		account.AccessTokenExpiresAt == nil || len(account.GrantedScopes) != 2 || account.RestartNeeded {
		t.Errorf("the account is %v", account)
	}

	encoded, _ := protojson.Marshal(account)
	for _, secret := range []string{signedInAccess, signedInRefresh, signedInClient, signedInHost, "id-token-canary"} {
		if strings.Contains(string(encoded), secret) || strings.Contains(logs.String(), secret) {
			t.Errorf("%q is in a reply or a log", secret)
		}
	}
}

func TestStartingASignInHandsBackTheAddressOnlyAndALaterPasteOfGarbageIsRefusedWithoutBeingEchoedOrLogged(t *testing.T) {
	server, auth, _, logs := aSignedInDeployment(t, true)
	t.Cleanup(func() { _, _ = auth.SignOut(context.Background()) })

	started, err := server.StartChatGPTSignIn(t.Context(), connect.NewRequest(&controlv1.StartChatGPTSignInRequest{}))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(started.Msg.RedirectUri, "http://127.0.0.1:") || !strings.HasPrefix(started.Msg.AuthorizationUrl, "http://127.0.0.1:1/api/accounts/authorize?") {
		t.Errorf("the sign-in is %v", started.Msg)
	}

	flow := started.Msg.FlowId
	state, err := server.GetChatGPTSignIn(t.Context(), connect.NewRequest(&controlv1.GetChatGPTSignInRequest{FlowId: flow}))
	if err != nil || state.Msg.State != controlv1.ChatGPTSignInState_CHAT_GPT_SIGN_IN_STATE_PENDING {
		t.Errorf("the flow is %v, %v", state, err)
	}

	_, err = server.CompleteChatGPTSignIn(t.Context(), connect.NewRequest(&controlv1.CompleteChatGPTSignInRequest{
		FlowId: flow, RedirectedAddress: "http://127.0.0.1/auth/callback?code=" + pastedCanary + "&state=wrong",
	}))
	if connect.CodeOf(err) != connect.CodeInvalidArgument {
		t.Errorf("a wrong state gave %v", err)
	}
	_, err = server.CompleteChatGPTSignIn(t.Context(), connect.NewRequest(&controlv1.CompleteChatGPTSignInRequest{
		FlowId: flow, RedirectedAddress: pastedCanary,
	}))
	if connect.CodeOf(err) != connect.CodeInvalidArgument {
		t.Errorf("garbage gave %v", err)
	}
	_, err = server.CompleteChatGPTSignIn(t.Context(), connect.NewRequest(&controlv1.CompleteChatGPTSignInRequest{
		FlowId: "other", RedirectedAddress: "code=x&state=y",
	}))
	if connect.CodeOf(err) != connect.CodeNotFound {
		t.Errorf("an unknown flow gave %v", err)
	}
	if _, err := server.GetChatGPTSignIn(t.Context(), connect.NewRequest(&controlv1.GetChatGPTSignInRequest{FlowId: "other"})); connect.CodeOf(err) != connect.CodeNotFound {
		t.Errorf("an unknown flow gave %v", err)
	}

	if strings.Contains(logs.String(), pastedCanary) || strings.Contains(logs.String(), "state=") || strings.Contains(logs.String(), "code_challenge") {
		t.Errorf("an address or an authorization URL reached the log: %s", logs.String())
	}
}

func TestSigningOutClearsTheFileAndSaysSoWhenRevocationCouldNotBeConfirmedAndAsksForARestart(t *testing.T) {
	server, _, path, logs := aSignedInDeployment(t, true)

	got, err := server.SignOutChatGPT(t.Context(), connect.NewRequest(&controlv1.SignOutChatGPTRequest{}))
	if err != nil {
		t.Fatal(err)
	}
	if got.Msg.Revoked || !strings.Contains(got.Msg.Detail, "ChatGPT Settings") {
		t.Errorf("the reply is %v", got.Msg)
	}

	raw, _ := os.ReadFile(path)
	if strings.Contains(string(raw), signedInAccess) || strings.Contains(string(raw), signedInRefresh) {
		t.Errorf("the file still holds tokens")
	}
	if !strings.Contains(string(raw), signedInClient) {
		t.Errorf("the registration was dropped")
	}
	for _, secret := range []string{signedInAccess, signedInRefresh, signedInClient} {
		if strings.Contains(logs.String(), secret) {
			t.Errorf("%q is in the log", secret)
		}
	}

	account, _ := server.GetChatGPTAccount(t.Context(), connect.NewRequest(&controlv1.GetChatGPTAccountRequest{}))
	if account.Msg.SignedIn || !account.Msg.RestartNeeded {
		t.Errorf("after sign-out the account is %v", account.Msg)
	}
}

func TestARestartIsNotAskedForWhenTheChatGPTBackendIsNotTheOneSelected(t *testing.T) {
	server, _, _, _ := aSignedInDeployment(t, false)
	_, _ = server.SignOutChatGPT(t.Context(), connect.NewRequest(&controlv1.SignOutChatGPTRequest{}))
	account, _ := server.GetChatGPTAccount(t.Context(), connect.NewRequest(&controlv1.GetChatGPTAccountRequest{}))
	if account.Msg.RestartNeeded {
		t.Errorf("a restart is asked for by a backend nobody uses")
	}
}

func TestTheModelsTheAccountCanUseAreListed(t *testing.T) {
	server, _, _, _ := aSignedInDeployment(t, true)
	got, err := server.ListChatGPTModels(t.Context(), connect.NewRequest(&controlv1.ListChatGPTModelsRequest{}))
	if err != nil || len(got.Msg.Models) != 1 || got.Msg.Models[0].Id != "gpt-a" || got.Msg.Models[0].DisplayName != "GPT A" {
		t.Errorf("got %v, %v", got, err)
	}
}
