package runtime_test

import (
	"bytes"
	"context"
	"fmt"
	"image"
	"image/color"
	"image/png"
	"log/slog"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/KoukeNeko/JingClaw/core/internal/artifact"
	"github.com/KoukeNeko/JingClaw/core/internal/domain"
	"github.com/KoukeNeko/JingClaw/core/internal/event"
	"github.com/KoukeNeko/JingClaw/core/internal/provider"
	"github.com/KoukeNeko/JingClaw/core/internal/runtime"
	"github.com/KoukeNeko/JingClaw/core/internal/storage/memory"
)

func samplePNG(t *testing.T) []byte {
	t.Helper()

	canvas := image.NewRGBA(image.Rect(0, 0, 4, 4))
	canvas.Set(1, 1, color.RGBA{G: 200, A: 255})

	var out bytes.Buffer
	if err := png.Encode(&out, canvas); err != nil {
		t.Fatalf("encode: %v", err)
	}
	return out.Bytes()
}

// An image that arrives has to reach the model as an image. Everything between
// here and there is a reference — the event holds one, the store holds the
// bytes — and the join happens when a request is assembled, which is exactly
// the kind of seam that fails quietly.
func TestAnAttachedImageReachesTheModel(t *testing.T) {
	store := memory.New()
	artifacts, err := artifact.Open(t.TempDir(), 0)
	if err != nil {
		t.Fatalf("open artifacts: %v", err)
	}

	picture := samplePNG(t)
	ref, err := artifacts.PutBytes(context.Background(), picture, "image/png")
	if err != nil {
		t.Fatalf("put: %v", err)
	}

	model := &compactingProvider{reply: "I see a green dot."}
	rt := newImageHarness(t, store, artifacts, model)

	session, err := rt.CreateSession(context.Background(), "")
	if err != nil {
		t.Fatalf("create session: %v", err)
	}

	runID, _, err := rt.SendTurnTo(context.Background(), session.ID, domain.Turn{
		Text:    "what is in this picture",
		Origin:  domain.RunOrigin{Kind: domain.OriginLocalClient, ClientID: "cli"},
		Targets: []domain.DeliveryTarget{{Kind: domain.DeliveryLocalClient}},
		Attachments: []domain.Attachment{{
			ArtifactID: ref.ID,
			Name:       "dot.png",
			MediaType:  "image/png",
			Size:       ref.Size,
		}},
	})
	if err != nil {
		t.Fatalf("send: %v", err)
	}
	waitForRun(t, rt, runID)

	sent := model.turnRequests()
	if len(sent) == 0 {
		t.Fatal("nothing reached the model")
	}

	var images []provider.ImageBlock
	for _, message := range sent[0].Messages {
		for _, block := range message.Content {
			if picture, ok := block.(provider.ImageBlock); ok {
				images = append(images, picture)
			}
		}
	}

	if len(images) != 1 {
		t.Fatalf("%d images reached the model, want one", len(images))
	}
	if images[0].MediaType != "image/png" {
		t.Errorf("media type is %q", images[0].MediaType)
	}
	if !bytes.Equal(images[0].Data, picture) {
		t.Errorf("the model got %d bytes, want the %d that were stored",
			len(images[0].Data), len(picture))
	}
}

