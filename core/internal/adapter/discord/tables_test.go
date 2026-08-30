package discord

import (
	"encoding/json"
	"strings"
	"testing"
	"unicode"

	jcgateway "github.com/KoukeNeko/JingClaw/core/internal/gateway"
	"github.com/KoukeNeko/JingClaw/core/internal/gateway/render"
)

// Rendered against this adapter's own style, not a copy of it in a test.
//
// A copy drifts: the fence, the width budget and the emphasis markers are
// configuration, and a test carrying its own version of them can pass while
// what Discord receives is something else.
func post(t *testing.T, text string) string {
	t.Helper()

	payload, err := json.Marshal(jcgateway.MessagePayload{Text: text})
	if err != nil {
		t.Fatalf("encode: %v", err)
	}

	out, err := render.Dispatch(jcgateway.Dispatch{
		Kind:    jcgateway.DispatchMessage,
		Payload: string(payload),
	}, discordStyle)
	if err != nil {
		t.Fatalf("render: %v", err)
	}
	return out
}

// displayWidth is what "the columns line up" means once the text is Chinese:
// the same number of characters is a different number of columns.
func displayWidth(text string) int {
	total := 0
	for _, r := range text {
		if unicode.Is(unicode.Mn, r) {
			continue
		}
		if r >= 0x1100 && (r <= 0x115F ||
			(r >= 0x2E80 && r <= 0xA4CF) ||
			(r >= 0xAC00 && r <= 0xD7A3) ||
			(r >= 0xF900 && r <= 0xFAFF) ||
			(r >= 0xFE30 && r <= 0xFE6F) ||
			(r >= 0xFF00 && r <= 0xFF60) ||
			(r >= 0xFFE0 && r <= 0xFFE6)) {
			total += 2
			continue
		}
		total++
	}
	return total
}

// Discord renders no table syntax at all, so a delimiter row is the giveaway:
// nobody has ever wanted to read one.
func TestNoTableSyntaxReachesDiscord(t *testing.T) {
	got := post(t, "報告：\n\n"+
		"| 舞台 | 台灣譯名 | 匪區誤用 |\n"+
		"|:---|:---|:---|\n"+
		"| 故事舞台 (Kivotos) | 奇普托斯 | 基沃托斯 |\n"+
		"| 對策委員會 | 白子 (Shiroko) | 砂狼白子 |\n")

	for _, syntax := range []string{"|:---", "|---", "---|"} {
		if strings.Contains(got, syntax) {
			t.Errorf("%q reached the channel:\n%s", syntax, got)
		}
	}
}

// And the columns have to line up once it is a block, or it is a worse
// version of the rows it was made from.
func TestChineseColumnsLineUpOnDiscord(t *testing.T) {
	got := post(t, "| 舞台 | 台灣譯名 |\n|---|---|\n"+
		"| 故事舞台 (Kivotos) | 奇普托斯 |\n"+
		"| 對策委員會 | 白子 |\n")

	if !strings.Contains(got, discordStyle.Fence) {
		t.Fatalf("the table did not become a block:\n%s", got)
	}

	var widths []int
	for _, line := range strings.Split(got, "\n") {
		cut := strings.LastIndex(line, "│")
		if cut < 0 {
			continue
		}
		widths = append(widths, displayWidth(line[:cut]))
	}
	if len(widths) < 3 {
		t.Fatalf("%d aligned rows, want at least 3:\n%s", len(widths), got)
	}
	for _, width := range widths[1:] {
		if width != widths[0] {
			t.Errorf("the columns do not line up (%v):\n%s", widths, got)
			break
		}
	}
}

// The width budget is this adapter's, so a table that exceeds it must stop
// being a grid rather than wrapping in the window.
func TestATableWiderThanTheBudgetIsNotABlock(t *testing.T) {
	wide := strings.Repeat("x", discordStyle.TableColumns)
	got := post(t, "| one | two |\n|---|---|\n| "+wide+" | "+wide+" |\n")

	if strings.Contains(got, discordStyle.Fence) {
		t.Errorf("a table twice the budget was still made a block:\n%s", got)
	}
}
