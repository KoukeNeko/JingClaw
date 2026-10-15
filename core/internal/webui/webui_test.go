package webui_test

import (
	"encoding/json"
	"io"
	"io/fs"
	"net/http"
	"net/http/httptest"
	"reflect"
	"regexp"
	"strings"
	"testing"

	"github.com/KoukeNeko/JingClaw/core/internal/config"
	"github.com/KoukeNeko/JingClaw/core/internal/webui"
)

func TestThePageIsServedWithItsPolicy(t *testing.T) {
	rec := httptest.NewRecorder()
	webui.Handler().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/", nil))

	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), "<html") {
		t.Fatalf("got %d: %.80s", rec.Code, rec.Body)
	}

	for header, want := range map[string]string{
		"X-Content-Type-Options": "nosniff",
		"Referrer-Policy":        "no-referrer",
		"Cache-Control":          "no-store",
	} {
		if got := rec.Header().Get(header); !strings.Contains(got, want) {
			t.Errorf("%s is %q, want it to contain %q", header, got, want)
		}
	}
}

// The policy is what stands behind the rule that nothing anyone wrote is read
// as markup, because the page may compile its own expressions. Each directive
// is pinned to its value, so that widening one (a script from somewhere else,
// an inline script, a connection to another host) is a change to this test and
// not a quiet one.
func TestThePolicyAllowsOnlyWhatThePageNeeds(t *testing.T) {
	rec := httptest.NewRecorder()
	webui.Handler().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/", nil))

	got := make(map[string]string)
	for _, directive := range strings.Split(rec.Header().Get("Content-Security-Policy"), ";") {
		name, value, _ := strings.Cut(strings.TrimSpace(directive), " ")
		got[name] = value
	}

	want := map[string]string{
		"default-src":     "'none'",
		"script-src":      "'self' 'unsafe-eval'",
		"style-src":       "'self'",
		"style-src-attr":  "'unsafe-inline'",
		"img-src":         "'self' data:",
		"connect-src":     "'self'",
		"base-uri":        "'none'",
		"form-action":     "'none'",
		"frame-ancestors": "'none'",
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("the policy is %v, want %v", got, want)
	}
}

// The page can approve a command. Nothing but a read is a reason to answer.
func TestOnlyReadsAreAnswered(t *testing.T) {
	for _, method := range []string{http.MethodPost, http.MethodPut, http.MethodDelete} {
		rec := httptest.NewRecorder()
		webui.Handler().ServeHTTP(rec, httptest.NewRequest(method, "/", strings.NewReader("x")))
		if rec.Code != http.StatusMethodNotAllowed {
			t.Errorf("%s got %d", method, rec.Code)
		}
	}
}

func TestAFileThatIsNotThereIsNotFound(t *testing.T) {
	rec := httptest.NewRecorder()
	webui.Handler().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/nothing.js", nil))
	if rec.Code != http.StatusNotFound {
		t.Errorf("got %d", rec.Code)
	}
}

var reference = regexp.MustCompile(`(?:src|href)="([^"]+)"`)

// A page that names a file the binary does not carry is blank, and nothing
// says why.
func TestEveryFileThePageNamesIsEmbedded(t *testing.T) {
	index, err := fs.ReadFile(webui.Assets(), "index.html")
	if err != nil {
		t.Fatalf("read the page: %v", err)
	}

	found := reference.FindAllStringSubmatch(string(index), -1)
	if len(found) == 0 {
		t.Fatal("the page names no files")
	}

	for _, match := range found {
		name := match[1]

		// The policy forbids loading from anywhere else; this is where a page
		// that tried would be caught before it is a blank screen.
		if strings.Contains(name, "://") || strings.HasPrefix(name, "//") {
			t.Errorf("the page loads %s from somewhere else", name)
			continue
		}
		if _, err := fs.Stat(webui.Assets(), name); err != nil {
			t.Errorf("the page names %s, which is not embedded", name)
		}
	}
}

// page returns one of the files the page is made of.
func page(t *testing.T, name string) string {
	t.Helper()

	raw, err := fs.ReadFile(webui.Assets(), name)
	if err != nil {
		t.Fatalf("read %s: %v", name, err)
	}
	return string(raw)
}

// htmlBinding is an attribute that puts a string into the page as markup.
var htmlBinding = regexp.MustCompile(`x-html="([^"]*)"`)

// markupSinks are the other ways a script puts a string into the page as
// markup, or lets one decide where a link goes.
var markupSinks = []string{
	"x-html", "v-html", "innerHTML", "outerHTML", "insertAdjacentHTML", "document.write", "srcdoc",
	":href", "x-bind:href", ":src", "x-bind:src", ":action", "x-bind:action",
}

// The page is allowed to run what the framework compiles ('unsafe-eval'), so
// the one thing between a skill's description, a memory, a session title or a
// line of the settings and a script in the administrator's page is that none
// of them is ever read as markup. The three places that are, show text that is
// written in schema.js and nowhere else; every other string is put in with
// x-text.
func TestNothingAnyoneWroteIsPutInThePageAsMarkup(t *testing.T) {
	// The schema's own sentences: the explanation under a setting, and the note
	// under a choice of one. Both are fixed text in a file that ships with the
	// program, escaped before the few backticks in it become <code>.
	allowed := map[string]int{
		"md(r.f.help)": 2,
		"md((r.f.options.find(o => o.v===ev(r)) || {}).note)": 1,
	}

	index := page(t, "index.html")

	found := make(map[string]int)
	for _, match := range htmlBinding.FindAllStringSubmatch(index, -1) {
		found[match[1]]++
	}
	for expression, n := range found {
		if allowed[expression] != n {
			t.Errorf("index.html has %d x-html of %q, and %d are allowed", n, expression, allowed[expression])
		}
	}
	for expression, n := range allowed {
		if found[expression] != n {
			t.Errorf("index.html has %d x-html of %q, and the allowed list has %d", found[expression], expression, n)
		}
	}

	// md is the function that turns backticks into <code>, and it trusts its
	// argument: it may only be called inside one of those.
	if got, want := strings.Count(index, "md("), 3; got != want {
		t.Errorf("index.html calls md( %d times, and only the %d inside the allowed x-html are meant", got, want)
	}

	for _, name := range []string{"index.html", "app.js", "api.js"} {
		text := page(t, name)
		for _, sink := range markupSinks {
			if strings.Contains(text, sink) && !(name == "index.html" && sink == "x-html") {
				t.Errorf("%s contains %s", name, sink)
			}
		}
	}
	if strings.Contains(page(t, "app.js"), "x-html") {
		t.Error("app.js builds an x-html")
	}
}

