package control

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"time"
	"unicode/utf8"

	"connectrpc.com/connect"
	"google.golang.org/protobuf/types/known/structpb"
	"google.golang.org/protobuf/types/known/timestamppb"

	controlv1 "github.com/KoukeNeko/JingClaw/core/gen/go/jingclaw/control/v1"
	"github.com/KoukeNeko/JingClaw/core/gen/go/jingclaw/control/v1/controlv1connect"
	"github.com/KoukeNeko/JingClaw/core/internal/config"
	"github.com/KoukeNeko/JingClaw/core/internal/storage"
)

// maxDocumentBytes is the most of any one document a write takes. The agents
// and persona documents are put in front of the model, so a megabyte is already
// far past anything meant to be read on every turn, and the config is a list
// of settings.
const maxDocumentBytes = 1 << 20

// sourceWeb is what a document saved through this service is recorded as
// having come from, in the history beside the text.
const sourceWeb = "web"

// ConfigOverride is a setting the saved text cannot change, because something
// wins over it.
type ConfigOverride struct {
	Key string
	// Source is "environment" or "flag", and Name the variable or flag as the
	// operator wrote it.
	Source string
	Name   string
}

// StartedDocuments are the hashes of the three documents as this process read
// them when it started: what is running, as opposed to what is saved.
//
// Empty for a document that had no text then, which is also what a document
// never written and one that was cleared hash to.
type StartedDocuments struct {
	Config, Agents, Persona string
}

// of is the hash the named document started from.
func (d StartedDocuments) of(name string) string {
	switch name {
	case storage.DocumentConfig:
		return d.Config
	case storage.DocumentAgents:
		return d.Agents
	case storage.DocumentPersona:
		return d.Persona
	}
	return ""
}

// AgentProblem is one reason there is no agent.
type AgentProblem struct {
	config.Problem

	// Source is "settings" for a saved setting that will not do, and "startup"
	// for anything else that kept the agent from being built.
	Source string
}

// AgentState is what the daemon says about whether it is running an agent.
//
// Fixed at start. Settings are read once, and there is no restart to ask for,
// so a daemon that came up without an agent stays that way until it is stopped
// and started again; what an operator saves in between does not change it.
type AgentState struct {
	// Repair is true for a daemon that could not build an agent and is serving
	// only what is needed to fix the settings.
	Repair   bool
	Problems []AgentProblem

	StartedAt time.Time
}

// ConfigServer lets a person read and change the settings.
//
// The settings are three documents in the database — config, agents and
// persona — and it owns none of them. Every call goes back to the database, so
// a save made through another connection between two calls is what the second
// one sees. What is running is not read from there: it is what this process
// started with, and the difference between the two is the pending restart every
// reply reports.
//
// There is no lock around a write. PutDocument compares the hash and replaces
// the text in one statement, so two writers that read the same version cannot
// both succeed, even in different processes: the second is told the document
// changed, which is what a stale hash would have told it.
type ConfigServer struct {
	documents storage.DocumentStore
	running   map[string]any

	overrides []ConfigOverride
	byKey     map[string]ConfigOverride

	started StartedDocuments
	state   AgentState
}

var _ controlv1connect.ConfigServiceHandler = (*ConfigServer)(nil)

// NewConfigServer serves the documents in the store. running is what this
// process loaded, after flags, for showing next to what is saved; started is
// the hashes it read the three documents as, for saying whether a save is
// running yet; state is whether it has an agent.
func NewConfigServer(
	documents storage.DocumentStore,
	running config.Config,
	overrides []ConfigOverride,
	started StartedDocuments,
	state AgentState,
) *ConfigServer {
	sort.Slice(overrides, func(i, j int) bool { return overrides[i].Key < overrides[j].Key })

	byKey := make(map[string]ConfigOverride, len(overrides))
	for _, override := range overrides {
		byKey[override.Key] = override
	}

	return &ConfigServer{
		documents: documents,
		running:   config.Flatten(running),
		overrides: overrides,
		byKey:     byKey,
		started:   started,
		state:     state,
	}
}