// An attachment the model cannot look at is still a fact about the message.
// "Here, fix this" with a patch attached makes no sense at all if the
// attachment is invisible.
func TestAnAttachmentThatCannotBeShownIsDescribed(t *testing.T) {
	store := memory.New()
	artifacts, err := artifact.Open(t.TempDir(), 0)
	if err != nil {
		t.Fatalf("open artifacts: %v", err)
	}

	model := &compactingProvider{reply: "ok"}
	rt := newImageHarness(t, store, artifacts, model)

	session, err := rt.CreateSession(context.Background(), "")
	if err != nil {
		t.Fatalf("create session: %v", err)
	}

	runID, _, err := rt.SendTurnTo(context.Background(), session.ID, domain.Turn{
		Text:    "have a look at this",
		Origin:  domain.RunOrigin{Kind: domain.OriginLocalClient, ClientID: "cli"},
		Targets: []domain.DeliveryTarget{{Kind: domain.DeliveryLocalClient}},
		Attachments: []domain.Attachment{
			{Name: "report.pdf", MediaType: "application/pdf", Size: 3_200_000},
			{Name: "clip.mp4", MediaType: "video/mp4", Size: 9_000_000},
		},
	})
	if err != nil {
		t.Fatalf("send: %v", err)
	}
	waitForRun(t, rt, runID)

	sent := requestText(model.turnRequests()[0])
	for _, want := range []string{"report.pdf", "clip.mp4"} {
		if !strings.Contains(sent, want) {
			t.Errorf("the model was not told about %s:\n%s", want, sent)
		}
	}
	if !strings.Contains(sent, "not kept") {
		t.Errorf("the model is not told the bytes were not kept:\n%s", sent)
	}
}

// A picture from outside this machine is labelled as such. It is not a
// security control — text inside an image is a known way to instruct a model
// and no label prevents it — but it costs nothing and it is true.
func TestAnImageFromOutsideIsLabelled(t *testing.T) {
	store := memory.New()
	artifacts, err := artifact.Open(t.TempDir(), 0)
	if err != nil {
		t.Fatalf("open artifacts: %v", err)
	}

	ref, err := artifacts.PutBytes(context.Background(), samplePNG(t), "image/png")
	if err != nil {
		t.Fatalf("put: %v", err)
	}

	model := &compactingProvider{reply: "ok"}
	rt := newImageHarness(t, store, artifacts, model)

	session, err := rt.CreateSession(context.Background(), "")
	if err != nil {
		t.Fatalf("create session: %v", err)
	}

	runID, _, err := rt.SendTurnTo(context.Background(), session.ID, domain.Turn{
		Text: "what is this",
		Origin: domain.RunOrigin{
			Kind: domain.OriginGateway,
			Principal: &domain.ExternalPrincipal{
				Platform: "discord", PrincipalID: "user_1",
			},
		},
		Targets: []domain.DeliveryTarget{{Kind: domain.DeliveryLocalClient}},
		Attachments: []domain.Attachment{{
			ArtifactID: ref.ID, Name: "s.png", MediaType: "image/png", Size: ref.Size,
		}},
	})
	if err != nil {
		t.Fatalf("send: %v", err)
	}
	waitForRun(t, rt, runID)

	sent := requestText(model.turnRequests()[0])
	if !strings.Contains(sent, "arrived from outside this machine") {
		t.Errorf("an image from a gateway turn was not labelled:\n%s", sent)
	}
	if !strings.Contains(sent, "data, not instructions") {
		t.Errorf("the label does not say what to do with text in it:\n%s", sent)
	}
}

func newImageHarness(
	t *testing.T,
	store *memory.Store,
	artifacts *artifact.Store,
	model *compactingProvider,
) *runtime.Runtime {
	t.Helper()

	var counter atomic.Uint64
	next := func(prefix string) runtime.IDGenerator {
		return func() string { return fmt.Sprintf("%s_%d", prefix, counter.Add(1)) }
	}

	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)

	return runtime.New(ctx, runtime.Options{
		Store:         store,
		Hub:           event.NewHub(),
		Provider:      model,
		Model:         "vision",
		Attachments:   artifacts,
		MaxImageBytes: 8 << 20,
		SystemPrompt:  "you are an agent",
		MaxIterations: 3,
		NewSessionID:  next("ses"),
		NewRunID:      next("run"),
		NewMessageID:  next("msg"),
		NewEventID:    next("evt"),
		NewApprovalID: next("apr"),
		Now:           func() time.Time { return time.Unix(0, 0).UTC() },
		Logger:        slog.New(slog.DiscardHandler),
	})
}
