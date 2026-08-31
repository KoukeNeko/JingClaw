package tui

import (
	"context"
	"fmt"
	"strings"
	"time"

	tea "charm.land/bubbletea/v2"

	"github.com/KoukeNeko/JingClaw/core/internal/domain"
)

// where the panel is: choosing a session, or inside one.
type where int

const (
	inTheList where = iota
	inASession
)

// answerPrompt marks the one line a person may type at.
//
// Only drawn while a run is parked on a question. A panel with a permanent
// input invites a turn to be typed into it, and a turn typed here would be a
// run with no origin — no platform authenticated anybody, and nowhere to
// answer. The CLI keeps "send" for when somebody means it.
const answerPrompt = "> "

// crashAfter is long enough for the first frame to have reached the terminal,
// which is what the staged failure is there to happen after.
const crashAfter = 200 * time.Millisecond

// panel is the whole screen.
//
// One model rather than one per screen. What it shows is a function of where
// it is, and splitting that into separate models would mean two copies of the
// keys, the size, and the last thing that went wrong.
type panel struct {
	// ctx is the panel's own lifetime, which the watching goroutine ends
	// with. Held on the model because commands are started from Update,
	// which has nothing else to hand them.
	ctx      context.Context
	sessions Sessions

	// opener is what hands a file to the machine, and where the file goes.
	// Held so a check can watch what would be opened without anything being
	// launched.
	opener Opener
	into   string

	where  where
	list   []Summary
	cursor int

	open   domain.SessionID
	screen Screen

	// typing is the answer being written, empty when nothing is.
	typing string

	// updates is the open stream for the session being shown, nil when none
	// is open.
	updates <-chan Update

	// trouble is the last thing that went wrong, drawn rather than swallowed.
	// A panel that hid this would look like a daemon with nothing happening.
	trouble string

	width  int
	height int

	panicNow bool
}

// newPanel is a panel that has not asked for anything yet.
func newPanel(ctx context.Context, sessions Sessions, opener Opener, into string) panel {
	return panel{ctx: ctx, sessions: sessions, opener: opener, into: into}
}

// Messages the panel sends itself.
type (
	// listed is the session list arriving.
	listed struct {
		sessions []Summary
		err      error
	}

	// showing is a session that has been opened and is ready to draw.
	showing struct {
		id     domain.SessionID
		screen Screen
		err    error
	}

	// arrived is one thing that happened in the session being watched.
	arrived struct {
		update Update
	}

	// decided is a decision having been sent, or refused.
	//
	// Only the outcome. What the decision did to the session arrives as an
	// event like anything else, so the panel does not update its own screen
	// from a reply and then again from the log.
	decided struct {
		err error
	}

	// crashNow is the staged failure arriving, once there is a screen to lose.
	crashNow struct{}
)

func (p panel) Init() tea.Cmd {
	if p.panicNow {
		// After a frame, not during the first one. The screen is taken by the
		// view that asks for it, so failing before that leaves nothing to put
		// back — and a check against it would pass without proving anything.
		return tea.Tick(crashAfter, func(time.Time) tea.Msg { return crashNow{} })
	}
	if p.sessions == nil {
		return nil
	}
	return p.loadTheList()
}

func (p panel) Update(message tea.Msg) (tea.Model, tea.Cmd) {
	switch message := message.(type) {
	case crashNow:
		panic("staged, to see what a crash leaves behind")

	case tea.WindowSizeMsg:
		p.width, p.height = message.Width, message.Height

	case listed:
		if message.err != nil {
			p.trouble = message.err.Error()
			return p, nil
		}
		p.list = message.sessions
		p.trouble = ""
		if p.cursor >= len(p.list) {
			p.cursor = 0
		}

	case showing:
		if message.err != nil {
			p.trouble = message.err.Error()
			return p, nil
		}
		p.where = inASession
		p.open = message.id
		p.screen = message.screen
		p.trouble = ""
		p.updates = p.watch()
		return p, p.nextUpdate()

	case arrived:
		return p.fold(message.update)

	case decided:
		// A refusal is drawn rather than dropped. Reporting success on one
		// would leave somebody believing they had unblocked a run that is
		// still waiting on them.
		if message.err != nil {
			p.trouble = message.err.Error()
		}
		return p, nil

	case tea.KeyPressMsg:
		return p.pressed(message)
	}

	return p, nil
}