func (s *ConfigServer) GetConfig(
	ctx context.Context,
	_ *connect.Request[controlv1.GetConfigRequest],
) (*connect.Response[controlv1.GetConfigResponse], error) {
	doc, err := s.documents.Document(ctx, storage.DocumentConfig)
	if err != nil {
		return nil, toConnectError(err)
	}

	// Text that does not parse is still text. It is what the page that has to
	// repair it needs to show, so it is answered with the reason instead of
	// with an error, and with no settings.
	var parseError string
	file, err := config.FileSettings([]byte(doc.Text))
	if err != nil {
		parseError = err.Error()
	}

	fileStruct, err := structpb.NewStruct(file)
	if err != nil {
		return nil, connect.NewError(connect.CodeInternal, err)
	}
	runningStruct, err := structpb.NewStruct(s.running)
	if err != nil {
		return nil, connect.NewError(connect.CodeInternal, err)
	}

	overrides := make([]*controlv1.ConfigOverride, 0, len(s.overrides))
	for _, override := range s.overrides {
		overrides = append(overrides, &controlv1.ConfigOverride{
			Key: override.Key, Source: override.Source, Name: override.Name,
		})
	}

	return connect.NewResponse(&controlv1.GetConfigResponse{
		Hash:           doc.Hash,
		File:           fileStruct,
		Running:        runningStruct,
		Overrides:      overrides,
		Text:           doc.Text,
		ParseError:     parseError,
		RunningHash:    s.started.of(doc.Name),
		PendingRestart: s.pending(doc),
		Version:        doc.Version,
	}), nil
}

func (s *ConfigServer) CheckConfig(
	ctx context.Context,
	req *connect.Request[controlv1.CheckConfigRequest],
) (*connect.Response[controlv1.CheckConfigResponse], error) {
	edits, refused, err := s.editsOf(req.Msg.GetEdits())
	if err != nil {
		return nil, err
	}

	doc, err := s.documents.Document(ctx, storage.DocumentConfig)
	if err != nil {
		return nil, toConnectError(err)
	}

	patched, problems, err := config.Preview([]byte(doc.Text), edits)
	if err != nil {
		return nil, connect.NewError(connect.CodeInvalidArgument, err)
	}

	return connect.NewResponse(&controlv1.CheckConfigResponse{
		Problems: problemsToProto(append(refused, problems...)),
		Diff:     diffToProto(patched),
	}), nil
}

func (s *ConfigServer) WriteConfig(
	ctx context.Context,
	req *connect.Request[controlv1.WriteConfigRequest],
) (*connect.Response[controlv1.WriteConfigResponse], error) {
	edits, refused, err := s.editsOf(req.Msg.GetEdits())
	if err != nil {
		return nil, err
	}
	if len(req.Msg.GetEdits()) == 0 {
		return nil, connect.NewError(connect.CodeInvalidArgument, errors.New("there is nothing to write"))
	}

	// Read again here and not trusted from the page: the edit is applied to the
	// text as it is now, so a hash that matches means the text the page showed
	// is the text that is patched.
	doc, err := s.documents.Document(ctx, storage.DocumentConfig)
	if err != nil {
		return nil, toConnectError(err)
	}
	// Refused before anything is worked out: an edit that began from another
	// version was made against settings that are no longer there.
	if doc.Hash != req.Msg.GetHash() {
		return nil, toConnectError(storage.ErrDocumentChanged)
	}

	patched, problems, err := config.Preview([]byte(doc.Text), edits)
	if err != nil {
		return nil, connect.NewError(connect.CodeInvalidArgument, err)
	}
	if problems = append(refused, problems...); len(problems) != 0 {
		return connect.NewResponse(&controlv1.WriteConfigResponse{
			Hash:           doc.Hash,
			Problems:       problemsToProto(problems),
			PendingRestart: s.pending(doc),
			Version:        doc.Version,
		}), nil
	}

	saved, err := s.save(ctx, doc, string(patched.Text))
	if err != nil {
		return nil, toConnectError(err)
	}

	changed := make([]string, len(edits))
	for i, edit := range edits {
		changed[i] = edit.Key
	}

	return connect.NewResponse(&controlv1.WriteConfigResponse{
		Written:        true,
		Hash:           saved.Hash,
		ChangedKeys:    changed,
		PendingRestart: s.pending(saved),
		Version:        saved.Version,
	}), nil
}

