package runtime_test

import (
	"context"
	"io"
	"strings"
	"sync"
	"testing"

	"github.com/KoukeNeko/JingClaw/core/internal/domain"
	"github.com/KoukeNeko/JingClaw/core/internal/provider"
)

// orderingProvider holds the first call open until released, and records
// every request it is given, so a test can see the conversation a queued run
// was actually shown when its turn came.
type orderingProvider struct {
	release  chan struct{}
	mu       sync.Mutex
	requests []provider.Request
}

func (p *orderingProvider) Name() string { return "ordering" }
func (p *orderingProvider) Models(context.Context) ([]provider.ModelInfo, error) {
	return []provider.ModelInfo{{ID: "ordering"}}, nil
}
func (p *orderingProvider) Generate(ctx context.Context, req provider.Request) (provider.Stream, error) {
	p.mu.Lock()
	p.requests = append(p.requests, req)
	which := len(p.requests)
	p.mu.Unlock()

	select {
	case <-p.release:
	case <-ctx.Done():
		return nil, ctx.Err()
	}
	return &answerStream{text: "answer " + itoa(uint64(which))}, nil
}

func (p *orderingProvider) request(index int) provider.Request {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.requests[index]
}

type answerStream struct {
	text string
	step int
}

func (s *answerStream) Recv(context.Context) (provider.Event, error) {
	s.step++
	switch s.step {
	case 1:
		return provider.TextDelta{Text: s.text}, nil
	case 2:
		return provider.Completed{StopReason: domain.StopEndTurn}, nil
	default:
		return nil, io.EOF
	}
}
func (s *answerStream) Close() error { return nil }

func lastText(message provider.Message) string {
	var text strings.Builder
	for _, block := range message.Content {
		if t, ok := block.(provider.TextBlock); ok {
			text.WriteString(t.Text)
		}
	}
	return text.String()
}

// A message that waited its turn is answered as the latest thing said.
//
// Its event lands in the log the moment it arrives, while the run before it
// is still writing. Replayed in log order that puts the waiting message in the
// middle of the previous answer, and the conversation the queued run is shown
// ends with the assistant's own last reply — so the model has nothing to
// answer. This is the failure a person sees as "the second message was never
// answered".
func TestAQueuedMessageIsTheLastThingTheModelSees(t *testing.T) {
	model := &orderingProvider{release: make(chan struct{})}
	rt, _ := newQueueRuntime(t, model)
	ctx := context.Background()

	session, err := rt.CreateSession(ctx, "queue order")
	if err != nil {
		t.Fatalf("create: %v", err)
	}

	first, _, err := rt.SendTurn(ctx, session.ID, "first question", domain.RunOrigin{})
	if err != nil {
		t.Fatalf("send first: %v", err)
	}
	// The first run is inside Generate now; the second has to wait for it.
	second, _, err := rt.SendTurn(ctx, session.ID, "second question", domain.RunOrigin{})
	if err != nil {
		t.Fatalf("send second: %v", err)
	}

	model.release <- struct{}{}
	if err := rt.Wait(ctx, first); err != nil {
		t.Fatalf("wait first: %v", err)
	}
	model.release <- struct{}{}
	if err := rt.Wait(ctx, second); err != nil {
		t.Fatalf("wait second: %v", err)
	}

	shown := model.request(1).Messages
	if len(shown) == 0 {
		t.Fatal("the queued run was shown no conversation at all")
	}

	last := shown[len(shown)-1]
	if last.Role != provider.RoleUser || !strings.Contains(lastText(last), "second question") {
		roles := make([]string, 0, len(shown))
		for _, message := range shown {
			roles = append(roles, string(message.Role)+":"+strings.TrimSpace(lastText(message)))
		}
		t.Fatalf("the queued run's own question is not the last thing the model sees:\n  %s",
			strings.Join(roles, "\n  "))
	}

	// And the previous exchange is intact in front of it, in order.
	if len(shown) < 3 {
		t.Fatalf("expected the first exchange before the second question, got %d messages", len(shown))
	}
	if shown[0].Role != provider.RoleUser || !strings.Contains(lastText(shown[0]), "first question") {
		t.Errorf("the first message is not the first question: %q", lastText(shown[0]))
	}
	if shown[1].Role != provider.RoleAssistant || !strings.Contains(lastText(shown[1]), "answer 1") {
		t.Errorf("the first answer does not follow the first question: %q", lastText(shown[1]))
	}
}