// pressed is every key the panel answers to.
func (p panel) pressed(key tea.KeyPressMsg) (tea.Model, tea.Cmd) {
	// A question in front of somebody turns the panel into a line to type at.
	// The keys that decide are single letters, and a person answering "add
	// the flag" must not allow a call by writing it.
	if p.isAnswering() {
		return p.typed(key)
	}

	switch key.String() {
	case "ctrl+c":
		return p, tea.Quit

	case "q":
		// Only from the list. Inside a session the same key would quit while
		// somebody is reading, when what they meant was to go back.
		if p.where == inTheList {
			return p, tea.Quit
		}

	case "esc":
		if p.where == inASession {
			return p.leaveTheSession(), nil
		}
		return p, tea.Quit

	case "ctrl+z":
		// Handed to the framework rather than left to the terminal.
		//
		// In raw mode the driver does not turn ctrl-z into SIGTSTP: it
		// arrives here as a key, and a panel that ignores it is one where the
		// usual way of stepping out does nothing. Suspending through the
		// framework is what gives the shell its terminal back and takes it
		// again on the way in.
		//
		// An external SIGTSTP is not handled, and that is a limit rather than
		// an oversight. Restoring from a signal handler means racing the
		// framework for the same terminal, and getting that wrong leaves the
		// mess it was meant to prevent.
		return p, tea.Suspend

	case "up", "k":
		if p.where == inTheList && p.cursor > 0 {
			p.cursor--
		}

	case "down", "j":
		if p.where == inTheList && p.cursor < len(p.list)-1 {
			p.cursor++
		}

	case "enter":
		if p.where == inTheList && p.cursor < len(p.list) {
			return p, p.openSession(p.list[p.cursor].ID)
		}

	case "r":
		if p.where == inTheList {
			return p, p.loadTheList()
		}

	case "a":
		return p, p.decide(domain.ApprovalAllowed, domain.RememberOnce)

	case "A":
		return p, p.decide(domain.ApprovalAllowed, domain.RememberSession)

	case "d":
		return p, p.decide(domain.ApprovalDenied, domain.RememberOnce)

	case "i":
		return p, p.interrupt()

	case "o":
		return p, p.openTheNewestOutput()
	}

	return p, nil
}

// isAnswering says whether the next keystroke is text rather than a command.
func (p panel) isAnswering() bool {
	return p.where == inASession && len(p.screen.Asked) > 0
}

// typed puts a keystroke into the answer being written.
//
// Escape and ctrl+c still mean what they mean: a person who opened a session
// by mistake must be able to leave it without answering a question they know
// nothing about.
func (p panel) typed(key tea.KeyPressMsg) (tea.Model, tea.Cmd) {
	switch key.String() {
	case "ctrl+c":
		return p, tea.Quit

	case "esc":
		return p.leaveTheSession(), nil

	case "enter":
		return p.sendTheAnswer()

	case "backspace":
		if p.typing != "" {
			runes := []rune(p.typing)
			p.typing = string(runes[:len(runes)-1])
		}
		return p, nil
	}

	// Text rather than the key's name: a key with no text behind it — an
	// arrow, a function key — would otherwise be typed into the answer as the
	// word for it.
	p.typing += key.Text
	return p, nil
}

