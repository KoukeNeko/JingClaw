package discord

import (
	"bytes"
	"encoding/json"
	"image/png"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/disgoorg/disgo"
	"github.com/disgoorg/disgo/bot"
	"github.com/disgoorg/disgo/rest"

	jcgateway "github.com/KoukeNeko/JingClaw/core/internal/gateway"
	"github.com/KoukeNeko/JingClaw/core/internal/gateway/render/tableimage"
)

// sent is what the stub was asked to post, in order.
type sent struct {
	// Method is the HTTP verb: POST for a new message, PATCH for one edited
	// in place, DELETE for one taken down.
	Method  string
	Path    string
	Content string
	File    []byte
}

// stubDiscord stands in for the REST API and records what it is given.
//
// The real posting path rather than a description of it: what is being
// checked is that an answer with a table in the middle arrives as three
// messages in the right order, and that is a fact about what reaches the
// platform.
func stubDiscord(t *testing.T) (*Adapter, *[]sent) {
	t.Helper()

	var (
		mu     sync.Mutex
		posted []sent
	)

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		record := sent{Method: r.Method, Path: r.URL.Path}

		if strings.HasPrefix(r.Header.Get("content-type"), "multipart/") {
			if err := r.ParseMultipartForm(8 << 20); err != nil {
				t.Errorf("stub: read multipart: %v", err)
			}
			for _, headers := range r.MultipartForm.File {
				for _, header := range headers {
					file, err := header.Open()
					if err != nil {
						t.Errorf("stub: open uploaded file: %v", err)
						continue
					}
					var body bytes.Buffer
					_, _ = body.ReadFrom(file)
					_ = file.Close()
					record.File = body.Bytes()
				}
			}
			if payload := r.MultipartForm.Value["payload_json"]; len(payload) > 0 {
				var message struct {
					Content string `json:"content"`
				}
				_ = json.Unmarshal([]byte(payload[0]), &message)
				record.Content = message.Content
			}
		} else {
			var message struct {
				Content string `json:"content"`
			}
			_ = json.NewDecoder(r.Body).Decode(&message)
			record.Content = message.Content
		}

		mu.Lock()
		posted = append(posted, record)
		mu.Unlock()

		w.Header().Set("content-type", "application/json")
		_, _ = w.Write([]byte(`{"id":"123456789012345678","channel_id":"1"}`))
	}))
	t.Cleanup(server.Close)

	// A token shaped the way the library expects: its first part is the
	// application id in base64, and the client refuses to build without one.
	client, err := disgo.New("MTIzNDU2Nzg5MDEyMzQ1Njc4.stub.stub",
		bot.WithRestClientConfigOpts(rest.WithURL(server.URL)),
	)
	if err != nil {
		t.Fatalf("build a client: %v", err)
	}

	// Built the way the daemon builds it, so the maps a growing answer lives
	// in exist; a bare struct literal has none and the first streamed answer
	// panics on assignment.
	adapter := New(Config{TablesAsImages: true, Logger: slog.Default()}, nil, nil)
	adapter.client = client
	return adapter, &posted
}

func answerWith(t *testing.T, text string) jcgateway.Dispatch {
	t.Helper()
	payload, err := json.Marshal(jcgateway.MessagePayload{Text: text})
	if err != nil {
		t.Fatal(err)
	}
	return jcgateway.Dispatch{
		Kind:    jcgateway.DispatchMessage,
		Payload: string(payload),
		Target:  jcgateway.ConversationRef{ChannelID: "987654321098765432"},
	}
}

const answerWithATable = "看一下這個：\n\n" +
	"| 項目 | 數據 |\n|---|---|\n| 融資 | 200 萬 |\n\n" +
	"所以他們還在早期。"

