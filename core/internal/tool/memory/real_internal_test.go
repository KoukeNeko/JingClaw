package memory

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/KoukeNeko/JingClaw/core/internal/domain"
	"github.com/KoukeNeko/JingClaw/core/internal/provider/openaicompat"
	"github.com/KoukeNeko/JingClaw/core/internal/storage"
	"github.com/KoukeNeko/JingClaw/core/internal/storage/sqlite"
)

// Recall measured against a real embedding model, words and meaning together,
// on the corpus TestRecallOnParaphrase uses plus Chinese and cross-language
// questions, and on turns that should find nothing. It skips without one:
//
//	JINGCLAW_REAL_EMBED=http://localhost:11434/v1 \
//	JINGCLAW_REAL_EMBED_MODEL=qwen3-embedding:0.6b \
//	go test ./internal/tool/memory/ -run RealRecall -v
func TestRealRecallWithEmbeddings(t *testing.T) {
	base, model := os.Getenv("JINGCLAW_REAL_EMBED"), os.Getenv("JINGCLAW_REAL_EMBED_MODEL")
	if base == "" || model == "" {
		t.Skip("set JINGCLAW_REAL_EMBED and JINGCLAW_REAL_EMBED_MODEL to run against a real model")
	}
	embedder, err := openaicompat.NewEmbedder(openaicompat.Config{BaseURL: base}, model)
	if err != nil {
		t.Fatal(err)
	}
	store, err := sqlite.Open(context.Background(), filepath.Join(t.TempDir(), "real.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })

	options := Options{Store: store, WorkspaceRef: "/srv/app", Embedder: embedder}
	corpus := map[string]string{
		"deploy":    "the deploy script needs sudo",
		"tests":     "tests are run with go test -race ./...",
		"restart":   "the production cluster must not have its control plane restarted",
		"language":  "answer in Traditional Chinese as used in Taiwan",
		"formatter": "run gofmt before every commit",
		"database":  "migrations live in internal/storage/sqlite/migrations",
		"zh-check":  "部署前一定要先跑 make check",
		"zh-tz":     "他住在台北，時區是 UTC+8",
		"zh-editor": "他平常用 vim 編輯程式碼",
		"zh-friday": "週五不部署到正式環境",
	}
	var written []domain.Memory
	for id, text := range corpus {
		memory := domain.Memory{
			ID: domain.MemoryID("mem_" + id), Scope: domain.ScopeWorkspace, ScopeRef: "/srv/app",
			Activation: domain.MemoryRetrieval, Text: text, Trust: domain.TrustUser,
			CreatedAt: time.Unix(1_700_000_000, 0).UTC(),
		}
		if err := store.Remember(context.Background(), memory, ""); err != nil {
			t.Fatal(err)
		}
		written = append(written, memory)
	}
	if done, err := options.EmbedMissing(context.Background()); err != nil || done != len(written) {
		t.Fatalf("embedded %d of %d: %v", done, len(written), err)
	}

	query := storage.MemoryQuery{Limit: 3}
	leads := func(q, want string) (bool, string) {
		found, err := options.searchAsked(context.Background(), q, query)
		if err != nil {
			t.Fatalf("search %q: %v", q, err)
		}
		return len(found) > 0 && found[0].ID == domain.MemoryID("mem_"+want), fmt.Sprint(texts(found))
	}

	cases := []struct{ query, want string }{
		{"deploy script", "deploy"}, {"sudo", "deploy"}, {"gofmt", "formatter"},
		{"migrations", "database"}, {"deploying", "deploy"}, {"how do I run the tests", "tests"},
		{"can I bounce the kubernetes masters", "restart"}, {"what language should I reply in", "language"},
		{"code style", "formatter"}, {"where do schema changes go", "database"},
		{"用什麼語言回答", "language"},
		{"上線之前要做什麼檢查", "zh-check"}, {"我這邊幾點", "zh-tz"},
		{"我習慣用哪個編輯器", "zh-editor"}, {"禮拜五可以 release 嗎", "zh-friday"},
		{"重啟正式環境的 control plane 可以嗎", "restart"}, {"資料庫 schema 變更放哪", "database"},
	}
	hits := 0
	for _, c := range cases {
		ok, got := leads(c.query, c.want)
		if ok {
			hits++
		} else {
			t.Logf("  missed: %-38q wanted %-10s got %s", c.query, c.want, got)
		}
	}

	unrelated := []string{
		"可以幫我看一下這個錯誤訊息嗎", "幫我寫一個排序的函式", "今天天氣如何",
		"你好，我想問一個問題", "請解釋一下這個設計的優缺點", "幫我把這個檔案重新命名",
		"can you explain what this function does", "please rename this variable",
	}
	// Unasked: what would be put in front of these turns.
	noisy := 0
	for _, q := range unrelated {
		found, err := options.searchUnasked(context.Background(), q, query)
		if err != nil {
			t.Fatal(err)
		}
		if len(found) > 0 {
			noisy++
			t.Logf("  noise:  %-38q got %v", q, texts(found))
		}
	}

	// And the same questions unasked, to see what the stricter line costs.
	unaskedHits := 0
	for _, c := range cases {
		found, err := options.searchUnasked(context.Background(), c.query, query)
		if err != nil {
			t.Fatal(err)
		}
		if len(found) > 0 && found[0].ID == domain.MemoryID("mem_"+c.want) {
			unaskedHits++
		}
	}

	// And by words alone, which is what there was before.
	words := Options{Store: store, WorkspaceRef: "/srv/app"}
	wordHits, wordNoise := 0, 0
	for _, c := range cases {
		found, err := words.searchAsked(context.Background(), c.query, query)
		if err != nil {
			t.Fatal(err)
		}
		if len(found) > 0 && found[0].ID == domain.MemoryID("mem_"+c.want) {
			wordHits++
		}
	}
	for _, q := range unrelated {
		found, err := words.searchUnasked(context.Background(), q, query)
		if err != nil {
			t.Fatal(err)
		}
		if len(found) > 0 {
			wordNoise++
		}
	}

	t.Logf("asked: first result right for %d of %d (words alone: %d)", hits, len(cases), wordHits)
	t.Logf("unasked: %d of %d, and %d of %d unrelated turns had a note put in front of them (words alone: %d)",
		unaskedHits, len(cases), noisy, len(unrelated), wordNoise)

	// Floors, so that a change that makes this worse says so. Set from the
	// measurement on qwen3-embedding:0.6b; another model may need its own.
	if hits <= wordHits {
		t.Errorf("meaning added nothing: %d right against %d by words alone", hits, wordHits)
	}
	if noisy > wordNoise {
		t.Errorf("meaning put notes in front of %d unrelated turns, words alone %d", noisy, wordNoise)
	}
}

func texts(memories []domain.Memory) []string {
	out := make([]string, len(memories))
	for i, memory := range memories {
		out[i] = memory.Text
	}
	return out
}
