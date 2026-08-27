package control

import (
	"context"
	"errors"
	"fmt"
	"io"

	"connectrpc.com/connect"

	controlv1 "github.com/KoukeNeko/JingClaw/core/gen/go/jingclaw/control/v1"
	"github.com/KoukeNeko/JingClaw/core/gen/go/jingclaw/control/v1/controlv1connect"
	"github.com/KoukeNeko/JingClaw/core/internal/artifact"
)

// chunkSize is what one streamed message carries.
//
// Large enough that a big artifact is not thousands of round trips, small
// enough that a client can draw the first screen before the rest arrives.
const chunkSize = 64 << 10

// ArtifactServer serves stored output to clients.
type ArtifactServer struct {
	store *artifact.Store
}

var _ controlv1connect.ArtifactServiceHandler = (*ArtifactServer)(nil)

func NewArtifactServer(store *artifact.Store) *ArtifactServer {
	return &ArtifactServer{store: store}
}

func (s *ArtifactServer) StatArtifact(
	_ context.Context,
	req *connect.Request[controlv1.StatArtifactRequest],
) (*connect.Response[controlv1.StatArtifactResponse], error) {
	ref, err := s.store.Stat(req.Msg.GetId())
	if err != nil {
		return nil, artifactError(err)
	}

	return connect.NewResponse(&controlv1.StatArtifactResponse{
		Id:   ref.ID,
		Size: ref.Size,
	}), nil
}

func (s *ArtifactServer) ReadArtifact(
	ctx context.Context,
	req *connect.Request[controlv1.ReadArtifactRequest],
	stream *connect.ServerStream[controlv1.ReadArtifactResponse],
) error {
	offset := req.Msg.GetOffset()
	if offset < 0 {
		return connect.NewError(connect.CodeInvalidArgument,
			fmt.Errorf("offset %d is before the beginning", offset))
	}

	ref, err := s.store.Stat(req.Msg.GetId())
	if err != nil {
		return artifactError(err)
	}

	reader, err := s.store.Reader(req.Msg.GetId())
	if err != nil {
		return artifactError(err)
	}
	defer func() { _ = reader.Close() }()

	if _, err := reader.Seek(offset, io.SeekStart); err != nil {
		return connect.NewError(connect.CodeInternal, err)
	}

	// Zero means to the end, which is what a client showing the whole thing
	// wants and saves it having to ask how much there is first.
	remaining := req.Msg.GetLimit()
	if remaining <= 0 {
		remaining = ref.Size - offset
	}

	buffer := make([]byte, chunkSize)
	for remaining > 0 {
		// Abandoned by a client that closed its tab, and the read stops there
		// rather than pushing the rest of a large file into a dead connection.
		if err := ctx.Err(); err != nil {
			return err
		}

		read, err := reader.Read(buffer[:min(int64(len(buffer)), remaining)])
		if read > 0 {
			if err := stream.Send(&controlv1.ReadArtifactResponse{
				Chunk:     buffer[:read],
				TotalSize: ref.Size,
			}); err != nil {
				return err
			}
			remaining -= int64(read)
		}
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return connect.NewError(connect.CodeInternal, err)
		}
	}

	return nil
}

// artifactError keeps the distinction the store makes.
//
// "You mistyped it" and "it is gone" are different answers, and collapsing
// them into one code makes a client's error message a guess.
func artifactError(err error) error {
	switch {
	case errors.Is(err, artifact.ErrBadID):
		return connect.NewError(connect.CodeInvalidArgument, err)
	case errors.Is(err, artifact.ErrNotFound):
		return connect.NewError(connect.CodeNotFound, err)
	default:
		return connect.NewError(connect.CodeInternal, err)
	}
}