// The shape the operator asked for: what was said, the picture, what was said
// after — three messages, in that order.
func TestATableArrivesBetweenTheTextAroundIt(t *testing.T) {
	if _, err := tableimage.Load(nil); err != nil {
		t.Skipf("no font with Chinese glyphs on this machine: %v", err)
	}

	adapter, posted := stubDiscord(t)

	if _, err := adapter.Post(t.Context(), answerWith(t, answerWithATable)); err != nil {
		t.Fatalf("post: %v", err)
	}

	if len(*posted) != 3 {
		t.Fatalf("posted %d messages, want 3: %+v", len(*posted), *posted)
	}

	first, middle, last := (*posted)[0], (*posted)[1], (*posted)[2]

	if !strings.Contains(first.Content, "看一下這個") {
		t.Errorf("the first message is %q", first.Content)
	}
	if len(first.File) != 0 {
		t.Error("the first message carried a file")
	}

	if len(middle.File) == 0 {
		t.Fatal("the middle message carried no picture")
	}
	if strings.Contains(middle.Content, "項目") {
		t.Errorf("the table was also written out: %q", middle.Content)
	}

	if !strings.Contains(last.Content, "所以他們還在早期") {
		t.Errorf("the last message is %q", last.Content)
	}
}

func TestWhatIsUploadedIsAPicture(t *testing.T) {
	if _, err := tableimage.Load(nil); err != nil {
		t.Skipf("no font: %v", err)
	}

	adapter, posted := stubDiscord(t)
	if _, err := adapter.Post(t.Context(), answerWith(t, answerWithATable)); err != nil {
		t.Fatalf("post: %v", err)
	}

	picture, err := png.Decode(bytes.NewReader((*posted)[1].File))
	if err != nil {
		t.Fatalf("what was uploaded is not a picture: %v", err)
	}
	if picture.Bounds().Dx() == 0 || picture.Bounds().Dy() == 0 {
		t.Error("the picture has no size")
	}
}

// An answer with no table takes the ordinary path, so turning this on changes
// nothing about every answer that has none.
func TestAnAnswerWithNoTableIsOneMessage(t *testing.T) {
	adapter, posted := stubDiscord(t)

	if _, err := adapter.Post(t.Context(), answerWith(t, "just an answer")); err != nil {
		t.Fatalf("post: %v", err)
	}

	if len(*posted) != 1 {
		t.Fatalf("posted %d messages, want 1", len(*posted))
	}
	if (*posted)[0].Content != "just an answer" {
		t.Errorf("the message is %q", (*posted)[0].Content)
	}
}

// Off, the answer is what it always was: one message with the table written
// out in it.
func TestWithTheSettingOffTheTableIsWrittenOut(t *testing.T) {
	adapter, posted := stubDiscord(t)
	adapter.config.TablesAsImages = false

	if _, err := adapter.Post(t.Context(), answerWith(t, answerWithATable)); err != nil {
		t.Fatalf("post: %v", err)
	}

	if len(*posted) != 1 {
		t.Fatalf("posted %d messages, want 1: %+v", len(*posted), *posted)
	}
	if !strings.Contains((*posted)[0].Content, "項目") {
		t.Errorf("the table is not in the message: %q", (*posted)[0].Content)
	}
	if len((*posted)[0].File) != 0 {
		t.Error("a picture was uploaded with the setting off")
	}
}

// A machine with no typeface that can draw the text. The answer still has to
// arrive: what this feature may do when it cannot work is look like it did
// before, and what it may never do is lose the answer.
func TestWithNoFontTheAnswerStillArrives(t *testing.T) {
	adapter, posted := stubDiscord(t)
	adapter.fonts = func() (tableimage.Fonts, error) {
		return tableimage.Load([]string{"/nowhere/no-such-font.ttc"})
	}

	if _, err := adapter.Post(t.Context(), answerWith(t, answerWithATable)); err != nil {
		t.Fatalf("post: %v", err)
	}

	if len(*posted) == 0 {
		t.Fatal("nothing arrived at all")
	}

	whole := ""
	for _, message := range *posted {
		if len(message.File) != 0 {
			t.Error("a picture was uploaded with no font to draw it")
		}
		whole += message.Content
	}

	for _, expected := range []string{"看一下這個", "項目", "融資", "所以他們還在早期"} {
		if !strings.Contains(whole, expected) {
			t.Errorf("%q is missing from what arrived:\n%s", expected, whole)
		}
	}
}

