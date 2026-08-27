package control_test

import (
	"bytes"
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"connectrpc.com/connect"

	controlv1 "github.com/KoukeNeko/JingClaw/core/gen/go/jingclaw/control/v1"
	"github.com/KoukeNeko/JingClaw/core/gen/go/jingclaw/control/v1/controlv1connect"
	"github.com/KoukeNeko/JingClaw/core/internal/artifact"
	"github.com/KoukeNeko/JingClaw/core/internal/control"
)

func newArtifactClient(t *testing.T) (controlv1connect.ArtifactServiceClient, *artifact.Store) {
	t.Helper()

	store, err := artifact.Open(t.TempDir(), 0)
	if err != nil {
		t.Fatalf("open artifact store: %v", err)
	}

	mux := http.NewServeMux()
	mux.Handle(controlv1connect.NewArtifactServiceHandler(control.NewArtifactServer(store)))

	server := httptest.NewServer(mux)
	t.Cleanup(server.Close)

	return controlv1connect.NewArtifactServiceClient(server.Client(), server.URL), store
}

func readAll(t *testing.T, client controlv1connect.ArtifactServiceClient, id string, offset, limit int64) ([]byte, int64) {
	t.Helper()

	stream, err := client.ReadArtifact(context.Background(),
		connect.NewRequest(&controlv1.ReadArtifactRequest{Id: id, Offset: offset, Limit: limit}))
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	defer func() { _ = stream.Close() }()

	var (
		body  bytes.Buffer
		total int64
	)
	for stream.Receive() {
		body.Write(stream.Msg().GetChunk())
		total = stream.Msg().GetTotalSize()
	}
	if err := stream.Err(); err != nil {
		t.Fatalf("stream: %v", err)
	}

	return body.Bytes(), total
}

// A client showing a build log needs the whole of it, and needs to be able to
// start drawing before the last byte arrives.
func TestAnArtifactStreamsBackWhole(t *testing.T) {
	client, store := newArtifactClient(t)

	// Larger than one chunk, so the streaming is actually exercised rather
	// than being one message that happens to arrive.
	content := strings.Repeat("0123456789abcdef", 40_000)
	ref, err := store.PutBytes(context.Background(), []byte(content), "text/plain")
	if err != nil {
		t.Fatalf("put: %v", err)
	}

	body, total := readAll(t, client, ref.ID, 0, 0)

	if string(body) != content {
		t.Errorf("read back %d bytes, want %d", len(body), len(content))
	}
	if total != int64(len(content)) {
		t.Errorf("the stream reports %d bytes, want %d", total, len(content))
	}
}

func TestAWindowOfAnArtifactCanBeAskedFor(t *testing.T) {
	client, store := newArtifactClient(t)

	ref, err := store.PutBytes(context.Background(), []byte("0123456789"), "text/plain")
	if err != nil {
		t.Fatalf("put: %v", err)
	}

	body, total := readAll(t, client, ref.ID, 3, 4)

	if string(body) != "3456" {
		t.Errorf("the window is %q, want %q", body, "3456")
	}
	// The total is what there is, not what was asked for; a client drawing a
	// progress bar needs the first, not the second.
	if total != 10 {
		t.Errorf("total is %d, want 10", total)
	}
}

func TestStatSaysHowMuchThereIs(t *testing.T) {
	client, store := newArtifactClient(t)

	ref, err := store.PutBytes(context.Background(), []byte("twelve bytes"), "text/plain")
	if err != nil {
		t.Fatalf("put: %v", err)
	}

	found, err := client.StatArtifact(context.Background(),
		connect.NewRequest(&controlv1.StatArtifactRequest{Id: ref.ID}))
	if err != nil {
		t.Fatalf("stat: %v", err)
	}

	if found.Msg.GetSize() != 12 {
		t.Errorf("size is %d, want 12", found.Msg.GetSize())
	}
}

// "You mistyped it" and "it is gone" are different answers, and a client whose
// error message has to guess between them is a client that says the wrong
// thing to somebody.
func TestAMalformedIdAndAMissingOneAreDifferentAnswers(t *testing.T) {
	client, _ := newArtifactClient(t)

	_, err := client.StatArtifact(context.Background(),
		connect.NewRequest(&controlv1.StatArtifactRequest{Id: "../../../etc/passwd"}))
	if connect.CodeOf(err) != connect.CodeInvalidArgument {
		t.Errorf("a path instead of an id gave %s, want invalid_argument", connect.CodeOf(err))
	}

	_, err = client.StatArtifact(context.Background(),
		connect.NewRequest(&controlv1.StatArtifactRequest{
			Id: "sha256-" + strings.Repeat("0", 64),
		}))
	if connect.CodeOf(err) != connect.CodeNotFound {
		t.Errorf("a well-formed id for nothing gave %s, want not_found", connect.CodeOf(err))
	}
}