// The settings used to be a file, and the page said so in places a person
// reads before deciding what to do. Each of these was true then and is false
// now, and one of them was false even then (that a restart is not needed).
func TestThePageDoesNotSpeakOfTheSettingsAsAFileOrAsLive(t *testing.T) {
	retired := []string{
		"設定檔", "config.toml", "PERSONA.md", "cfg.path",
		"不需要重啟", "不用重啟", "重新讀取", "下一次執行就會用到", "applies from the next run",
	}
	for _, name := range []string{"index.html", "app.js", "schema.js"} {
		text := page(t, name)
		for _, phrase := range retired {
			if strings.Contains(text, phrase) {
				t.Errorf("%s still says %q", name, phrase)
			}
		}
	}
}

// A key typed into the page has one place to be: the password box it was typed
// in, read once when it is sent, and cleared whether the call worked or not. It
// is in no state the page keeps, which is also what a dump of that state, a
// settings check, the diff and the browser's storage would show.
func TestAKeyTypedIntoThePageHasNowhereToLive(t *testing.T) {
	index, app, api := page(t, "index.html"), page(t, "app.js"), page(t, "api.js")

	boxes := regexp.MustCompile(`<input[^>]*type="password"[^>]*>`).FindAllString(index, -1)
	if len(boxes) != 1 {
		t.Fatalf("the page has %d password boxes, and the key has one", len(boxes))
	}
	if !strings.Contains(boxes[0], `autocomplete="off"`) {
		t.Errorf("the password box lets the browser fill or keep it: %s", boxes[0])
	}
	// Anything that binds the box to the page's state puts the key there.
	for _, binding := range []string{"x-model", ":value", "x-bind:value", "@input", "x-on:input", "x-ref"} {
		if strings.Contains(boxes[0], binding) {
			t.Errorf("the password box has %s, which would keep what is typed in the page's state", binding)
		}
	}

	if got := strings.Count(app, `API.call("CredentialService", "PutCredential"`); got != 1 {
		t.Errorf("app.js saves a credential in %d places, and the one that clears the box is the only one", got)
	}
	if strings.Contains(index, "PutCredential") || strings.Contains(api, "PutCredential") {
		t.Error("a credential is saved from somewhere that does not clear the box")
	}
	// The value is read from the box, and only there, and the box is cleared in
	// a finally, which runs when the call fails as well.
	if got := strings.Count(app, "value: input.value"); got != 1 {
		t.Errorf("app.js reads the typed key %d times into a request", got)
	}
	// Every other mention of the box is looking at whether it is empty, or
	// emptying it. A read that keeps the text anywhere is none of those.
	rest := app
	for _, allowed := range []string{"!input.value.trim()", `input.value = ""`, "value: input.value"} {
		rest = strings.ReplaceAll(rest, allowed, "")
	}
	if strings.Contains(rest, "input.value") {
		t.Error("app.js reads the password box in a way that could keep what is typed")
	}
	if !regexp.MustCompile(`finally \{ input\.value = ""`).MatchString(app) {
		t.Error("app.js does not clear the password box in a finally")
	}
	// The saved time is sent back as the string that was received.
	if !strings.Contains(app, "body.expectModifiedAt = expect") || !strings.Contains(app, `this.credExpect = c.modifiedAt || ""`) {
		t.Error("app.js does not send modifiedAt back as it received it")
	}

	// Nothing the page keeps outlives it or reaches a log.
	for name, text := range map[string]string{"app.js": app, "index.html": index} {
		for _, sink := range []string{"localStorage", "sessionStorage", "console.", "document.cookie", "location.hash =", "history.pushState"} {
			if strings.Contains(text, sink) {
				t.Errorf("%s uses %s", name, sink)
			}
		}
	}
	if strings.Count(api, "sessionStorage") == 0 {
		t.Error("api.js no longer keeps the session in sessionStorage, and this test should be told why")
	}

	// A credential is not an edit of the settings: the checks and the diff are
	// built from edits, so none of them may be built from a credential.
	for _, name := range []string{"editsPayload() {", "scheduleCheck() {", "async runCheck() {", "askWrite() {", "async write() {"} {
		if body := method(t, app, name); strings.Contains(body, "cred") {
			t.Errorf("%s mentions a credential", name)
		}
	}
}

