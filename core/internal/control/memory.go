package control

import (
	"context"
	"errors"

	"connectrpc.com/connect"
	"google.golang.org/protobuf/types/known/timestamppb"

	controlv1 "github.com/KoukeNeko/JingClaw/core/gen/go/jingclaw/control/v1"
	"github.com/KoukeNeko/JingClaw/core/gen/go/jingclaw/control/v1/controlv1connect"
	"github.com/KoukeNeko/JingClaw/core/internal/domain"
	"github.com/KoukeNeko/JingClaw/core/internal/storage"
)

// defaultMemoryLimit keeps a listing readable without hiding anything: the
// limit is the caller's to raise, and the point of this service is that a
// person can see everything.
const defaultMemoryLimit = 200

// MemoryServer lets a person see and remove what the agent believes.
type MemoryServer struct {
	store storage.MemoryStore
}

var _ controlv1connect.MemoryServiceHandler = (*MemoryServer)(nil)

func NewMemoryServer(store storage.MemoryStore) *MemoryServer {
	return &MemoryServer{store: store}
}

func (s *MemoryServer) ListMemories(
	ctx context.Context,
	req *connect.Request[controlv1.ListMemoriesRequest],
) (*connect.Response[controlv1.ListMemoriesResponse], error) {
	limit := int(req.Msg.GetLimit())
	if limit <= 0 {
		limit = defaultMemoryLimit
	}

	// No scope filter: this is the operator's own view of everything the agent
	// believes, including what arrived from outside. Hiding some of it here
	// would defeat the only control that has ever worked.
	found, err := s.store.Memories(ctx, storage.MemoryQuery{
		IncludeInvalidated: req.Msg.GetIncludeInvalidated(),
		Limit:              limit,
	})
	if err != nil {
		return nil, toConnectError(err)
	}

	converted := make([]*controlv1.Memory, 0, len(found))
	for _, memory := range found {
		converted = append(converted, memoryToProto(memory))
	}

	return connect.NewResponse(&controlv1.ListMemoriesResponse{Memories: converted}), nil
}

func (s *MemoryServer) ForgetMemory(
	ctx context.Context,
	req *connect.Request[controlv1.ForgetMemoryRequest],
) (*connect.Response[controlv1.ForgetMemoryResponse], error) {
	if req.Msg.GetId() == "" {
		return nil, connect.NewError(connect.CodeInvalidArgument, errors.New("id is required"))
	}

	if err := s.store.Forget(ctx, domain.MemoryID(req.Msg.GetId())); err != nil {
		if errors.Is(err, storage.ErrMemoryNotFound) {
			return nil, connect.NewError(connect.CodeNotFound, err)
		}
		return nil, toConnectError(err)
	}

	return connect.NewResponse(&controlv1.ForgetMemoryResponse{}), nil
}

func memoryToProto(memory domain.Memory) *controlv1.Memory {
	converted := &controlv1.Memory{
		Id:              string(memory.ID),
		Scope:           string(memory.Scope),
		ScopeRef:        memory.ScopeRef,
		Activation:      string(memory.Activation),
		Text:            memory.Text,
		Trust:           trustToProto(memory.Trust),
		Origin:          originToProto(memory.Origin),
		SourceSessionId: string(memory.SourceSession),
		SourceSeq:       uint64(memory.SourceSeq),
		ApprovedBy:      memory.ApprovedBy,
		CreatedAt:       timestamppb.New(memory.CreatedAt),
		SupersededBy:    string(memory.SupersededBy),
		ValidFrom:       timestamppb.New(memory.ValidFrom),
	}
	if memory.InvalidatedAt != nil {
		converted.InvalidatedAt = timestamppb.New(*memory.InvalidatedAt)
	}
	if memory.ValidUntil != nil {
		converted.ValidUntil = timestamppb.New(*memory.ValidUntil)
	}
	return converted
}