func (s *ConfigServer) ReplaceConfigText(
	ctx context.Context,
	req *connect.Request[controlv1.ReplaceConfigTextRequest],
) (*connect.Response[controlv1.ReplaceConfigTextResponse], error) {
	text := req.Msg.GetText()
	if err := checkDocumentText(text); err != nil {
		return nil, err
	}

	doc, err := s.documents.Document(ctx, storage.DocumentConfig)
	if err != nil {
		return nil, toConnectError(err)
	}
	if doc.Hash != req.Msg.GetHash() {
		return nil, toConnectError(storage.ErrDocumentChanged)
	}

	// The check an edit gets, run on the whole text, so that what is saved here
	// is as startable as what is saved there. Text that cannot even be read is
	// a problem to show next to the editor and not an error: it is the mistake
	// this call exists to let somebody correct.
	if problems := textProblems([]byte(text)); len(problems) != 0 {
		return connect.NewResponse(&controlv1.ReplaceConfigTextResponse{
			Hash:           doc.Hash,
			Problems:       problemsToProto(problems),
			PendingRestart: s.pending(doc),
			Version:        doc.Version,
		}), nil
	}

	saved, err := s.save(ctx, doc, text)
	if err != nil {
		return nil, toConnectError(err)
	}

	return connect.NewResponse(&controlv1.ReplaceConfigTextResponse{
		Written:        true,
		Hash:           saved.Hash,
		PendingRestart: s.pending(saved),
		Version:        saved.Version,
	}), nil
}

func (s *ConfigServer) GetInstructionFile(
	ctx context.Context,
	req *connect.Request[controlv1.GetInstructionFileRequest],
) (*connect.Response[controlv1.GetInstructionFileResponse], error) {
	name, err := instructionDocument(req.Msg.GetFile())
	if err != nil {
		return nil, err
	}

	doc, err := s.documents.Document(ctx, name)
	if err != nil {
		return nil, toConnectError(err)
	}

	return connect.NewResponse(&controlv1.GetInstructionFileResponse{
		Text:           doc.Text,
		Hash:           doc.Hash,
		RunningHash:    s.started.of(name),
		PendingRestart: s.pending(doc),
		Version:        doc.Version,
	}), nil
}

func (s *ConfigServer) PutInstructionFile(
	ctx context.Context,
	req *connect.Request[controlv1.PutInstructionFileRequest],
) (*connect.Response[controlv1.PutInstructionFileResponse], error) {
	name, err := instructionDocument(req.Msg.GetFile())
	if err != nil {
		return nil, err
	}

	text := req.Msg.GetText()
	if err := checkDocumentText(text); err != nil {
		return nil, err
	}

	// An edit that started from nothing must still be refused if somebody has
	// since saved something, and the empty hash is how it says it started from
	// nothing; the store compares it the same as any other.
	doc, err := s.documents.Document(ctx, name)
	if err != nil {
		return nil, toConnectError(err)
	}
	if doc.Hash != req.Msg.GetHash() {
		return nil, toConnectError(storage.ErrDocumentChanged)
	}

	saved, err := s.save(ctx, doc, text)
	if err != nil {
		return nil, toConnectError(err)
	}

	return connect.NewResponse(&controlv1.PutInstructionFileResponse{
		Hash:           saved.Hash,
		PendingRestart: s.pending(saved),
		Version:        saved.Version,
	}), nil
}