// A skill is somebody else's text from its name to the last file in it, and an
// installation changes what every later session is told. So the page that
// reviews one shows it only as text, with what cannot be seen made visible, and
// installs only the tree it showed, replacing only when asked twice.
func TestTheSkillsPageShowsAnotherPersonsTextAsTextAndInstallsOnlyWhatItShowed(t *testing.T) {
	index, app := page(t, "index.html"), page(t, "app.js")

	// Every box of raw text (SKILL.md, a file of the skill, what git said) is a
	// <pre> bound with x-text through vis, which marks the characters that hide
	// or reorder what is read.
	raws := regexp.MustCompile(`<pre class="raw"[^>]*>`).FindAllString(index, -1)
	if len(raws) != 3 {
		t.Errorf("the page has %d raw boxes, and the three are SKILL.md, a file and git's error", len(raws))
	}
	for _, raw := range raws {
		if !strings.Contains(raw, `x-text="vis(`) {
			t.Errorf("a raw box is not shown through vis: %s", raw)
		}
	}
	for _, line := range []string{`x-text="vis(rev.md)"`, `x-text="vis(rev.file.text)"`, `x-text="vis(stageErr)"`, `x-text="vis(l.t)"`} {
		if !strings.Contains(index, line) {
			t.Errorf("index.html does not show %s", line)
		}
	}

	// Inside the skills view, a binding that names a field a repository, a skill
	// or git wrote has to go through vis (or srcLine, which does). Words in
	// quotes are the page's own and are not looked at.
	_, view, found := strings.Cut(index, `<template x-if="view==='skills'">`)
	if !found {
		t.Fatal("index.html has no skills view")
	}
	view, _, _ = strings.Cut(view, "</main>")
	quoted := regexp.MustCompile(`'[^']*'`)
	written := regexp.MustCompile(`\b(name|description|version|directory|reason|path|repository|commit|treeDigest|stageErr|skillErr|msg|error|md|oldMd|text|source)\b`)
	for _, match := range regexp.MustCompile(`x-text="([^"]*)"`).FindAllStringSubmatch(view, -1) {
		expression := quoted.ReplaceAllString(match[1], "")
		if written.MatchString(expression) && !strings.Contains(expression, "vis(") && !strings.Contains(expression, "srcLine(") {
			t.Errorf("the skills view shows %q without vis", match[1])
		}
	}

	// Nothing the skills page calls is a service it was not built against, and
	// all six are used.
	calls := regexp.MustCompile(`API\.call\("SkillService", "(\w+)"`).FindAllStringSubmatch(app, -1)
	used := make(map[string]int)
	for _, call := range calls {
		used[call[1]]++
	}
	for _, name := range []string{"ListSkills", "StageSkill", "GetStagedSkill", "ActivateSkill", "DiscardStagedSkill", "RemoveSkill"} {
		if used[name] == 0 {
			t.Errorf("app.js never calls %s", name)
		}
		delete(used, name)
	}
	for name := range used {
		t.Errorf("app.js calls SkillService/%s, which the page was not built against", name)
	}

	// One place installs. It sends the digest of the review on the screen, and
	// replace only from the confirmation that says so.
	if got := strings.Count(app, `API.call("SkillService", "ActivateSkill"`); got != 1 {
		t.Errorf("app.js activates a skill in %d places", got)
	}
	if !strings.Contains(app, "name: rv.name, treeDigest: rv.treeDigest, replace: replace === true") {
		t.Error("the activation does not send the digest of the review that was shown, with replace only when told")
	}
	if got := strings.Count(index, "activate(true)"); got != 1 {
		t.Errorf("index.html asks to replace in %d places, and the confirmation is the one", got)
	}
	if got := strings.Count(app, "this.activate(false)"); got != 1 {
		t.Errorf("app.js activates without replacing in %d places", got)
	}
	if !strings.Contains(app, `if (rv.replaces) this.dialog = "replaceskill"`) {
		t.Error("a skill that replaces another is not put behind its own confirmation")
	}
	if !strings.Contains(index, `:disabled="!rev.matches || rev.busy"`) {
		t.Error("the activate button is not held while the staged tree does not match its digest")
	}

	// A daemon with no agent does not serve the skills, so the page does not ask.
	begin := method(t, app, "async begin() {")
	repairBranch, _, _ := strings.Cut(begin[strings.Index(begin, "if (this.repair) {"):], "} else {")
	if strings.Contains(repairBranch, "Skills") {
		t.Error("the page asks for skills when the daemon has no agent")
	}
	if tick := method(t, app, "tick() {"); !strings.Contains(tick, `!this.repair && this.view === "skills"`) {
		t.Error("the page's poll asks for skills without checking for an agent")
	}
	if !strings.Contains(index, `x-show="mode==='running'"`) {
		t.Error("the page switcher is not held back until there is an agent")
	}
	// The approvals and questions that wait on a person belong to the board and
	// to a session, not above a skill under review or any other page of the
	// management view.
	if !strings.Contains(index, `x-show="cards.length && !manage"`) {
		t.Error("the cards waiting on a person are drawn above the management pages")
	}
	if !strings.Contains(app, `get manage() { return this.view === "skills" || this.view === "memory" || this.view === "channels"; }`) {
		t.Error("the management view is not the skills, memory and channels pages")
	}
}