// sendTheAnswer unblocks the run, when there is anything to send.
//
// Nothing for an empty line. A run parked on a question is unblocked by an
// answer, and "" is one it cannot act on: sending it turns a run waiting on a
// person into a run that carried on with nothing.
func (p panel) sendTheAnswer() (tea.Model, tea.Cmd) {
	if strings.TrimSpace(p.typing) == "" || p.sessions == nil {
		return p, nil
	}

	sessions, ctx := p.sessions, p.ctx
	answer := Answer{ID: p.screen.Asked[0].ID, Text: p.typing}

	// Cleared here rather than when the daemon replies. The next question
	// would otherwise open with the previous answer already in it, and enter
	// would send it before anybody had read what was being asked.
	p.typing = ""

	return p, func() tea.Msg {
		return decided{err: sessions.Answer(ctx, answer)}
	}
}

// decide answers the request at the front of the queue.
//
// Nothing when nothing is waiting, rather than the last thing that was: a
// stray keypress on a quiet session must not send a decision about a call
// that has already been settled.
func (p panel) decide(status domain.ApprovalStatus, scope domain.RememberScope) tea.Cmd {
	if p.where != inASession || len(p.screen.Waiting) == 0 || p.sessions == nil {
		return nil
	}

	sessions, ctx := p.sessions, p.ctx
	decision := Decision{ID: p.screen.Waiting[0].ID, Status: status, Scope: scope}
	return func() tea.Msg {
		return decided{err: sessions.Decide(ctx, decision)}
	}
}

// interrupt stops the run in flight, when there is one.
func (p panel) interrupt() tea.Cmd {
	if p.where != inASession || p.screen.ActiveRun == "" || p.sessions == nil {
		return nil
	}

	sessions, ctx, run := p.sessions, p.ctx, p.screen.ActiveRun
	return func() tea.Msg {
		return decided{err: sessions.Interrupt(ctx, run)}
	}
}

// openTheNewestOutput writes stored output out and hands it to the machine.
//
// Not rendered here. A terminal is a poor image viewer and a worse PDF
// reader, and the program that already knows how to open a file is the one
// the person configured for it.
//
// The newest, which is the one being read about: a run that has just failed
// has its log at the bottom of the screen, and offering a cursor over every
// call a session ever made would be a second selection to keep in step with
// what is drawn.
func (p panel) openTheNewestOutput() tea.Cmd {
	stored, found := newestOutput(p.screen.Messages)
	if p.where != inASession || !found || p.sessions == nil || p.opener == nil {
		return nil
	}

	extension, openable := extensionFor(stored.MediaType)
	if !openable {
		// Refused rather than opened as something else. An artifact is
		// whatever a tool produced, which includes whatever a page the run
		// read suggested it produce, and handing that to the machine's
		// default program for it is running somebody else's file.
		return func() tea.Msg {
			return decided{err: fmt.Errorf(
				"tui: %s is a %s, which this panel will not hand to the machine",
				stored.Artifact, stored.MediaType)}
		}
	}

	sessions, ctx, opener, into := p.sessions, p.ctx, p.opener, p.into
	return func() tea.Msg {
		data, err := sessions.ReadArtifact(ctx, stored.Artifact)
		if err != nil {
			return decided{err: err}
		}

		path, err := writeForOpening(into, stored.Artifact, extension, data)
		if err != nil {
			return decided{err: err}
		}
		return decided{err: opener.Open(ctx, path)}
	}
}

// newestOutput is the last call in the session that stored anything.
func newestOutput(messages []Message) (ToolCall, bool) {
	for message := len(messages) - 1; message >= 0; message-- {
		calls := messages[message].ToolCalls
		for call := len(calls) - 1; call >= 0; call-- {
			if calls[call].Artifact != "" {
				return calls[call], true
			}
		}
	}
	return ToolCall{}, false
}

// leaveTheSession goes back to the list and stops watching.
//
// The stream is dropped rather than kept warm. Holding it would mean the
// panel accumulating one goroutine per session anybody looked at, each still
// folding events onto a screen nobody is reading.
func (p panel) leaveTheSession() panel {
	p.where = inTheList
	p.open = ""
	p.screen = Screen{}
	p.updates = nil
	p.typing = ""
	return p
}

