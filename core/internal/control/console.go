package control

import (
	"context"
	"errors"
	"sort"

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
	grants  *Grants
	baseURL string
}

var _ controlv1connect.ConsoleServiceHandler = (*ConsoleServer)(nil)

func NewConsoleServer(pairing *Pairing, grants *Grants, baseURL string) *ConsoleServer {
	return &ConsoleServer{pairing: pairing, grants: grants, baseURL: baseURL}
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

// ListConsoleGrants is which browsers can currently reach this agent.
func (s *ConsoleServer) ListConsoleGrants(
	_ context.Context,
	_ *connect.Request[controlv1.ListConsoleGrantsRequest],
) (*connect.Response[controlv1.ListConsoleGrantsResponse], error) {
	listed := s.grants.List()

	// Newest first: the one somebody just paired is the one they are most
	// likely to be looking for, and the oldest is the one most likely to be
	// forgotten rather than wanted.
	sort.Slice(listed, func(i, j int) bool {
		return listed[i].IssuedAt.After(listed[j].IssuedAt)
	})

	out := make([]*controlv1.ConsoleGrant, 0, len(listed))
	for _, one := range listed {
		out = append(out, &controlv1.ConsoleGrant{
			Id:       one.ID,
			PairedAt: timestamppb.New(one.IssuedAt),
			LastUsed: timestamppb.New(one.LastUsed),
			Label:    one.Label,
		})
	}

	return connect.NewResponse(&controlv1.ListConsoleGrantsResponse{Grants: out}), nil
}

// RevokeConsoleGrant ends one console session, or every one.
func (s *ConsoleServer) RevokeConsoleGrant(
	_ context.Context,
	req *connect.Request[controlv1.RevokeConsoleGrantRequest],
) (*connect.Response[controlv1.RevokeConsoleGrantResponse], error) {
	if req.Msg.GetAll() {
		return connect.NewResponse(&controlv1.RevokeConsoleGrantResponse{
			Revoked: int32(s.grants.RevokeAll()),
		}), nil
	}

	id := req.Msg.GetGrantId()
	if id == "" {
		return nil, connect.NewError(connect.CodeInvalidArgument,
			errors.New("name a grant to revoke, or set all"))
	}

	revoked := 0
	if s.grants.Revoke(id) {
		revoked = 1
	}
	// Zero is answered rather than treated as an error: an id that names
	// nothing may be one that expired a minute ago, and "it is not there" is
	// what the caller wanted either way. Reporting the count is what stops it
	// reading as success.
	return connect.NewResponse(&controlv1.RevokeConsoleGrantResponse{
		Revoked: int32(revoked),
	}), nil
}
