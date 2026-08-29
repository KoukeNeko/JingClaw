package render

import (
	"encoding/json"
	"fmt"
	"strings"
	"testing"

	jcgateway "github.com/KoukeNeko/JingClaw/core/internal/gateway"
)

// discordStyle is Discord's, deliberately: these assertions were written
// against Discord's output, and keeping them unchanged is what shows that
// moving the renderer out of that adapter changed nothing a reader would see.
var discordStyle = Style{
	MaxLength:     2000,
	SoftLength:    1900,
	SubduedPrefix: "-# ",
	Bold:          "**",
	Italic:        "_",
	Fence:         "```",
}

func head(text string, n int) string {
	if len(text) <= n {
		return text
	}
	return text[:n]
}

func tail(text string, n int) string {
	if len(text) <= n {
		return text
	}
	return text[len(text)-n:]
}

func TestShortTextIsPostedWhole(t *testing.T) {
	segments := Split("a short answer", discordStyle)

	if len(segments) != 1 || segments[0] != "a short answer" {
		t.Errorf("got %d segments: %q", len(segments), segments)
	}
}

func TestEverySegmentFitsDiscordsLimit(t *testing.T) {
	long := strings.Repeat("This is a sentence of a plausible length. ", 300)

	for i, segment := range Split(long, discordStyle) {
		if len(segment) > discordStyle.MaxLength {
			t.Errorf("segment %d is %d bytes, over the %d limit", i, len(segment), discordStyle.MaxLength)
		}
	}
}

// Nothing may be dropped: the reader is entitled to the whole answer.
func TestSplittingLosesNoWords(t *testing.T) {
	var builder strings.Builder
	for i := range 400 {
		builder.WriteString("word")
		builder.WriteString(string(rune('a' + i%26)))
		builder.WriteString(" ")
	}
	original := builder.String()

	rejoined := strings.Join(Split(original, discordStyle), " ")

	for _, word := range strings.Fields(original) {
		if !strings.Contains(rejoined, word) {
			t.Fatalf("splitting lost %q", word)
			return
		}
	}
}

// A code fence broken across a boundary would render one half as code and the
// other as prose.
func TestCodeFencesAreClosedAndReopened(t *testing.T) {
	var builder strings.Builder
	builder.WriteString("Here is the change:\n\n```go\n")
	for range 200 {
		builder.WriteString("\tfmt.Println(\"a line of code that is fairly long\")\n")
	}
	builder.WriteString("```\n")

	segments := Split(builder.String(), discordStyle)
	if len(segments) < 2 {
		t.Fatalf("expected the sample to split; got %d segment(s)", len(segments))
	}

	for i, segment := range segments {
		if strings.Count(segment, "```")%2 != 0 {
			t.Errorf("segment %d leaves a fence open:\n%s", i, tail(segment, 120))
		}
	}

	// The continuation has to reopen with the same language, or the second
	// half loses its highlighting.
	if !strings.HasPrefix(segments[1], "```go") {
		t.Errorf("the continuation does not reopen the fence: %q", head(segments[1], 40))
	}
}

func TestBreaksPreferParagraphThenLine(t *testing.T) {
	paragraph := strings.Repeat("word ", 300)
	text := paragraph + "\n\n" + paragraph + "\n\n" + paragraph

	segments := Split(text, discordStyle)
	if len(segments) < 2 {
		t.Fatalf("expected a split; got %d segment(s)", len(segments))
	}

	// A break at a paragraph boundary leaves no partial word at the seam.
	first := segments[0]
	if strings.HasSuffix(first, "wor") || strings.HasSuffix(first, "wo") {
		t.Errorf("the split cut a word in half: %q", tail(first, 40))
	}
}

// An approval message must say what will happen and where the decision is
// actually made.
func TestApprovalRendersTheActionAndWhereToDecide(t *testing.T) {
	rendered := renderApproval(jcgateway.ApprovalPayload{
		ApprovalID: "apr_123",
		ToolName:   "write_file",
		Summary:    "write_file README.md",
		Effects:    []string{"Modifies README.md", "Cannot be undone by running this again"},
	}, discordStyle)

	for _, want := range []string{"write_file README.md", "Modifies README.md", "apr_123", "agent approve"} {
		if !strings.Contains(rendered, want) {
			t.Errorf("the approval does not mention %q:\n%s", want, rendered)
		}
	}
}