// fold applies one update from the stream.
func (p panel) fold(update Update) (tea.Model, tea.Cmd) {
	switch {
	case update.Err != nil:
		p.trouble = update.Err.Error()

	case update.OldestSeq != 0:
		// Said rather than swallowed. The events between where the panel was
		// and what the daemon still holds are gone, and a screen that quietly
		// resumed would have a hole in it nobody could see.
		p.trouble = fmt.Sprintf(
			"missed events that have since been discarded; showing from %d",
			update.OldestSeq)
		p.screen.HeadSeq = update.OldestSeq - 1

	case update.Event != nil:
		p.screen = Fold(p.screen, *update.Event)
	}

	return p, p.nextUpdate()
}

// loadTheList asks for the sessions.
func (p panel) loadTheList() tea.Cmd {
	sessions := p.sessions
	ctx := p.ctx
	return func() tea.Msg {
		found, err := sessions.List(ctx)
		return listed{sessions: found, err: err}
	}
}

// openSession asks for one session as it stands.
func (p panel) openSession(id domain.SessionID) tea.Cmd {
	sessions := p.sessions
	ctx := p.ctx
	return func() tea.Msg {
		screen, err := sessions.Open(ctx, id)
		return showing{id: id, screen: screen, err: err}
	}
}

// watch opens the stream from where the view stopped.
//
// From HeadSeq and not from zero: the view already accounts for everything up
// to it, and starting again from the beginning would fold every message onto
// a screen that already has it.
func (p panel) watch() <-chan Update {
	if p.sessions == nil {
		return nil
	}
	return p.sessions.Watch(p.ctx, p.open, p.screen.HeadSeq)
}

// nextUpdate waits for the next thing on the open stream.
//
// Issued again after every update, because a command runs once. A panel that
// asked only when the stream opened would draw the first thing that happened
// and then sit there while the session moved on.
func (p panel) nextUpdate() tea.Cmd {
	updates := p.updates
	if updates == nil {
		return nil
	}
	return func() tea.Msg {
		update, open := <-updates
		if !open {
			return nil
		}
		return arrived{update: update}
	}
}

func (p panel) View() tea.View {
	drawn := &strings.Builder{}

	switch p.where {
	case inASession:
		drawTheSession(drawn, p.screen)
		if p.isAnswering() {
			fmt.Fprintf(drawn, "\n%s%s\n", answerPrompt, p.typing)
		}
	default:
		drawTheList(drawn, p.list, p.cursor)
	}

	if p.trouble != "" {
		fmt.Fprintf(drawn, "\n%s\n", p.trouble)
	}
	fmt.Fprintf(drawn, "\n%s\n", p.keys())

	view := tea.NewView(drawn.String())
	view.AltScreen = true
	return view
}

// keys is the line saying what the panel answers to, which changes with where
// it is. A panel that showed every key everywhere would offer "back" on the
// screen there is nothing to go back from.
func (p panel) keys() string {
	if p.where == inASession {
		// While there is a line to type at, the decision keys are not
		// offered: they are letters, and letters are going into the answer.
		if p.isAnswering() {
			return "enter answers · esc back · ctrl+c quit"
		}

		keys := "esc back · ctrl+c quit"
		if _, found := newestOutput(p.screen.Messages); found {
			keys = "o open output · " + keys
		}
		if p.screen.ActiveRun != "" {
			keys = "i interrupt · " + keys
		}
		if len(p.screen.Waiting) > 0 {
			keys = "a allow · A allow this session · d deny · " + keys
		}
		return keys
	}
	return "↑↓ move · enter open · r refresh · q quit"
}

func drawTheList(drawn *strings.Builder, list []Summary, cursor int) {
	drawn.WriteString("Sessions\n\n")

	if len(list) == 0 {
		drawn.WriteString("  no sessions yet\n")
		return
	}

	for index, session := range list {
		marker := "  "
		if index == cursor {
			marker = "> "
		}
		fmt.Fprintf(drawn, "%s%s\n", marker, nameOf(session))
	}
}