// What the agent remembers can be what somebody on a chat platform said, and a
// binding is made of ids that an operator or a settings text supplied. So the
// pages that list them show every word as text, with what cannot be seen made
// visible; they only call what they were built against, and a memory or a
// binding is deleted only from the confirmation that says what is lost. Nothing
// on the board's own poll asks for either, and a daemon with no agent does not
// serve them.
func TestTheMemoryAndChannelPagesShowOthersTextAsTextAndDeleteOnlyAfterConfirming(t *testing.T) {
	index, app := page(t, "index.html"), page(t, "app.js")

	// Every x-text that puts a field somebody else wrote into the page goes
	// through vis: in the two pages, and in the two confirmations.
	_, memoryOn, found := strings.Cut(index, `<template x-if="view==='memory'">`)
	if !found {
		t.Fatal("index.html has no memory view")
	}
	pages, _, _ := strings.Cut(memoryOn, "</main>")
	dialogs := ""
	for _, label := range []string{"t-forget", "t-delbind"} {
		_, rest, found := strings.Cut(index, `aria-labelledby="`+label+`"`)
		if !found {
			t.Fatalf("index.html has no dialog %s", label)
		}
		dialog, _, _ := strings.Cut(rest, "</dialog>")
		dialogs += dialog
	}
	quoted := regexp.MustCompile(`'[^']*'`)
	written := regexp.MustCompile(`\b(text|scopeRef|approvedBy|channel|tenant|account|workspace|error|who|memFrom|titleOf|bindErr|schErr|memNote|bindNote)\b`)
	for _, match := range regexp.MustCompile(`x-text="([^"]*)"`).FindAllStringSubmatch(pages+dialogs, -1) {
		expression := quoted.ReplaceAllString(match[1], "")
		// A note is built in app.js, from vis'd parts.
		if written.MatchString(expression) && !strings.Contains(expression, "vis(") && !regexp.MustCompile(`^(memNote|bindNote)$`).MatchString(strings.TrimSpace(expression)) {
			t.Errorf("a memory or channel page shows %q without vis", match[1])
		}
	}
	// The notes are words of the page's own plus ids that went through vis.
	for _, note := range []string{"this.bindNote = ", "this.memNote = "} {
		for _, line := range strings.Split(app, "\n") {
			if strings.Contains(line, note) && regexp.MustCompile(`\b(channel|workspace|text)\b`).MatchString(quoted.ReplaceAllString(line, "")) && !strings.Contains(line, "vis(") {
				t.Errorf("a note is built from a field without vis: %s", strings.TrimSpace(line))
			}
		}
	}

	// Only these methods, and each of the three that change something from one
	// place.
	want := map[string][]string{
		"MemoryService":  {"ListMemories", "ForgetMemory"},
		"ChannelService": {"ListBindings", "UpsertBinding", "DeleteBinding"},
	}
	for service, methods := range want {
		used := make(map[string]int)
		for _, call := range regexp.MustCompile(`API\.call\("`+service+`", "(\w+)"`).FindAllStringSubmatch(app, -1) {
			used[call[1]]++
		}
		for _, name := range methods {
			if used[name] != 1 {
				t.Errorf("app.js calls %s/%s in %d places, and one is meant", service, name, used[name])
			}
			delete(used, name)
		}
		for name := range used {
			t.Errorf("app.js calls %s/%s, which the page was not built against", service, name)
		}
	}
	if got := strings.Count(app, `API.call("SessionService", "CreateSchedule"`); got != 1 {
		t.Errorf("app.js creates a schedule in %d places", got)
	}

	// Deleting is behind a confirmation: the lists only open it, and the one
	// button that does it is in the dialog that says what is lost.
	for method, ask := range map[string]string{"forgetMem()": "askForget(m)", "removeBinding()": "askUnbind(b)"} {
		if got := strings.Count(index, `@click="`+method+`"`); got != 1 {
			t.Errorf("index.html runs %s from %d places, and the dialog is the one", method, got)
		}
		if !strings.Contains(index, `@click="`+ask+`"`) {
			t.Errorf("the list does not open the confirmation with %s", ask)
		}
	}
	if !strings.Contains(index, "這是刪除，無法復原") || !strings.Contains(index, "不會因為忘掉這則記憶而被刪掉") {
		t.Error("the memory confirmation does not say that it is a deletion, and what is left")
	}

	// A binding is only added where there is none: the form reads the list again
	// and stops at a channel that is bound, because writing over one drops the
	// approvers that only the settings can set.
	add := method(t, app, "async addBinding() {")
	if reread, upsert := strings.Index(add, "await this.loadBindings()"), strings.Index(add, `"UpsertBinding"`); reread < 0 || upsert < reread || !strings.Contains(add[reread:upsert], "return;") {
		t.Error("addBinding does not read the bindings again and stop at a channel that is already bound before it upserts")
	}

	// Who may trigger is said as the CLI says it: an empty list is anybody the
	// platform lets in, which is not nobody.
	if !strings.Contains(index, "平台放進這個頻道的任何人") {
		t.Error("the channels page does not say that a binding naming nobody lets in whoever the platform does")
	}
	channels := index[strings.Index(index, `<template x-if="view==='channels'">`):]
	channels, _, _ = strings.Cut(channels, "</main>")
	for _, wrong := range []string{"沒有人能觸發", "誰都不能觸發", "不能觸發"} {
		if strings.Contains(channels, wrong) {
			t.Errorf("the channels page says %q of a binding that names nobody", wrong)
		}
	}

	// The board's own poll asks for neither, opening the page asks for neither,
	// and a daemon with no agent is never asked.
	if tick := method(t, app, "tick() {"); strings.Contains(tick, "Memory") || strings.Contains(tick, "Bindings") {
		t.Error("the page's poll asks for memories or bindings")
	}
	begin := method(t, app, "async begin() {")
	if strings.Contains(begin, "Memory") || strings.Contains(begin, "Bindings") {
		t.Error("opening the page asks for memories or bindings")
	}
	if refresh := method(t, app, "async refresh() {"); strings.Contains(refresh, "MemoryService") || strings.Contains(refresh, "ChannelService") {
		t.Error("the board's refresh asks for memories or bindings")
	}
}

// The cards that wait on a person carry what an agent, somebody in a chat or
// whoever named a session wrote: what the call is, what it would do, the
// question, its choices, the titles. A reader who cannot see a character that
// reorders or hides text decides about something other than what runs, so every
// one of them goes through vis, and the card shows the arguments themselves
// because the summary keeps only the start of each.
func TestTheCardsThatWaitOnAPersonShowWhatTheyCarryThroughVisAndTheArgumentsInFull(t *testing.T) {
	index, app := page(t, "index.html"), page(t, "app.js")

	_, section, found := strings.Cut(index, `<section class="need" x-show="cards.length`)
	if !found {
		t.Fatal("index.html has no section of cards that wait")
	}
	section, _, _ = strings.Cut(section, "</section>")

	quoted := regexp.MustCompile(`'[^']*'`)
	written := regexp.MustCompile(`\b(summary|preview|prompt|label|detail|arguments|e|o)\b`)
	for _, match := range regexp.MustCompile(`(?:x-text|:title)="([^"]*)"`).FindAllStringSubmatch(section, -1) {
		expression := quoted.ReplaceAllString(match[1], "")
		if written.MatchString(expression) && !strings.Contains(expression, "vis(") {
			t.Errorf("a card shows %q without vis", match[1])
		}
	}
	for _, line := range []string{
		`x-text="vis(argsText(p.x.arguments))"`, `x-text="vis(p.x.summary)"`, `x-text="vis(p.x.prompt)"`, `x-text="titleOf(p.sid)"`,
	} {
		if !strings.Contains(section, line) {
			t.Errorf("the cards do not show %s", line)
		}
	}
	if !strings.Contains(app, "arguments: a.arguments") {
		t.Error("app.js does not carry the arguments of an approval to the card")
	}

	// The same for the names of sessions and what a schedule asks, wherever they
	// are shown, and for the toast, which says them back.
	for _, line := range []string{`x-text="vis(s.title)"`, `x-text="vis(detail.title)"`} {
		if !strings.Contains(index, line) {
			t.Errorf("index.html does not show %s", line)
		}
	}
	if got := strings.Count(index, `x-text="vis(sc.prompt)"`); got != 2 {
		t.Errorf("index.html shows what a schedule asks through vis in %d places, and there are two lists", got)
	}
	if strings.Contains(index, `x-text="sc.prompt"`) || strings.Contains(index, `x-text="s.title"`) || strings.Contains(index, `x-text="detail.title"`) {
		t.Error("index.html shows a title or a schedule's prompt without vis")
	}
	if !strings.Contains(app, "this.toast = vis(t)") {
		t.Error("a toast is not shown through vis")
	}
	// A display name is whatever the person chose to be called; the id beside it
	// is what the platform says.
	for _, line := range []string{"vis(p.displayName)", "vis(p.principalId)"} {
		if !strings.Contains(app, line) {
			t.Errorf("a run's origin does not show %s", line)
		}
	}

	// The characters it marks. Source only: a character missing from this list
	// is one that is shown as itself.
	hidden := regexp.MustCompile(`const HIDDEN = (.*)\n`).FindStringSubmatch(app)
	if hidden == nil {
		t.Fatal("app.js has no HIDDEN")
	}
	for _, class := range []string{`\u034F`, `\u115F`, `\u1160`, `\u17B4`, `\u17B5`, `\u180B-\u180E`, `\u2800`, `\u3164`, `\uFFA0`, `\uFE00-\uFE0F`, `\u202E`, `\u200B-\u200F`} {
		if !strings.Contains(hidden[1], class) {
			t.Errorf("vis does not mark %s", class)
		}
	}
}

