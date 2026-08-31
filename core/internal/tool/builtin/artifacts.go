package builtin

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"github.com/KoukeNeko/JingClaw/core/internal/domain"

	"github.com/KoukeNeko/JingClaw/core/internal/artifact"
	"github.com/KoukeNeko/JingClaw/core/internal/tool"
)

// archive stores the whole of something a tool could only show part of.
//
// Truncation without this is destruction: the model is told there was more and
// given no way to reach it. A failure to store is returned rather than
// swallowed, so the tool can say the rest is gone instead of implying it is
// one call away.
func archive(
	ctx context.Context,
	store *artifact.Store,
	content []byte,
	mediaType string,
) (*tool.Artifact, error) {
	if store == nil {
		return nil, nil
	}

	// Stored with a context that outlives the call being cancelled would be
	// wrong: abandoning a run should abandon the copy too.
	ref, err := store.PutBytes(ctx, content, mediaType)
	if err != nil {
		return nil, err
	}

	return &tool.Artifact{ID: ref.ID, Size: ref.Size, MediaType: ref.MediaType}, nil
}

// noteArtifact tells the model where the rest of the output went.
//
// Saying only that bytes were omitted invites the model to guess at what they
// contained. Saying how to read them turns a truncated result into a starting
// point.
func noteArtifact(ref *tool.Artifact, err error) string {
	if err != nil {
		return fmt.Sprintf("\n[the rest could not be stored (%v) and is not recoverable]", err)
	}
	if ref == nil {
		return "\n[the rest was not stored and is not recoverable]"
	}
	return fmt.Sprintf("\n[the whole output is %d bytes; read it with read_artifact on %s]",
		ref.Size, ref.ID)
}

// ReadArtifact reads stored output that was too large to show in full.
type ReadArtifact struct {
	Artifacts *artifact.Store

	Limits Limits
}

func (t *ReadArtifact) Spec() tool.Spec {
	return tool.Spec{
		Name: "read_artifact",
		Description: "Read stored output that another tool could only show part of. " +
			"Tools that truncate give an artifact id; this returns a window of the whole thing. " +
			"Use offset to page through it.",
		InputSchema: json.RawMessage(`{
  "type": "object",
  "properties": {
    "artifact_id": {
      "type": "string",
      "minLength": 1,
      "description": "The id another tool reported, of the form sha256-<hex>."
    },
    "offset": {
      "type": "integer",
      "minimum": 0,
      "description": "Byte to start at. Defaults to the beginning."
    },
    "limit": {
      "type": "integer",
      "minimum": 1,
      "description": "How many bytes to return. Defaults to the same amount read_file returns."
    }
  },
  "required": ["artifact_id"],
  "additionalProperties": false
}`),
		// It reads back what this agent already produced and was already
		// shown part of, so it grants no reach it did not have.
		Level: tool.LevelInternal,
		Capabilities: tool.Capabilities{
			// Whatever some earlier tool stored, and the store does not
			// record which. web_read puts pages here, so the honest floor is
			// the least of the three — the same rule the runtime applies to a
			// tool it cannot identify.
			//
			// Better would be for an artifact to carry the provenance of
			// whatever produced it, so reading back a git diff were not
			// treated as reading back a stranger's page. That is a change to
			// the artifact store, and this is the safe answer until then.
			Provenance:   domain.ProvenanceExternal,
			Idempotent:   true,
			ParallelSafe: true,
		},
	}
}

type readArtifactArgs struct {
	ArtifactID string `json:"artifact_id"`
	Offset     int64  `json:"offset"`
	Limit      int64  `json:"limit"`
}

func (t *ReadArtifact) Execute(_ context.Context, call tool.Call) (tool.Result, error) {
	var args readArtifactArgs
	if err := json.Unmarshal(call.Arguments, &args); err != nil {
		return tool.Result{}, tool.Errorf(tool.CodeInvalidArguments, "", "%v", err)
	}

	if t.Artifacts == nil {
		return tool.Result{}, tool.Errorf(tool.CodeUnsupported,
			"Nothing was stored, so there is nothing to read back.",
			"this agent is running without an artifact store")
	}

	limit := args.Limit
	if limit <= 0 {
		limit = t.Limits.withDefaults().ReadLimit
	}

	window, total, err := t.Artifacts.ReadRange(args.ArtifactID, args.Offset, limit)
	if err != nil {
		return tool.Result{}, artifactError(args.ArtifactID, err)
	}

	if len(window) == 0 {
		return tool.Result{
			Content: fmt.Sprintf("%s is %d bytes; offset %d is past the end.",
				args.ArtifactID, total, args.Offset),
			Summary: fmt.Sprintf("read_artifact: past the end of %d bytes", total),
		}, nil
	}

	end := args.Offset + int64(len(window))
	header := fmt.Sprintf("bytes %d-%d of %d\n", args.Offset, end, total)

	// Saying what is left is what lets a model decide whether to keep paging
	// rather than stopping because it cannot tell.
	footer := ""
	if end < total {
		footer = fmt.Sprintf("\n[%d bytes remain; read again from offset %d]", total-end, end)
	}

	return tool.Result{
		Content:       header + string(window) + footer,
		Summary:       fmt.Sprintf("read_artifact: %d of %d bytes", len(window), total),
		Truncated:     end < total,
		OriginalBytes: total,
	}, nil
}

// artifactError turns a store failure into something the model can act on.
//
// "You mistyped it" and "it is gone" are different problems with different
// next steps, so they reach the model as different codes.
func artifactError(id string, err error) *tool.Error {
	switch {
	case errors.Is(err, artifact.ErrBadID):
		return tool.Errorf(tool.CodeInvalidArguments,
			"Use the id exactly as the tool that produced it reported.",
			"%q is not an artifact id", id)
	case errors.Is(err, artifact.ErrNotFound):
		return tool.Errorf(tool.CodeNotFound,
			"It may have been produced by a different agent, or removed.",
			"there is no artifact %s", id)
	default:
		return tool.Errorf(tool.CodeInternal, "", "could not read %s: %v", id, err)
	}
}