// Every state a reader would act on renders; anything invented does not.
//
// "completed" renders now, where once it did not. That is not a change of
// mind about whether a channel wants told the obvious: these lines rewrite one
// message rather than accumulating, so the completed one replaces the line
// that would otherwise sit above the answer still claiming to be working.
func TestStatusRendersEveryStateAReaderWouldActOn(t *testing.T) {
	cases := map[string]bool{
		"running":   true,
		"working":   false,
		"completed": true,
		"failed":    true,
		"cancelled": true,
		"queued":    false,
	}

	for state, wantText := range cases {
		rendered := renderStatus(jcgateway.StatusPayload{State: state}, discordStyle)
		if wantText && rendered == "" {
			t.Errorf("%q renders nothing", state)
		}
		if !wantText && rendered != "" {
			t.Errorf("%q renders %q; a reader does not need it", state, rendered)
		}
	}
}

func TestRunningStatusIsProviderAcknowledgement(t *testing.T) {
	if rendered := renderStatus(jcgateway.StatusPayload{State: "running"}, discordStyle); rendered != "👀" {
		t.Fatalf("running status = %q, want eyes acknowledgement", rendered)
	}
}

// The line says what it is doing, and the thing it is doing is the useful part.
func TestTheWorkingLineNamesTheTool(t *testing.T) {
	rendered := renderStatus(jcgateway.StatusPayload{State: "working", Detail: "read_file notes.txt"}, discordStyle)

	if rendered != "" {
		t.Errorf("the working status was not removed: %q", rendered)
	}
}

func TestCompletedStatusKeepsItsTextSummary(t *testing.T) {
	rendered := renderStatus(jcgateway.StatusPayload{
		State:      "completed",
		DurationMS: 23900,
		Summary: &jcgateway.RunSummary{
			Provider:     "ollama",
			Model:        "gemma4:31b-cloud",
			InputTokens:  32800,
			OutputTokens: 306,
			Tools:        []jcgateway.ToolUse{{Name: "web_read", Calls: 2}},
		},
	}, discordStyle)

	want := "-# • web_read ×2\n-# ⏱ 23.9s · ↑32.8K ↓306 (33.1K) · 13 tok/s · ollama/gemma4:31b-cloud"
	if rendered != want {
		t.Fatalf("completed status = %q, want %q", rendered, want)
	}
}

func TestUnknownDispatchKindIsAnError(t *testing.T) {
	_, err := Dispatch(jcgateway.Dispatch{Kind: "invented", Payload: "{}"}, discordStyle)
	if err == nil {
		t.Error("an unrecognised dispatch kind was rendered anyway")
	}
}

func TestMessagePayloadRoundTrips(t *testing.T) {
	encoded, err := json.Marshal(jcgateway.MessagePayload{Text: "測試訊息"})
	if err != nil {
		t.Fatalf("encode: %v", err)
	}

	rendered, err := Dispatch(jcgateway.Dispatch{
		Kind:    jcgateway.DispatchMessage,
		Payload: string(encoded),
	}, discordStyle)
	if err != nil {
		t.Fatalf("render: %v", err)
	}
	if rendered != "測試訊息" {
		t.Errorf("got %q", rendered)
	}
}

func TestMessagePayloadNormalizesLatexSymbolsForDiscord(t *testing.T) {
	rendered, err := Dispatch(jcgateway.Dispatch{
		Kind:    jcgateway.DispatchMessage,
		Payload: `{"text":"A \\rightarrow B and C \\leftrightarrow D"}`,
	}, discordStyle)
	if err != nil {
		t.Fatalf("render: %v", err)
	}
	if rendered != "A → B and C ↔ D" {
		t.Errorf("got %q, want normalized symbols", rendered)
	}
}

func TestMessagePayloadRemovesUnsupportedLatexFormatting(t *testing.T) {
	rendered := NormalizeText(`$\color{red}{\text{上漲 356.23 點}}$，\rightarrow 目標`)
	if rendered != "上漲 356.23 點，→ 目標" {
		t.Errorf("got %q, want readable Discord text", rendered)
	}
}