// What the person was about to press must not move, and pressing it twice must
// not decide twice.
func TestTheCardsDoNotMoveUnderThePointerAndAnActionIsNotSentTwice(t *testing.T) {
	app := page(t, "app.js")

	// Oldest first, so that a request that arrives is added at the end.
	if !strings.Contains(app, "sort((m, n) => m.created - n.created)") {
		t.Error("the waiting cards are not ordered oldest first")
	}

	// An action that is still being done is not done again.
	act := method(t, app, "async act(call, ok, key) {")
	if !strings.Contains(act, "this._doing[key]") || !strings.Contains(act, "finally") {
		t.Errorf("act does not hold an action while it is being done: %s", act)
	}
	for _, line := range []string{`"d:" + p.id`, `"a:" + p.id`, `"i:" + run.id`, `"p:" + sc.id`} {
		if !strings.Contains(app, line) {
			t.Errorf("app.js does not give an action its key: %s", line)
		}
	}
	for _, name := range []string{"async createSession() {", "async write() {"} {
		if body := method(t, app, name); !strings.Contains(body, "return;") || !strings.Contains(body, "finally") {
			t.Errorf("%s is not held while it is being done", name)
		}
	}
}

// Two ways the page used to look fine and be wrong: a board that could not be
// read looked like a board with nothing on it, and a daemon that had stopped
// looked like one still being watched.
func TestAPageThatCannotReadOrReachTheDaemonSaysSo(t *testing.T) {
	index, app := page(t, "index.html"), page(t, "app.js")

	refresh := method(t, app, "async refresh() {")
	if !strings.Contains(refresh, "this.loadErr = ") || !strings.Contains(refresh, "this.sessLoaded = true") {
		t.Error("a failed read of the sessions is not kept where the page can say so")
	}
	// Every sentence that says there is nothing here waits for a read that worked.
	for _, sentence := range []string{"目前沒有事在等你。", "還沒有排程。"} {
		line := regexp.MustCompile(`(?m)^.*` + regexp.QuoteMeta(sentence) + `.*$`).FindString(index)
		if line == "" || !strings.Contains(line, "boardKnown") {
			t.Errorf("%q is said without knowing the board was read: %s", sentence, line)
		}
	}
	if !strings.Contains(index, "還沒有對話。") || !strings.Contains(index, "!list.length && boardKnown") {
		t.Error("an empty deployment is not told apart from an empty filter, or is said before the read")
	}

	// The credential ends: the whole page starts over rather than keeping the
	// settings and the approvals of a session that is over behind a cover.
	if fail := method(t, app, "fail(e, quiet) {"); !strings.Contains(fail, "location.reload()") {
		t.Error("a credential that has ended leaves the page's contents where they were")
	}
	// A refused action says it was not sent; a poll does not shout.
	if !strings.Contains(app, "連不上 daemon，這個動作沒有送出") {
		t.Error("an action that could not be sent says nothing")
	}
	if !strings.Contains(index, `:class="{open: drawer, off: !online}"`) || !strings.Contains(index, "staleText") {
		t.Error("the page is not marked old when the daemon cannot be reached")
	}
}

// A value of the wrong kind is refused by the daemon outright, not listed among
// its problems. It has to land on the field like any other problem, or the
// write button stays held with no word on why.
func TestAValueTheDaemonRefusesOutrightIsShownOnItsField(t *testing.T) {
	app := page(t, "app.js")

	check := method(t, app, "async runCheck() {")
	for _, want := range []string{`e.code !== "invalid_argument"`, `this.check = { problems: [nice({ key, why, fix: "" })]`} {
		if !strings.Contains(check, want) {
			t.Errorf("runCheck does not turn a refused value into a problem: missing %s", want)
		}
	}
}

// A key box opened while there was no file must not replace one that appears
// before it is saved.
func TestAKeySavedAfterOpeningTheBoxOnNoFileLeavesANewFileAlone(t *testing.T) {
	app := page(t, "app.js")

	for _, want := range []string{"body.expectAbsent = true", `c.fileState === "CREDENTIAL_FILE_STATE_ABSENT"`} {
		if !strings.Contains(app, want) {
			t.Errorf("app.js does not hold a save to the file that was not there: missing %s", want)
		}
	}
}

