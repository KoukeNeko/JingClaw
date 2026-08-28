package control

import (
	"context"

	"connectrpc.com/connect"
	"google.golang.org/protobuf/types/known/timestamppb"

	controlv1 "github.com/KoukeNeko/JingClaw/core/gen/go/jingclaw/control/v1"
	"github.com/KoukeNeko/JingClaw/core/gen/go/jingclaw/control/v1/controlv1connect"
)

// ConsoleServer issues pairing codes.
//
// Reachable by the operator's credential and not by the console's, which is
// the point of the scopes: a page that was let in must not be able to let
// anything else in.
type ConsoleServer struct {
	pairing *Pairing
	baseURL string
}

var _ controlv1connect.ConsoleServiceHandler = (*ConsoleServer)(nil)

func NewConsoleServer(pairing *Pairing, baseURL string) *ConsoleServer {
	return &ConsoleServer{pairing: pairing, baseURL: baseURL}
}

func (s *ConsoleServer) IssuePairingCode(
	_ context.Context,
	_ *connect.Request[controlv1.IssuePairingCodeRequest],
) (*connect.Response[controlv1.IssuePairingCodeResponse], error) {
	code, expires, err := s.pairing.Issue()
	if err != nil {
		return nil, connect.NewError(connect.CodeInternal, err)
	}

	return connect.NewResponse(&controlv1.IssuePairingCodeResponse{
		Code:      code,
		Url:       ConsoleURL(s.baseURL, code),
		ExpiresAt: timestamppb.New(expires),
	}), nil
}

// ConsoleURL is the address to open, with the code in it.
//
// Built in one place so the daemon's banner and the CLI cannot disagree about
// what a person should be looking at.
func ConsoleURL(baseURL, code string) string {
	return baseURL + "/?c=" + code
}