// The summary sits under an answer somebody is reading, so it has to say the
// three things they would ask and stop.
func TestSummaryReportsToolsSourcesAndCost(t *testing.T) {
	rendered := renderSummary(&jcgateway.RunSummary{
		Tools: []jcgateway.ToolUse{
			{Name: "web_read", Calls: 2},
			{Name: "read_file", Calls: 3, Failed: 1},
		},
		Sources: []jcgateway.Source{
			{Kind: "web", Ref: "https://example.com/docs", Retained: true},
			{Kind: "file", Ref: "notes.md", Retained: true},
		},
		InputTokens:       4231,
		CachedInputTokens: 3000,
		OutputTokens:      380,
	}, discordStyle)

	for _, want := range []string{
		"web_read ×2",
		"read_file ×3 (1 failed)",
		"https://example.com/docs",
		"notes.md",
		"4231 in / 380 out",
		"3000 of the input was cached",
	} {
		if !strings.Contains(rendered, want) {
			t.Errorf("the summary does not mention %q:\n%s", want, rendered)
		}
	}
}

func TestCompletedStatusUsesCompactUsageLine(t *testing.T) {
	rendered := renderStatus(jcgateway.StatusPayload{
		State:      "completed",
		DurationMS: 36600,
		Summary: &jcgateway.RunSummary{
			Provider:     "google-ai-studio",
			Model:        "gemma-4-31b-it",
			InputTokens:  15100,
			OutputTokens: 648,
			Tools:        []jcgateway.ToolUse{{Name: "read_file", Calls: 2}},
		},
	}, discordStyle)

	want := "-# • read_file ×2\n-# ⏱ 36.6s · ↑15.1K ↓648 (15.7K) · 18 tok/s · google-ai-studio/gemma-4-31b-it"
	if rendered != want {
		t.Fatalf("completed status = %q, want %q", rendered, want)
	}
}

// The claim the summary must never make. Nothing here can observe a model
// declining to rely on something it was shown; material folded into a summary
// may well have shaped the answer through that summary. Calling it unused
// would be inventing a causal fact out of a structural one.
func TestSummaryNeverClaimsASourceWentUnused(t *testing.T) {
	rendered := renderSummary(&jcgateway.RunSummary{
		Sources: []jcgateway.Source{
			{Kind: "web", Ref: "https://example.com/kept", Retained: true},
			{Kind: "web", Ref: "https://example.com/folded", Retained: false},
		},
	}, discordStyle)

	for _, forbidden := range []string{"unused", "not used", "ignored", "discarded"} {
		if strings.Contains(strings.ToLower(rendered), forbidden) {
			t.Errorf("the summary claims a source went unused (%q):\n%s", forbidden, rendered)
		}
	}
	if !strings.Contains(rendered, "folded into a summary") {
		t.Errorf("the summary does not say what actually happened:\n%s", rendered)
	}
}

// Zero means the provider reported nothing, not that nothing was spent.
func TestSummaryOmitsTokensRatherThanReportingZero(t *testing.T) {
	rendered := renderStatus(jcgateway.StatusPayload{
		State:   "completed",
		Summary: &jcgateway.RunSummary{Tools: []jcgateway.ToolUse{{Name: "grep", Calls: 1}}},
	}, discordStyle)

	if strings.Contains(rendered, "0 in") || strings.Contains(rendered, "in / 0 out") {
		t.Errorf("an unreported token count was printed as zero:\n%s", rendered)
	}
}

// Work that failed halfway was still paid for, and that is the case a reader
// most often wants accounted for.
func TestAFailedRunStillAccountsForItself(t *testing.T) {
	rendered := renderStatus(jcgateway.StatusPayload{
		State:  "failed",
		Detail: "the model gave up",
		Summary: &jcgateway.RunSummary{
			Tools:        []jcgateway.ToolUse{{Name: "web_read", Calls: 1, Failed: 1}},
			InputTokens:  12000,
			OutputTokens: 90,
		},
	}, discordStyle)

	if !strings.Contains(rendered, "the model gave up") {
		t.Errorf("the failure reason is gone:\n%s", rendered)
	}
	if !strings.Contains(rendered, "12.0k in / 90 out") {
		t.Errorf("a failed run does not report its cost:\n%s", rendered)
	}
}

// A run with nothing to report adds nothing. A summary that is always there
// and usually empty is noise under every answer.
func TestNoSummaryAddsNothing(t *testing.T) {
	rendered := renderStatus(jcgateway.StatusPayload{State: "completed", Detail: "3s"}, discordStyle)

	if rendered != "-# ⏱ 3s" {
		t.Errorf("an empty summary added something:\n%q", rendered)
	}
}