func (s *ConfigServer) ListDocumentVersions(
	ctx context.Context,
	req *connect.Request[controlv1.ListDocumentVersionsRequest],
) (*connect.Response[controlv1.ListDocumentVersionsResponse], error) {
	name, err := settingsDocument(req.Msg.GetDocument())
	if err != nil {
		return nil, err
	}

	kept, err := s.documents.DocumentVersions(ctx, name)
	if err != nil {
		return nil, toConnectError(err)
	}

	versions := make([]*controlv1.DocumentVersion, len(kept))
	for i, doc := range kept {
		versions[i] = documentVersionToProto(doc)
	}
	return connect.NewResponse(&controlv1.ListDocumentVersionsResponse{Versions: versions}), nil
}

func (s *ConfigServer) GetDocumentVersion(
	ctx context.Context,
	req *connect.Request[controlv1.GetDocumentVersionRequest],
) (*connect.Response[controlv1.GetDocumentVersionResponse], error) {
	name, err := settingsDocument(req.Msg.GetDocument())
	if err != nil {
		return nil, err
	}

	doc, err := s.documents.DocumentVersion(ctx, name, req.Msg.GetVersion())
	if err != nil {
		return nil, toConnectError(err)
	}

	return connect.NewResponse(&controlv1.GetDocumentVersionResponse{
		Info: documentVersionToProto(doc),
		Text: doc.Text,
	}), nil
}

func (s *ConfigServer) GetAgentState(
	_ context.Context,
	_ *connect.Request[controlv1.GetAgentStateRequest],
) (*connect.Response[controlv1.GetAgentStateResponse], error) {
	mode := "running"
	if s.state.Repair {
		mode = "repair"
	}

	problems := make([]*controlv1.ConfigProblem, len(s.state.Problems))
	for i, problem := range s.state.Problems {
		problems[i] = problemToProto(problem.Problem)
		problems[i].Source = problem.Source
	}

	resp := &controlv1.GetAgentStateResponse{Mode: mode, Problems: problems}
	if !s.state.StartedAt.IsZero() {
		resp.StartedAt = timestamppb.New(s.state.StartedAt)
	}
	return connect.NewResponse(resp), nil
}

// ── helpers ────────────────────────────────────────────────────────────

// pending says whether the document as saved is not the one this process
// started from. By hash, so a text that was changed and then changed back is
// the one that is running again and needs nothing.
func (s *ConfigServer) pending(doc storage.Document) bool {
	return doc.Hash != s.started.of(doc.Name)
}

// save makes text the document, unless it already is.
//
// A save that changes nothing is not recorded: a page's Save button is pressed
// without an edit often enough that each press would otherwise push a real
// earlier version out of the twenty that are kept.
func (s *ConfigServer) save(ctx context.Context, current storage.Document, text string) (storage.Document, error) {
	if text == current.Text {
		return current, nil
	}
	return s.documents.PutDocument(ctx, current.Name, text, sourceWeb, current.Hash, time.Now())
}

// checkDocumentText refuses text that no document takes.
func checkDocumentText(text string) error {
	if len(text) > maxDocumentBytes {
		return connect.NewError(connect.CodeInvalidArgument,
			fmt.Errorf("a document is limited to %d bytes", maxDocumentBytes))
	}
	if !utf8.ValidString(text) {
		return connect.NewError(connect.CodeInvalidArgument, errors.New("a document must be UTF-8 text"))
	}
	return nil
}

// textProblems is everything wrong with a whole config text: a key it cannot
// hold, a setting that will not do, or the text not being readable at all.
func textProblems(text []byte) []config.Problem {
	unreadable := func(err error) []config.Problem {
		return []config.Problem{{
			Why: err.Error(),
			Fix: "Correct the text so it reads as TOML settings, or save an earlier version again.",
		}}
	}

	bootstrap, err := config.BootstrapProblems(text)
	if err != nil {
		return unreadable(err)
	}
	problems, err := config.Check(text)
	if err != nil {
		return unreadable(err)
	}
	return append(bootstrap, problems...)
}