// The ChatGPT card handles two things that must not leak: the authorization
// address, which is for the operator's own browser, and the address the browser
// ended on, which carries a one-time code. Neither becomes markup or a link,
// the page opens the first only when it is OpenAI's, the second lives in the
// box it was pasted into until the call returns, and no reply carries a token.
func TestTheChatGPTCardKeepsAddressesAndTokensOutOfThePage(t *testing.T) {
	index, app := page(t, "index.html"), page(t, "app.js")

	// Opened by the page only when the origin is exactly OpenAI's, and nowhere else.
	open := method(t, app, "openAuthorizeUrl(url) {")
	check, call := strings.Index(open, `u.origin !== "https://auth.openai.com"`), strings.Index(open, "window.open(")
	if check < 0 || call < check || !strings.Contains(open[check:call], "return") {
		t.Errorf("openAuthorizeUrl does not return before window.open unless the origin is https://auth.openai.com: %s", open)
	}
	// "noopener" in the features string makes window.open return null even when
	// the tab opens, so the handle is cut off after the call instead.
	if strings.Contains(open, `"noopener`) || !strings.Contains(open[call:], "w.opener = null") {
		t.Error("the authorization page is opened with a handle back to the dashboard")
	}
	if got := strings.Count(app, "window.open("); got != 1 {
		t.Errorf("app.js opens a window in %d places, and the one that checks the origin is the only one", got)
	}
	if strings.Contains(index, "window.open") || strings.Contains(index, "location.href") {
		t.Error("index.html opens or navigates somewhere itself")
	}

	// Shown as text in a <code>, and in no attribute.
	if got := strings.Count(index, `<code class="g-url" x-text="gptFlow.url"></code>`); got != 1 {
		t.Errorf("index.html shows the authorization address as text in %d places, and one is meant", got)
	}
	if got := strings.Count(index, "gptFlow.url"); got != 1 {
		t.Errorf("index.html mentions the authorization address %d times, and only the x-text is meant", got)
	}

	// The address carries this attempt's state and the deployment's host id, so it
	// is only on the page while the sign-in waits: every way a sign-in ends without
	// succeeding clears it (and a success drops the whole flow).
	if got := strings.Count(app, `f.url = ""`); got != 3 {
		t.Errorf("app.js clears the authorization address in %d of the 3 places a sign-in fails or expires", got)
	}
	if !strings.Contains(index, `<div x-show="gptFlow.state==='pending'">
                    <p class="nm">在瀏覽器開啟這個網址，完成登入</p>`) {
		t.Error("the authorization address is not shown only while the sign-in waits")
	}

	// The paste-back box: not bound to anything, read once, cleared in a finally.
	box := regexp.MustCompile(`<input[^>]*placeholder="貼上瀏覽器網址列的網址"[^>]*>`).FindAllString(index, -1)
	if len(box) != 1 {
		t.Fatalf("the page has %d paste-back boxes, and one is meant", len(box))
	}
	for _, binding := range []string{"x-model", ":value", "x-bind:value", "@input", "x-on:input", "x-ref", `type="password"`} {
		if strings.Contains(box[0], binding) {
			t.Errorf("the paste-back box has %s, which would keep the address in the page's state or hide it from the person pasting it", binding)
		}
	}
	if !strings.Contains(box[0], `autocomplete="off"`) {
		t.Error("the paste-back box lets the browser keep what is pasted")
	}
	if got := strings.Count(app, "redirectedAddress: box.value"); got != 1 {
		t.Errorf("app.js sends a pasted address from %d places, and one is meant", got)
	}
	rest := app
	for _, allowed := range []string{"!box.value.trim()", `box.value = ""`, "redirectedAddress: box.value"} {
		rest = strings.ReplaceAll(rest, allowed, "")
	}
	if strings.Contains(rest, "box.value") {
		t.Error("app.js reads the paste-back box in a way that could keep what is pasted")
	}
	if !regexp.MustCompile(`finally \{ box\.value = ""`).MatchString(app) {
		t.Error("app.js does not clear the paste-back box in a finally")
	}

	// Exactly the six calls the service offers, each from one place; nothing reads a token.
	used := make(map[string]int)
	for _, call := range regexp.MustCompile(`API\.call\("ChatGPTAuthService", "(\w+)"`).FindAllStringSubmatch(app, -1) {
		used[call[1]]++
	}
	for _, name := range []string{"GetChatGPTAccount", "StartChatGPTSignIn", "GetChatGPTSignIn", "CompleteChatGPTSignIn", "SignOutChatGPT", "ListChatGPTModels"} {
		if used[name] != 1 {
			t.Errorf("app.js calls ChatGPTAuthService/%s in %d places, and one is meant", name, used[name])
		}
		delete(used, name)
	}
	for name := range used {
		t.Errorf("app.js calls ChatGPTAuthService/%s, which the page was not built against", name)
	}
	// (accessTokenExpiresAt is a time, which is not a token.)
	if leak := regexp.MustCompile(`\b(accessToken|refreshToken|idToken|access_token|refresh_token|id_token)\b`).FindString(app); leak != "" {
		t.Errorf("app.js reads %s, and no reply of the sign-in service carries one", leak)
	}

	// Signing out is behind a dialog that says what it does; the models are asked
	// for once, not with every poll (each asks ChatGPT).
	if got := strings.Count(index, `@click="gptSignOut()"`); got != 1 {
		t.Errorf("index.html signs out from %d places, and the dialog is the one", got)
	}
	if !strings.Contains(index, `@click="askGptOut()"`) {
		t.Error("the sign-out button does not open the confirmation")
	}
	if !strings.Contains(index, "撤銷") || !strings.Contains(index, "清除這台機器上存的登入資料") {
		t.Error("the sign-out confirmation does not say what it revokes and clears")
	}
	if got := strings.Count(method(t, app, "async loadGpt() {"), "this.loadGptModels()"); got != 1 {
		t.Errorf("loadGpt asks for the model list in %d places", got)
	}
	if strings.Contains(method(t, app, "tick() {"), "loadGptModels") {
		t.Error("the page's poll asks ChatGPT for the model list")
	}

	// Said, not guessed: where limits live, and no number of the page's own.
	if !strings.Contains(index, "Settings → Usage") {
		t.Error("the card does not point to ChatGPT Settings → Usage")
	}
	if strings.Contains(index+app, "resetsAt") || strings.Contains(index+app, "重置時間：") {
		t.Error("the page shows a reset time, which OpenAI documents no way to know")
	}
}

// The look of the settings page is defined in one place, at the top of the
// stylesheet, so that a card, a toggle and a pill are the same everywhere.
func TestTheSettingsPageTokensAreDefinedOnce(t *testing.T) {
	css := page(t, "styles.css")

	root, _, found := strings.Cut(css, "}")
	if !found || !strings.HasPrefix(root, ":root{") {
		t.Fatal("styles.css does not start with its :root tokens")
	}
	for _, token := range []string{"--r-card:", "--r-pill:", "--tog-w:", "--tog-h:", "--pill:"} {
		if strings.Count(css, token) != 1 || !strings.Contains(root, token) {
			t.Errorf("%s is not defined exactly once, in the first :root", token)
		}
	}
}

// method is the text of one of app.js's methods, from its name to the closing
// brace the file's layout gives it: a method on one line ends with the line.
func method(t *testing.T, script, name string) string {
	t.Helper()

	start := strings.Index(script, "\n      "+name)
	if start < 0 {
		t.Fatalf("app.js has no %s, and this test reads it", name)
	}
	rest := script[start+1:]
	first, _, _ := strings.Cut(rest, "\n")
	if !strings.HasSuffix(first, "{") {
		return first
	}
	end := strings.Index(rest, "\n      },")
	if end < 0 {
		t.Fatalf("app.js has no end for %s", name)
	}
	return rest[:end]
}