// A status line does not go through the splitter, and Discord refuses an
// oversized message outright — so an unbounded summary would take the "Done"
// line down with it. Worse than no summary at all.
func TestSummaryStaysPostable(t *testing.T) {
	sources := make([]jcgateway.Source, 0, 12)
	for i := range 12 {
		sources = append(sources, jcgateway.Source{
			Kind: "web",
			// The shape that actually causes this: a real URL with a few
			// hundred characters of tracking parameters on the end.
			Ref: "https://example.com/a/very/long/path/segment/" + strings.Repeat("tracking-parameter-", 20) +
				string(rune('a'+i)),
			Retained: i%2 == 0,
		})
	}

	rendered := renderSummary(&jcgateway.RunSummary{Sources: sources, InputTokens: 90000, OutputTokens: 1200}, discordStyle)

	if len(rendered) > discordStyle.MaxLength {
		t.Fatalf("the status line is %d characters, over Discord's %d limit",
			len(rendered), discordStyle.MaxLength)
	}
	// Bounded, not silent: the reader still learns what the run cost.
	for _, want := range []string{"90.0k in / 1200 out"} {
		if !strings.Contains(rendered, want) {
			t.Errorf("bounding the summary lost %q:\n%s", want, rendered)
		}
	}
}

// One absurd address must not crowd out the others when the list does fit.
func TestALongAddressIsShortenedNotDropped(t *testing.T) {
	rendered := renderSummary(&jcgateway.RunSummary{Sources: []jcgateway.Source{
		{Kind: "web", Ref: "https://example.com/" + strings.Repeat("x", 400), Retained: true},
		{Kind: "file", Ref: "notes.md", Retained: true},
	}}, discordStyle)

	if !strings.Contains(rendered, "https://example.com/xxx") {
		t.Errorf("the long address was dropped entirely:\n%s", rendered)
	}
	if !strings.Contains(rendered, "…") {
		t.Errorf("the long address was not shortened:\n%s", rendered)
	}
	if !strings.Contains(rendered, "notes.md") {
		t.Errorf("a long address crowded out the one after it:\n%s", rendered)
	}
}

// The tool list has no bound of its own: an MCP server may register many tools
// with names nobody here chose. Whatever it produces still has to be postable.
func TestAnEnormousToolListIsStillPostable(t *testing.T) {
	tools := make([]jcgateway.ToolUse, 0, 200)
	for i := range 200 {
		tools = append(tools, jcgateway.ToolUse{
			Name:  fmt.Sprintf("mcp_some_server_a_rather_long_tool_name_number_%d", i),
			Calls: 1,
		})
	}

	rendered := renderSummary(&jcgateway.RunSummary{Tools: tools, InputTokens: 500, OutputTokens: 10}, discordStyle)

	if len(rendered) > discordStyle.MaxLength {
		t.Fatalf("the status line is %d characters, over Discord's %d limit",
			len(rendered), discordStyle.MaxLength)
	}
	if !strings.Contains(rendered, "too long to post in full") {
		t.Errorf("the summary was cut without saying so:\n%s", rendered)
	}
}

// A console channel is a log. What ran, how long it took, whether it worked.
func TestALogLineSaysWhatRanAndHowLongItTook(t *testing.T) {
	rendered := renderLog(jcgateway.LogPayload{
		Tool:       "exec_command",
		Summary:    "go test ./...: ok",
		DurationMS: 1800,
		Artifact:   "sha256-abc123",
	}, discordStyle)

	for _, want := range []string{"exec_command", "1.8s", "go test", "sha256-abc123"} {
		if !strings.Contains(rendered, want) {
			t.Errorf("the log line does not mention %q:\n%s", want, rendered)
		}
	}
	// Subtext, because a log is context for the answer rather than the answer.
	if !strings.HasPrefix(rendered, "-# ") {
		t.Errorf("a log line is shown at the same weight as the reply:\n%s", rendered)
	}
}

func TestAFailedCallIsMarkedAsOne(t *testing.T) {
	rendered := renderLog(jcgateway.LogPayload{
		Tool: "web_read", Summary: "404", DurationMS: 90, IsError: true,
	}, discordStyle)
	if !strings.Contains(rendered, "✗") {
		t.Errorf("a failure is not distinguishable from a success:\n%s", rendered)
	}
}

// Durations span milliseconds to minutes, and one format cannot carry that
// range without losing the fast ones or padding the slow ones.
func TestDurationsAreReadableAtEveryScale(t *testing.T) {
	for ms, want := range map[int64]string{
		5:       "5ms",
		999:     "999ms",
		1500:    "1.5s",
		59_000:  "59.0s",
		61_000:  "1m01s",
		600_000: "10m00s",
	} {
		if got := formatDuration(ms); got != want {
			t.Errorf("formatDuration(%d) = %q, want %q", ms, got, want)
		}
	}
}