// editsOf turns the edits a client sent into edits, and sets aside the ones
// that can never be made.
//
// The second return is those: an edit to a key that says where the settings
// are kept is not an error in the request so much as a problem with the edit,
// and is reported beside any others rather than instead of them.
func (s *ConfigServer) editsOf(given []*controlv1.ConfigEdit) ([]config.Edit, []config.Problem, error) {
	var (
		edits   = make([]config.Edit, 0, len(given))
		refused []config.Problem
	)

	for _, edit := range given {
		if override, held := s.byKey[edit.GetKey()]; held {
			// Not an error in the edit so much as in the offer: the saved text
			// is not what decides this setting, so changing it would do nothing
			// and look as though it had.
			return nil, nil, connect.NewError(connect.CodeInvalidArgument,
				fmt.Errorf("%s is set by %s, so the saved settings cannot change it", override.Key, override.Name))
		}

		if !edit.GetRemove() && edit.GetValue() == nil {
			return nil, nil, connect.NewError(connect.CodeInvalidArgument,
				fmt.Errorf("%s has no value", edit.GetKey()))
		}

		var value any
		if edit.GetValue() != nil {
			value = edit.GetValue().AsInterface()
		}

		made := config.Edit{Key: edit.GetKey(), Value: value, Remove: edit.GetRemove()}
		if problem, bootstrap := config.BootstrapProblem(made); bootstrap {
			refused = append(refused, problem)
			continue
		}
		edits = append(edits, made)
	}
	return edits, refused, nil
}

// instructionDocument is the document an instruction file request names.
func instructionDocument(file controlv1.InstructionFile) (string, error) {
	switch file {
	case controlv1.InstructionFile_INSTRUCTION_FILE_PERSONA:
		return storage.DocumentPersona, nil
	case controlv1.InstructionFile_INSTRUCTION_FILE_AGENTS:
		return storage.DocumentAgents, nil
	}
	return "", connect.NewError(connect.CodeInvalidArgument, errors.New("which one: persona or agents?"))
}

// settingsDocument is the document a history request names.
func settingsDocument(document controlv1.SettingsDocument) (string, error) {
	switch document {
	case controlv1.SettingsDocument_SETTINGS_DOCUMENT_CONFIG:
		return storage.DocumentConfig, nil
	case controlv1.SettingsDocument_SETTINGS_DOCUMENT_AGENTS:
		return storage.DocumentAgents, nil
	case controlv1.SettingsDocument_SETTINGS_DOCUMENT_PERSONA:
		return storage.DocumentPersona, nil
	}
	return "", connect.NewError(connect.CodeInvalidArgument, errors.New("which document: config, agents or persona?"))
}

// documentVersionToProto is a version without its text, which a list of twenty
// would otherwise carry twenty times.
func documentVersionToProto(doc storage.Document) *controlv1.DocumentVersion {
	return &controlv1.DocumentVersion{
		Version: doc.Version,
		Hash:    doc.Hash,
		Source:  doc.Source,
		SavedAt: timestamppb.New(doc.UpdatedAt),
	}
}

func problemToProto(problem config.Problem) *controlv1.ConfigProblem {
	return &controlv1.ConfigProblem{
		Key: problem.Key, Value: problem.Value, Why: problem.Why, Fix: problem.Fix,
	}
}

func problemsToProto(problems []config.Problem) []*controlv1.ConfigProblem {
	out := make([]*controlv1.ConfigProblem, len(problems))
	for i, problem := range problems {
		out[i] = problemToProto(problem)
	}
	return out
}

func diffToProto(patched config.Patched) []*controlv1.ConfigDiffLine {
	var out []*controlv1.ConfigDiffLine
	for _, line := range patched.Diff() {
		out = append(out, &controlv1.ConfigDiffLine{Kind: line.Kind, Text: line.Text})
	}
	return out
}