// nameOf is what a row says. A session takes its title from its first turn,
// so one that has not had a turn yet has none and is drawn by id instead: a
// blank row is one nobody can tell from the next blank row.
func nameOf(session Summary) string {
	if session.Title != "" {
		return session.Title
	}
	return string(session.ID)
}

func drawTheSession(drawn *strings.Builder, screen Screen) {
	if screen.ActiveRun != "" {
		fmt.Fprintf(drawn, "running: %s\n\n", screen.ActiveRun)
	}

	for _, message := range screen.Messages {
		drawTheTurn(drawn, message)
	}

	if len(screen.Waiting) > 0 {
		drawWhatIsWaiting(drawn, screen.Waiting)
	}
	if len(screen.Asked) > 0 {
		drawWhatWasAsked(drawn, screen.Asked[0])
	}
}

// drawWhatWasAsked shows the question a run parked itself on.
func drawWhatWasAsked(drawn *strings.Builder, asked Asked) {
	fmt.Fprintf(drawn, "\nit asked: %s\n", asked.Prompt)

	// The options as offered. A question with a fixed set answered in free
	// text is one the run rejects, and the person typing finds out after.
	for _, option := range asked.Options {
		fmt.Fprintf(drawn, "  · %s\n", option.Label)
		if option.Detail != "" {
			fmt.Fprintf(drawn, "    %s\n", option.Detail)
		}
	}
}

// drawWhatIsWaiting shows the request being decided, and how many are behind
// it.
//
// One at a time, and the oldest first. Every key that decides acts on this
// one, so there is never a question of which request a keypress was about —
// the alternative is a second cursor and a way to get it out of step with
// what is on the screen.
func drawWhatIsWaiting(drawn *strings.Builder, requests []Waiting) {
	asked := requests[0]

	fmt.Fprintf(drawn, "\nwaiting on you: %s\n", asked.ToolName)
	if asked.Summary != "" {
		fmt.Fprintf(drawn, "  %s\n", asked.Summary)
	}
	if asked.Preview != "" {
		fmt.Fprintf(drawn, "\n%s\n\n", asked.Preview)
	}
	for _, effect := range asked.Effects {
		fmt.Fprintf(drawn, "  · %s\n", effect)
	}

	// Said only when it is true. A mark on every request is one nobody reads.
	if asked.ReadForeign {
		drawn.WriteString(
			"\n  this run had read text somebody else wrote before asking\n")
	}

	if behind := len(requests) - 1; behind > 0 {
		fmt.Fprintf(drawn, "\n  %d more waiting behind this one\n", behind)
	}
}

func drawTheTurn(drawn *strings.Builder, message Message) {
	who := "you"
	if message.Role != domain.RoleUser {
		who = "agent"
	}

	// The working-out first and labelled, because it is what the model wrote
	// before the answer and this is the only client it reaches. Run together
	// with the reply it would read as something the agent said.
	if message.Reasoning != "" {
		fmt.Fprintf(drawn, "%s (thinking)\n%s\n\n", who, message.Reasoning)
	}
	if message.Text != "" {
		fmt.Fprintf(drawn, "%s\n%s\n\n", who, message.Text)
	}

	for _, call := range message.ToolCalls {
		fmt.Fprintf(drawn, "  %s %s%s\n", howItWent(call), call.Name, whatItLeft(call))
	}
	if len(message.ToolCalls) > 0 {
		drawn.WriteString("\n")
	}
}

// whatItLeft marks a call whose output was too large to have been sent.
//
// Said at all because otherwise the only way to reach a build log is to know
// it exists. Said briefly because most calls leave nothing, and a column of
// mostly-empty annotations is one nobody reads.
func whatItLeft(call ToolCall) string {
	if call.Artifact == "" {
		return ""
	}
	return "  (output stored)"
}

func howItWent(call ToolCall) string {
	switch {
	case !call.Completed:
		return "..."
	case call.IsError:
		return "failed"
	default:
		return "ok"
	}
}