// Which model answered, because that changes and the answers change with it.
func TestTheSummarySaysWhoAnswered(t *testing.T) {
	rendered := renderStatus(jcgateway.StatusPayload{
		State:  "completed",
		Detail: "12s",
		Summary: &jcgateway.RunSummary{
			Provider:     "ollama",
			Model:        "gemma4:31b-cloud",
			Tools:        []jcgateway.ToolUse{{Name: "read_file", Calls: 2, Milliseconds: 40}},
			InputTokens:  100,
			OutputTokens: 20,
		},
	}, discordStyle)

	if !strings.Contains(rendered, "ollama/gemma4:31b-cloud") {
		t.Errorf("the summary does not say who answered:\n%s", rendered)
	}
	if !strings.Contains(rendered, "-# • read_file ×2") {
		t.Errorf("the completion status does not list its tools:\n%s", rendered)
	}
}

// A summary says a command exited zero. The output says which test failed and
// on what line, which is what somebody watching a console is watching for.
func TestALogLineCarriesWhatTheToolPrinted(t *testing.T) {
	rendered := renderLog(jcgateway.LogPayload{
		Tool:       "exec_command",
		Summary:    "go test ./...: exit 1",
		DurationMS: 4200,
		IsError:    true,
		Output:     "--- FAIL: TestThing (0.01s)\n    thing_test.go:42: got 3, want 4\nFAIL",
	}, discordStyle)

	for _, want := range []string{"exec_command", "4.2s", "✗", "thing_test.go:42", "want 4"} {
		if !strings.Contains(rendered, want) {
			t.Errorf("the log line does not carry %q:\n%s", want, rendered)
		}
	}
	// In a code block, because the alignment of a failure is the information.
	if !strings.Contains(rendered, "```") {
		t.Errorf("output is not shown as output:\n%s", rendered)
	}
}

// Cut output says it was cut, so nobody reads a partial log as a whole one.
func TestCutOutputSaysSo(t *testing.T) {
	rendered := renderLog(jcgateway.LogPayload{
		Tool:            "exec_command",
		Output:          "the last of it",
		OutputTruncated: true,
	}, discordStyle)
	if !strings.Contains(rendered, "…") {
		t.Errorf("cut output does not say it was cut:\n%s", rendered)
	}
}

// A tool that printed nothing adds no empty block.
func TestNoOutputAddsNoBlock(t *testing.T) {
	rendered := renderLog(jcgateway.LogPayload{Tool: "read_file", Summary: "read a.go"}, discordStyle)
	if strings.Contains(rendered, "```") {
		t.Errorf("an empty code block was added:\n%s", rendered)
	}
}

// A log line is not exempt from the platform's limit. The bound on captured
// output is in runes and the limit is in characters, so output that is all
// multi-byte would otherwise be refused outright and the line lost.
func TestALogLineOfWideCharactersIsStillPostable(t *testing.T) {
	// The bound the projector applies, in characters that are three bytes
	// each.
	wide := strings.Repeat("測", 1200)

	body := renderLog(jcgateway.LogPayload{
		Tool:   "exec_command",
		Output: wide,
	}, discordStyle)

	for _, segment := range Split(body, discordStyle) {
		if len(segment) > discordStyle.MaxLength {
			t.Errorf("a segment is %d bytes, over the %d limit", len(segment), discordStyle.MaxLength)
		}
	}
	if len(Split(body, discordStyle)) == 0 {
		t.Error("the line was lost entirely")
	}
}

// The channel is told, rather than being shown a normal completion for a run
// that did nothing.
func TestASilentRunIsCalledOut(t *testing.T) {
	rendered := renderStatus(jcgateway.StatusPayload{
		State:  "completed",
		Detail: "3s",
		Summary: &jcgateway.RunSummary{
			Provider: "ollama", Model: "gemma4:31b-cloud",
			Silent:      true,
			InputTokens: 2991, OutputTokens: 69,
		},
	}, discordStyle)

	if !strings.Contains(rendered, "returned nothing") {
		t.Errorf("a run that did nothing reads as an ordinary success:\n%s", rendered)
	}
	// And still says what it cost, because it was paid for.
	if !strings.Contains(rendered, "2991") {
		t.Errorf("the cost of a silent run is not reported:\n%s", rendered)
	}
}