// The table the model actually drew, copied from a channel. It ignored the
// instruction and boxed the table itself, which is the case this whole path
// had to grow to cover: what reaches the reader is what the model wrote, not
// what it was asked to write.
const modelDrewItsOwn = "以下為江總督整理：\n\n```text\n" +
	"+---------------+------------------------+\n" +
	"| 時間節點      | ARR（年經常性營收）    |\n" +
	"+---------------+------------------------+\n" +
	"| 2025 年 7 月  | 30 萬美元（~960萬）    |\n" +
	"| 2025 年 11 月 | 50 萬美元（~1600萬）   |\n" +
	"+---------------+------------------------+\n" +
	"```\n\n所以還在早期。\n"

func TestATableTheModelDrewAlsoBecomesAPicture(t *testing.T) {
	if _, err := tableimage.Load(nil); err != nil {
		t.Skipf("no font: %v", err)
	}

	adapter, posted := stubDiscord(t)
	if _, err := adapter.Post(t.Context(), answerWith(t, modelDrewItsOwn)); err != nil {
		t.Fatalf("post: %v", err)
	}

	if len(*posted) != 3 {
		t.Fatalf("posted %d messages, want 3: %+v", len(*posted), *posted)
	}

	if !strings.Contains((*posted)[0].Content, "以下為江總督整理") {
		t.Errorf("the first message is %q", (*posted)[0].Content)
	}
	if len((*posted)[1].File) == 0 {
		t.Fatal("the drawn table did not become a picture")
	}
	if strings.Contains((*posted)[1].Content, "+---") {
		t.Errorf("the boxes were posted as well: %q", (*posted)[1].Content)
	}
	if !strings.Contains((*posted)[2].Content, "所以還在早期") {
		t.Errorf("the last message is %q", (*posted)[2].Content)
	}
}

// The reason fences are otherwise left alone. A program's output that happens
// to look like a table must arrive as the bytes it is.
func TestProgramOutputIsStillPostedAsText(t *testing.T) {
	adapter, posted := stubDiscord(t)

	output := "查詢結果：\n\n```\nmysql> select * from people;\n" +
		"+----+-------+\n| id | name  |\n+----+-------+\n|  1 | ada   |\n+----+-------+\n" +
		"1 row in set (0.00 sec)\n```\n"

	if _, err := adapter.Post(t.Context(), answerWith(t, output)); err != nil {
		t.Fatalf("post: %v", err)
	}

	for _, message := range *posted {
		if len(message.File) != 0 {
			t.Error("a program's output was turned into a picture")
		}
	}

	whole := ""
	for _, message := range *posted {
		whole += message.Content
	}
	for _, expected := range []string{"mysql>", "1 row in set", "| id | name"} {
		if !strings.Contains(whole, expected) {
			t.Errorf("%q did not survive:\n%s", expected, whole)
		}
	}
}