// ── the schema ─────────────────────────────────────────────────────────

type option struct {
	V string `json:"v"`
}

type field struct {
	Key     string   `json:"key"`
	Zh      string   `json:"zh"`
	Type    string   `json:"type"`
	Kind    string   `json:"kind"`
	Def     any      `json:"def"`
	Help    string   `json:"help"`
	Options []option `json:"options"`
	Item    *struct {
		Title  string  `json:"title"`
		Fields []field `json:"fields"`
	} `json:"item"`
}

type block struct {
	When *struct {
		Key string `json:"key"`
		Eq  string `json:"eq"`
	} `json:"when"`
	Fields []field `json:"fields"`
}

type schema struct {
	Sections []struct {
		ID     string  `json:"id"`
		Blocks []block `json:"blocks"`
	} `json:"sections"`
}

func loadSchema(t *testing.T) schema {
	t.Helper()

	file, err := webui.Assets().Open("schema.js")
	if err != nil {
		t.Fatalf("open the schema: %v", err)
	}
	defer func() { _ = file.Close() }()

	raw, err := io.ReadAll(file)
	if err != nil {
		t.Fatal(err)
	}

	// The file is JSON assigned to a name, so the page can read it without
	// fetching it, and so a test can read it without running a browser.
	text := string(raw)
	_, body, found := strings.Cut(text, "window.SCHEMA = ")
	if !found {
		t.Fatal("the schema does not assign window.SCHEMA")
	}

	var loaded schema
	if err := json.Unmarshal([]byte(strings.TrimSpace(strings.TrimSuffix(strings.TrimSpace(body), ";"))), &loaded); err != nil {
		t.Fatalf("the schema is not JSON: %v", err)
	}
	return loaded
}

// harnessOnly are the settings the example deliberately does not show, because
// they drive the offline provider the end-to-end checks use. A page that
// offered them would teach the wrong thing for the same reason the example
// does not. Kept the same as config_test.go's list, which is where it is
// argued.
var harnessOnly = map[string]bool{
	"provider.fake_model":     true,
	"provider.fake_delay":     true,
	"provider.fake_reasoning": true,
	"provider.fake_script":    true,
}

// bootstrapOnly are the settings the saved text cannot hold, because they say
// where it is kept: a flag or a variable gives them when the daemon starts, and
// a line for one in the text is ignored. A page that offered an input for one
// would take a value, show it as saved, and change nothing.
//
// Written out as well as read from config.BootstrapKeys, so that a list that
// lost one of them could not quietly start offering it.
var bootstrapOnly = map[string]bool{
	"server.data_dir":    true,
	"server.runtime_dir": true,
}

func (s schema) fields() map[string]field {
	out := make(map[string]field)
	for _, section := range s.Sections {
		for _, b := range section.Blocks {
			for _, f := range b.Fields {
				out[f.Key] = f
			}
		}
	}
	return out
}

// kindOf is the type a schema field has to carry for a setting of this kind.
func kindOf(kind config.Kind) []string {
	if kind == config.KindString {
		return []string{"string", "enum"}
	}
	return []string{string(kind)}
}

// The page is built from this file, so a setting it does not list is one the
// page can never offer — and said nowhere. The settings come from the struct,
// the same walk that checks the example documents them.
func TestTheSchemaCoversEverySettingAndOnlyTheseSettings(t *testing.T) {
	declared := loadSchema(t).fields()

	listed := make(map[string]bool)
	for _, bootstrap := range config.BootstrapKeys() {
		listed[bootstrap.Key] = true
		if !bootstrapOnly[bootstrap.Key] {
			t.Errorf("%s cannot be saved in the settings, and this test does not list it", bootstrap.Key)
		}
	}
	for key := range bootstrapOnly {
		if !listed[key] {
			t.Errorf("%s is listed here as given at start, and the program no longer says so", key)
		}
	}

	known := make(map[string]config.Setting)
	for _, setting := range config.Settings() {
		known[setting.Key] = setting

		if bootstrapOnly[setting.Key] {
			if _, offered := declared[setting.Key]; offered {
				t.Errorf("%s is given by a flag or a variable, not by the saved settings, and should not be offered", setting.Key)
			}
			continue
		}
		if harnessOnly[setting.Key] {
			if _, offered := declared[setting.Key]; offered {
				t.Errorf("%s is for the test double and should not be offered", setting.Key)
			}
			continue
		}

		f, ok := declared[setting.Key]
		if !ok {
			t.Errorf("the schema does not describe %s", setting.Key)
			continue
		}

		if !contains(kindOf(setting.Kind), f.Type) {
			t.Errorf("%s is a %s in the program and a %s in the schema", setting.Key, setting.Kind, f.Type)
		}
		if f.Zh == "" || f.Help == "" {
			t.Errorf("%s has no name or no explanation", setting.Key)
		}

		if setting.Kind == config.KindList {
			if f.Item == nil {
				t.Errorf("%s is a list and the schema does not say what an element is", setting.Key)
				continue
			}
			checkElement(t, setting, *f.Item)
			continue
		}
		if !sameJSON(setting.Default, f.Def) {
			t.Errorf("%s defaults to %v in the program and %v in the schema", setting.Key, setting.Default, f.Def)
		}
	}

	for key := range declared {
		if _, ok := known[key]; !ok {
			t.Errorf("the schema describes %s, which is not a setting", key)
		}
	}
}

func checkElement(t *testing.T, list config.Setting, item struct {
	Title  string  `json:"title"`
	Fields []field `json:"fields"`
}) {
	t.Helper()

	inner := make(map[string]field, len(item.Fields))
	for _, f := range item.Fields {
		inner[f.Key] = f
	}

	for _, setting := range list.Items {
		f, ok := inner[setting.Key]
		if !ok {
			t.Errorf("%s: the schema does not describe %s of an element", list.Key, setting.Key)
			continue
		}
		if !contains(kindOf(setting.Kind), f.Type) {
			t.Errorf("%s[].%s is a %s in the program and a %s in the schema", list.Key, setting.Key, setting.Kind, f.Type)
		}
		if !sameJSON(setting.Default, f.Def) {
			t.Errorf("%s[].%s defaults to %v in the program and %v in the schema", list.Key, setting.Key, setting.Default, f.Def)
		}
		delete(inner, setting.Key)
	}
	for key := range inner {
		t.Errorf("%s: the schema describes %s of an element, which is not a setting", list.Key, key)
	}
	if _, ok := inner[item.Title]; !ok && item.Title != "" {
		found := false
		for _, setting := range list.Items {
			found = found || setting.Key == item.Title
		}
		if !found {
			t.Errorf("%s names %s as the title of an element, which it does not have", list.Key, item.Title)
		}
	}
}

