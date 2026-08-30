package daemon

import (
	"testing"

	"github.com/KoukeNeko/JingClaw/core/internal/config"
	"github.com/KoukeNeko/JingClaw/core/internal/provider"
)

// Where the number came from decides who to believe. A person serving the
// model themselves knows what they gave it; a catalog describes what the
// weights can do, which for a locally served model is often several times what
// the server actually allocated.
func TestWhoIsBelievedAboutTheContextWindow(t *testing.T) {
	tests := []struct {
		name       string
		configured int64
		model      provider.ModelInfo
		want       int64
		wantSource provider.ContextSource
	}{
		{
			name:       "the operator outranks the provider",
			configured: 8192,
			model: provider.ModelInfo{
				ContextWindow: 131072, ContextSource: provider.ContextCatalog,
			},
			want: 8192, wantSource: provider.ContextOperator,
		},
		{
			// The case that matters locally: the server says what it loaded,
			// and that beats what the model could have done.
			name: "a server reporting what it actually loaded",
			model: provider.ModelInfo{
				ContextWindow:  4096,
				TrainedContext: 131072,
				ContextSource:  provider.ContextRuntime,
			},
			want: 4096, wantSource: provider.ContextRuntime,
		},
		{
			name:  "a provider catalog when nothing better exists",
			model: provider.ModelInfo{ContextWindow: 32768, ContextSource: provider.ContextCatalog},
			want:  32768, wantSource: provider.ContextCatalog,
		},
		{
			// Compaction against an invented window either throws history
			// away early or fails to save the session that needed saving.
			name:       "nobody knew",
			model:      provider.ModelInfo{},
			want:       0,
			wantSource: provider.ContextUnknown,
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			cfg := config.Defaults()
			cfg.Context.Window = test.configured

			window, source := contextWindow(cfg, test.model)
			if window != test.want {
				t.Errorf("window is %d, want %d", window, test.want)
			}
			if source != test.wantSource {
				t.Errorf("source is %q, want %q", source, test.wantSource)
			}
		})
	}
}