// A streamed answer that turns out to hold a table is not said twice.
//
// While it is being written, an answer grows in one message. When it finishes
// and a table is to be drawn, what was said before the table has to replace
// that growing message in place — the way finishAsMessages does — and the
// picture and what follows come after it. Posting the split answer beside the
// streamed one leaves the whole answer standing, table written out, and then
// the same answer again in pieces.
func TestAStreamedAnswerWithATableIsNotSaidTwice(t *testing.T) {
	if _, err := tableimage.Load(nil); err != nil {
		t.Skipf("no font with Chinese glyphs on this machine: %v", err)
	}

	adapter, posted := stubDiscord(t)
	target := jcgateway.ConversationRef{ChannelID: "987654321098765432"}

	// It starts being written: one message, growing.
	partial := jcgateway.Dispatch{
		Kind:    jcgateway.DispatchMessage,
		Target:  target,
		Payload: `{"text":"看一下這個：","message_id":"msg_1"}`,
	}
	if _, err := adapter.Post(t.Context(), partial); err != nil {
		t.Fatalf("partial: %v", err)
	}
	if len(*posted) != 1 || (*posted)[0].Method != "POST" {
		t.Fatalf("the partial did not start one message: %+v", *posted)
	}

	// Then the whole thing arrives, and it has a table in the middle.
	payload, _ := json.Marshal(map[string]any{
		"text":       answerWithATable,
		"message_id": "msg_1",
		"final":      true,
	})
	final := jcgateway.Dispatch{Kind: jcgateway.DispatchMessage, Target: target, Payload: string(payload)}
	if _, err := adapter.Post(t.Context(), final); err != nil {
		t.Fatalf("final: %v", err)
	}

	after := (*posted)[1:]

	// The growing message is finished in place with what came before the
	// table — not left as it was and the answer posted again beside it.
	if len(after) == 0 || after[0].Method != "PATCH" {
		t.Fatalf("the streamed message was not finished in place; what followed the partial was: %+v", after)
	}
	if !strings.Contains(after[0].Content, "看一下這個") || strings.Contains(after[0].Content, "項目") {
		t.Errorf("the edited message should hold only what came before the table: %q", after[0].Content)
	}

	// Then the picture, then what was said after: two new messages, not three.
	fresh := 0
	for _, one := range after {
		if one.Method == "POST" {
			fresh++
		}
	}
	if fresh != 2 {
		t.Errorf("finishing posted %d new messages, want 2 (the picture and the text after): %+v", fresh, after)
	}

	// And the answer is released, so its message is not extended by mistake.
	if _, still := adapter.liveAnswer("msg_1"); still {
		t.Error("the finished answer kept its growing message")
	}
}

// An answer that opens with a table has nothing at the front to finish the
// growing message into. It is taken down — otherwise the table stands written
// out, and then again as a picture of itself.
func TestAStreamedAnswerThatOpensWithATableTakesTheGrowingMessageDown(t *testing.T) {
	if _, err := tableimage.Load(nil); err != nil {
		t.Skipf("no font with Chinese glyphs on this machine: %v", err)
	}

	adapter, posted := stubDiscord(t)
	target := jcgateway.ConversationRef{ChannelID: "987654321098765432"}

	partial := jcgateway.Dispatch{
		Kind: jcgateway.DispatchMessage, Target: target,
		Payload: `{"text":"| 項目 |","message_id":"msg_2"}`,
	}
	if _, err := adapter.Post(t.Context(), partial); err != nil {
		t.Fatalf("partial: %v", err)
	}

	tableFirst := "| 項目 | 數據 |\n|---|---|\n| 融資 | 200 萬 |\n\n所以他們還在早期。"
	payload, _ := json.Marshal(map[string]any{"text": tableFirst, "message_id": "msg_2", "final": true})
	final := jcgateway.Dispatch{Kind: jcgateway.DispatchMessage, Target: target, Payload: string(payload)}
	if _, err := adapter.Post(t.Context(), final); err != nil {
		t.Fatalf("final: %v", err)
	}

	after := (*posted)[1:]
	if len(after) == 0 || after[0].Method != "DELETE" {
		t.Fatalf("the growing message was not taken down; what followed the partial was: %+v", after)
	}
	for _, one := range after {
		if one.Method == "PATCH" {
			t.Errorf("a message was edited in place though the answer opens with a table: %+v", after)
		}
	}
	if _, still := adapter.liveAnswer("msg_2"); still {
		t.Error("the finished answer kept its growing message")
	}
}