// Each setting that names a credential file gets the controls for putting a key
// there. The page finds a slot by the setting that names its file, so a setting
// the schema offers as a file name and no slot is kept in (or the other way
// round) is a field with no controls, or controls with no field.
func TestEverySecretFileSettingIsACredentialSlotAndTheOtherWayRound(t *testing.T) {
	offered := make(map[string]bool)
	for key, f := range loadSchema(t).fields() {
		if f.Kind == "secretfile" {
			offered[key] = true
		}
	}

	slots := (config.Config{}).CredentialFiles()
	if len(slots) != 8 || len(offered) != 8 {
		t.Errorf("%d credential slots and %d file names in the schema, want 8 of each", len(slots), len(offered))
	}
	for _, slot := range slots {
		if !offered[slot.Key] {
			t.Errorf("%s names a credential file, and the schema does not offer it as one", slot.Key)
		}
		delete(offered, slot.Key)
	}
	for key := range offered {
		t.Errorf("the schema offers %s as a credential file name, and no credential slot is kept in it", key)
	}
}

// A block that waits on a setting that is not there is shown never, or always.
func TestEveryBlockThatWaitsOnASettingWaitsOnOneThatExists(t *testing.T) {
	loaded := loadSchema(t)
	declared := loaded.fields()

	for _, section := range loaded.Sections {
		for _, b := range section.Blocks {
			if b.When == nil {
				continue
			}
			if _, ok := declared[b.When.Key]; !ok {
				t.Errorf("a block in %s waits on %s, which is not described", section.ID, b.When.Key)
			}
		}
	}
}

// Every choice the page offers has to be one the daemon accepts. A dropdown
// with an option that fails validation is a page that invites its own error.
func TestEveryChoiceThePageOffersIsAcceptedByTheDaemon(t *testing.T) {
	loaded := loadSchema(t)

	// The settings a choice only means something under, so the check that is
	// being asked of it is actually made.
	gates := make(map[string]config.Edit)
	for _, section := range loaded.Sections {
		for _, b := range section.Blocks {
			if b.When != nil {
				for _, f := range b.Fields {
					gates[f.Key] = config.Edit{Key: b.When.Key, Value: b.When.Eq}
				}
			}
		}
	}

	var checked int
	for _, section := range loaded.Sections {
		for _, b := range section.Blocks {
			for _, f := range b.Fields {
				if f.Type != "enum" {
					continue
				}
				for _, o := range f.Options {
					edits := []config.Edit{{Key: f.Key, Value: o.V}}
					if gate, ok := gates[f.Key]; ok && gate.Key != f.Key {
						edits = append(edits, gate)
					}

					_, problems, err := config.Preview([]byte(config.Example), edits)
					if err != nil {
						t.Errorf("%s = %q could not be written: %v", f.Key, o.V, err)
						continue
					}
					for _, problem := range problems {
						if problem.Key == f.Key {
							t.Errorf("%s = %q is offered, and the daemon refuses it: %s", f.Key, o.V, problem.Why)
						}
					}
					checked++
				}
			}
		}
	}
	if checked < 20 {
		t.Errorf("only %d choices were checked", checked)
	}

	// The same for a choice inside a list's element.
	for _, section := range loaded.Sections {
		for _, b := range section.Blocks {
			for _, f := range b.Fields {
				if f.Item == nil {
					continue
				}
				for _, inner := range f.Item.Fields {
					if inner.Type != "enum" {
						continue
					}
					for _, o := range inner.Options {
						element := map[string]any{"name": "x", "command": "x", inner.Key: o.V}
						if f.Key != "mcp.servers" {
							continue
						}
						_, problems, err := config.Preview([]byte(config.Example), []config.Edit{{Key: f.Key, Value: []any{element}}})
						if err != nil {
							t.Errorf("%s[].%s = %q could not be written: %v", f.Key, inner.Key, o.V, err)
							continue
						}
						for _, problem := range problems {
							if strings.HasSuffix(problem.Key, "."+inner.Key) {
								t.Errorf("%s[].%s = %q is offered, and the daemon refuses it: %s", f.Key, inner.Key, o.V, problem.Why)
							}
						}
					}
				}
			}
		}
	}
}

func contains(list []string, want string) bool {
	for _, got := range list {
		if got == want {
			return true
		}
	}
	return false
}

// sameJSON compares as JSON does, so 12 and 12.0 are the same default and an
// absent list is an empty one.
func sameJSON(a, b any) bool {
	normal := func(v any) any {
		if v == nil {
			return []any{}
		}
		raw, _ := json.Marshal(v)
		var out any
		_ = json.Unmarshal(raw, &out)
		return out
	}
	return reflect.DeepEqual(normal(a), normal(b))
}

// Small behaviours of the settings page that were wrong once, kept from coming
// back. They read the source, as the guards above do; the browser flows cover
// what they do.
func TestTheSettingsPageKeepsItsSmallFixes(t *testing.T) {
	app := page(t, "app.js")

	// A saved list item lacks the defaults the running one has; both sides are
	// filled before they are compared, or every edit marks them pending.
	pending := method(t, app, "pendingKey(key) {")
	if strings.Count(pending, "this.filled(") != 2 {
		t.Errorf("pendingKey does not fill both the saved and the running value: %s", pending)
	}
	// Escape clears the search before it closes the page.
	esc := method(t, app, "esc() {")
	if q, drawer := strings.Index(esc, "this.q = \"\""), strings.LastIndex(esc, "this.drawer = false"); q < 0 || q > drawer {
		t.Errorf("Escape closes the page before it clears the search: %s", esc)
	}
	// A sign-in done in repair mode settles the problem that asked for it.
	if fixed := method(t, app, "fixed(p) {"); !strings.Contains(fixed, "this.gpt.signedIn") {
		t.Errorf("a ChatGPT sign-in does not mark the startup problem fixed: %s", fixed)
	}
}
