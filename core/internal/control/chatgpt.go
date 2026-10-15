package control

import (
	"context"
	"errors"
	"log/slog"

	"connectrpc.com/connect"
	"google.golang.org/protobuf/types/known/timestamppb"

	controlv1 "github.com/KoukeNeko/JingClaw/core/gen/go/jingclaw/control/v1"
	"github.com/KoukeNeko/JingClaw/core/gen/go/jingclaw/control/v1/controlv1connect"
	"github.com/KoukeNeko/JingClaw/core/internal/chatgptauth"
	"github.com/KoukeNeko/JingClaw/core/internal/provider"
)

// ModelLister is what lists the models the signed-in account can use.
type ModelLister interface {
	Models(ctx context.Context) ([]provider.ModelInfo, error)
}

// ChatGPTServer lets a person sign this deployment in with ChatGPT.
//
// Nothing a reply carries is a token, a code, the issued client id or the host
// id, and nothing here logs an address the browser was redirected to. The one
// URL it returns is for the operator's own browser.
type ChatGPTServer struct {
	auth   *chatgptauth.Auth
	models ModelLister

	// selected is whether the chatgpt backend is the one the settings choose,
	// which is when a change of sign-in matters to the running process.
	selected bool
	logger   *slog.Logger
}

var _ controlv1connect.ChatGPTAuthServiceHandler = (*ChatGPTServer)(nil)

// NewChatGPTServer serves the deployment's ChatGPT sign-in.
func NewChatGPTServer(auth *chatgptauth.Auth, models ModelLister, selected bool, logger *slog.Logger) *ChatGPTServer {
	return &ChatGPTServer{auth: auth, models: models, selected: selected, logger: logger}
}

func (s *ChatGPTServer) GetChatGPTAccount(
	_ context.Context,
	_ *connect.Request[controlv1.GetChatGPTAccountRequest],
) (*connect.Response[controlv1.GetChatGPTAccountResponse], error) {
	account, err := s.auth.Account()
	if err != nil {
		return nil, connect.NewError(connect.CodeFailedPrecondition, err)
	}

	out := &controlv1.GetChatGPTAccountResponse{
		SignedIn:         account.SignedIn,
		Label:            account.Label,
		Email:            account.Email,
		GrantedScopes:    account.Scopes,
		PlanUsageGranted: account.PlanUsageGranted,
		LastErrorCode:    account.LastErrorCode,
		LastErrorMessage: account.LastErrorMessage,
		RestartNeeded:    s.selected && s.auth.Usable() != s.auth.StartedUsable(),
	}
	if !account.ExpiresAt.IsZero() {
		out.AccessTokenExpiresAt = timestamppb.New(account.ExpiresAt)
	}
	return connect.NewResponse(out), nil
}

func (s *ChatGPTServer) StartChatGPTSignIn(
	ctx context.Context,
	_ *connect.Request[controlv1.StartChatGPTSignInRequest],
) (*connect.Response[controlv1.StartChatGPTSignInResponse], error) {
	started, err := s.auth.Start(ctx)
	if err != nil {
		return nil, connect.NewError(connect.CodeFailedPrecondition, err)
	}
	s.logger.Info("chatgpt sign-in started", "redirect_uri", started.RedirectURI)

	return connect.NewResponse(&controlv1.StartChatGPTSignInResponse{
		FlowId:           started.FlowID,
		AuthorizationUrl: started.AuthorizationURL,
		RedirectUri:      started.RedirectURI,
		ExpiresAt:        timestamppb.New(started.ExpiresAt),
	}), nil
}

func (s *ChatGPTServer) GetChatGPTSignIn(
	_ context.Context,
	req *connect.Request[controlv1.GetChatGPTSignInRequest],
) (*connect.Response[controlv1.GetChatGPTSignInResponse], error) {
	state, err := s.auth.Flow(req.Msg.GetFlowId())
	if err != nil {
		return nil, signInError(err)
	}
	return connect.NewResponse(&controlv1.GetChatGPTSignInResponse{
		State: signInState(state.Status), Error: state.Error,
	}), nil
}

func (s *ChatGPTServer) CompleteChatGPTSignIn(
	ctx context.Context,
	req *connect.Request[controlv1.CompleteChatGPTSignInRequest],
) (*connect.Response[controlv1.CompleteChatGPTSignInResponse], error) {
	if err := s.auth.Complete(ctx, req.Msg.GetFlowId(), req.Msg.GetRedirectedAddress()); err != nil {
		return nil, signInError(err)
	}
	s.logger.Info("chatgpt signed in")
	return connect.NewResponse(&controlv1.CompleteChatGPTSignInResponse{
		State: controlv1.ChatGPTSignInState_CHAT_GPT_SIGN_IN_STATE_SUCCEEDED,
	}), nil
}

func (s *ChatGPTServer) SignOutChatGPT(
	ctx context.Context,
	_ *connect.Request[controlv1.SignOutChatGPTRequest],
) (*connect.Response[controlv1.SignOutChatGPTResponse], error) {
	result, err := s.auth.SignOut(ctx)
	if err != nil {
		return nil, connect.NewError(connect.CodeFailedPrecondition, err)
	}
	s.logger.Info("chatgpt signed out", "revocation_confirmed", result.Revoked)
	return connect.NewResponse(&controlv1.SignOutChatGPTResponse{
		Revoked: result.Revoked, Detail: result.Detail,
	}), nil
}

func (s *ChatGPTServer) ListChatGPTModels(
	ctx context.Context,
	_ *connect.Request[controlv1.ListChatGPTModelsRequest],
) (*connect.Response[controlv1.ListChatGPTModelsResponse], error) {
	models, err := s.models.Models(ctx)
	if err != nil {
		return nil, connect.NewError(connect.CodeFailedPrecondition, err)
	}
	out := &controlv1.ListChatGPTModelsResponse{}
	for _, one := range models {
		out.Models = append(out.Models, &controlv1.ChatGPTModel{Id: one.ID, DisplayName: one.DisplayName})
	}
	return connect.NewResponse(out), nil
}

func signInState(status chatgptauth.FlowStatus) controlv1.ChatGPTSignInState {
	switch status {
	case chatgptauth.FlowSucceeded:
		return controlv1.ChatGPTSignInState_CHAT_GPT_SIGN_IN_STATE_SUCCEEDED
	case chatgptauth.FlowFailed:
		return controlv1.ChatGPTSignInState_CHAT_GPT_SIGN_IN_STATE_FAILED
	}
	return controlv1.ChatGPTSignInState_CHAT_GPT_SIGN_IN_STATE_PENDING
}

// signInError maps a sign-in failure to a status. The messages are this
// package's own and never carry the address that was pasted.
func signInError(err error) error {
	switch {
	case errors.Is(err, chatgptauth.ErrUnknownFlow):
		return connect.NewError(connect.CodeNotFound, err)
	case errors.Is(err, chatgptauth.ErrStateMismatch), errors.Is(err, chatgptauth.ErrBadAddress),
		errors.Is(err, chatgptauth.ErrAddressIncomplete), errors.Is(err, chatgptauth.ErrAddressTooLong):
		return connect.NewError(connect.CodeInvalidArgument, err)
	}
	return connect.NewError(connect.CodeFailedPrecondition, err)
}
