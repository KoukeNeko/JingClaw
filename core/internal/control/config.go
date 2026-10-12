package control

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"sync"

	"connectrpc.com/connect"
	"google.golang.org/protobuf/types/known/structpb"

	controlv1 "github.com/KoukeNeko/JingClaw/core/gen/go/jingclaw/control/v1"
	"github.com/KoukeNeko/JingClaw/core/gen/go/jingclaw/control/v1/controlv1connect"
	"github.com/KoukeNeko/JingClaw/core/internal/config"
)

// maxInstructionFile is the most of either instruction file one write takes.
// Both are put in front of the model, so a megabyte is already far past
// anything meant to be read on every turn.
const maxInstructionFile = 1 << 20

// ConfigOverride is a setting the file cannot change, because something wins
// over it.
type ConfigOverride struct {
	Key string
	// Source is "environment" or "flag", and Name the variable or flag as the
	// operator wrote it.
	Source string
	Name   string
}

// ConfigServer lets a person read and change the settings file.
//
// It owns no settings. The file is the source of truth and every call goes
// back to it, so an edit made in an editor between two calls is what the
// second one sees.
type ConfigServer struct {
	path    string
	running map[string]any

	overrides []ConfigOverride
	byKey     map[string]ConfigOverride

	// deployment is the directory the instruction files live in, empty when
	// this daemon was pointed at a settings file outside any deployment.
	deployment string

	// One write at a time. The hash check refuses a stale edit, but between
	// reading the file and replacing it two writers would still race.
	write sync.Mutex
}

var _ controlv1connect.ConfigServiceHandler = (*ConfigServer)(nil)

// NewConfigServer serves the file at path. running is what this process loaded,
// after flags, for showing next to what the file says.
func NewConfigServer(path string, running config.Config, overrides []ConfigOverride, deployment string) *ConfigServer {
	sort.Slice(overrides, func(i, j int) bool { return overrides[i].Key < overrides[j].Key })

	byKey := make(map[string]ConfigOverride, len(overrides))
	for _, override := range overrides {
		byKey[override.Key] = override
	}

	// Absolute from here on. A path given relative to wherever the daemon was
	// started is the wrong file the moment anything changes directory, and it
	// is what the page shows the operator as the file being written.
	if path != "" {
		if absolute, err := filepath.Abs(path); err == nil {
			path = absolute
		}
	}

	return &ConfigServer{
		path:       path,
		running:    config.Flatten(running),
		overrides:  overrides,
		byKey:      byKey,
		deployment: deployment,
	}
}

func (s *ConfigServer) GetConfig(
	_ context.Context,
	_ *connect.Request[controlv1.GetConfigRequest],
) (*connect.Response[controlv1.GetConfigResponse], error) {
	content, err := s.read()
	if err != nil {
		return nil, err
	}

	file, err := config.FileSettings(content)
	if err != nil {
		// A file that does not parse is one the daemon would not have started
		// with, so this is rare, and the operator has to fix it in an editor.
		return nil, connect.NewError(connect.CodeFailedPrecondition, err)
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
		Path:      s.path,
		Hash:      fileHash(content),
		File:      fileStruct,
		Running:   runningStruct,
		Overrides: overrides,
	}), nil
}

func (s *ConfigServer) CheckConfig(
	_ context.Context,
	req *connect.Request[controlv1.CheckConfigRequest],
) (*connect.Response[controlv1.CheckConfigResponse], error) {
	edits, err := s.editsOf(req.Msg.GetEdits())
	if err != nil {
		return nil, err
	}

	content, err := s.read()
	if err != nil {
		return nil, err
	}

	patched, problems, err := config.Preview(content, edits)
	if err != nil {
		return nil, connect.NewError(connect.CodeInvalidArgument, err)
	}

	return connect.NewResponse(&controlv1.CheckConfigResponse{
		Problems: problemsToProto(problems),
		Diff:     diffToProto(patched),
	}), nil
}

func (s *ConfigServer) WriteConfig(
	_ context.Context,
	req *connect.Request[controlv1.WriteConfigRequest],
) (*connect.Response[controlv1.WriteConfigResponse], error) {
	edits, err := s.editsOf(req.Msg.GetEdits())
	if err != nil {
		return nil, err
	}
	if len(edits) == 0 {
		return nil, connect.NewError(connect.CodeInvalidArgument, errors.New("there is nothing to write"))
	}

	s.write.Lock()
	defer s.write.Unlock()

	content, err := s.read()
	if err != nil {
		return nil, err
	}
	// Refused before anything is worked out: an edit that began from another
	// version of the file was made against settings that are no longer there.
	if fileHash(content) != req.Msg.GetHash() {
		return nil, toConnectError(config.ErrChangedUnderneath)
	}

	patched, problems, err := config.Preview(content, edits)
	if err != nil {
		return nil, connect.NewError(connect.CodeInvalidArgument, err)
	}
	if len(problems) != 0 {
		return connect.NewResponse(&controlv1.WriteConfigResponse{
			Hash:     fileHash(content),
			Problems: problemsToProto(problems),
		}), nil
	}

	if err := config.Replace(s.path, patched.Text, req.Msg.GetHash()); err != nil {
		return nil, toConnectError(err)
	}

	changed := make([]string, len(edits))
	for i, edit := range edits {
		changed[i] = edit.Key
	}

	return connect.NewResponse(&controlv1.WriteConfigResponse{
		Written:     true,
		Hash:        fileHash(patched.Text),
		ChangedKeys: changed,
	}), nil
}

func (s *ConfigServer) GetInstructionFile(
	_ context.Context,
	req *connect.Request[controlv1.GetInstructionFileRequest],
) (*connect.Response[controlv1.GetInstructionFileResponse], error) {
	path, err := s.instructionPath(req.Msg.GetFile())
	if err != nil {
		return nil, err
	}

	content, err := os.ReadFile(path)
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return nil, connect.NewError(connect.CodeInternal, err)
	}

	return connect.NewResponse(&controlv1.GetInstructionFileResponse{
		Path: path, Text: string(content), Hash: fileHash(content),
	}), nil
}

func (s *ConfigServer) PutInstructionFile(
	_ context.Context,
	req *connect.Request[controlv1.PutInstructionFileRequest],
) (*connect.Response[controlv1.PutInstructionFileResponse], error) {
	path, err := s.instructionPath(req.Msg.GetFile())
	if err != nil {
		return nil, err
	}

	text := req.Msg.GetText()
	if len(text) > maxInstructionFile {
		return nil, connect.NewError(connect.CodeInvalidArgument,
			fmt.Errorf("the file is limited to %d bytes", maxInstructionFile))
	}

	s.write.Lock()
	defer s.write.Unlock()

	// Checked here as well as in Replace, which only compares when it is given
	// a hash: an edit that started from an empty file must still be refused if
	// somebody has since written one.
	current, err := os.ReadFile(path)
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return nil, connect.NewError(connect.CodeInternal, err)
	}
	if fileHash(current) != req.Msg.GetHash() {
		return nil, toConnectError(config.ErrChangedUnderneath)
	}

	if err := config.Replace(path, []byte(text), req.Msg.GetHash()); err != nil {
		return nil, toConnectError(err)
	}
	return connect.NewResponse(&controlv1.PutInstructionFileResponse{Hash: fileHash([]byte(text))}), nil
}

// ── helpers ────────────────────────────────────────────────────────────

// read is the file as it is now. One that does not exist yet reads as empty,
// which is what a first edit starts from.
func (s *ConfigServer) read() ([]byte, error) {
	if s.path == "" {
		return nil, connect.NewError(connect.CodeFailedPrecondition,
			errors.New("this daemon was not started from a settings file"))
	}

	content, err := os.ReadFile(s.path)
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return nil, connect.NewError(connect.CodeInternal, err)
	}
	return content, nil
}

// fileHash is the hash of a file, and empty for one that is not there, which
// is what the client sends back when it started from nothing.
func fileHash(content []byte) string {
	if len(content) == 0 {
		return ""
	}
	return config.Hash(content)
}

func (s *ConfigServer) editsOf(given []*controlv1.ConfigEdit) ([]config.Edit, error) {
	edits := make([]config.Edit, 0, len(given))
	for _, edit := range given {
		if override, held := s.byKey[edit.GetKey()]; held {
			// Not an error in the edit so much as in the offer: the file is
			// not what decides this setting, so changing it would do nothing
			// and look as though it had.
			return nil, connect.NewError(connect.CodeInvalidArgument,
				fmt.Errorf("%s is set by %s, so the file cannot change it", override.Key, override.Name))
		}

		if !edit.GetRemove() && edit.GetValue() == nil {
			return nil, connect.NewError(connect.CodeInvalidArgument,
				fmt.Errorf("%s has no value", edit.GetKey()))
		}

		var value any
		if edit.GetValue() != nil {
			value = edit.GetValue().AsInterface()
		}
		edits = append(edits, config.Edit{Key: edit.GetKey(), Value: value, Remove: edit.GetRemove()})
	}
	return edits, nil
}

func (s *ConfigServer) instructionPath(file controlv1.InstructionFile) (string, error) {
	if s.deployment == "" {
		return "", connect.NewError(connect.CodeFailedPrecondition,
			errors.New("there is no deployment directory to keep these files in"))
	}

	switch file {
	case controlv1.InstructionFile_INSTRUCTION_FILE_PERSONA:
		return filepath.Join(s.deployment, config.PersonaFile), nil
	case controlv1.InstructionFile_INSTRUCTION_FILE_AGENTS:
		return filepath.Join(s.deployment, config.InstructionsFile), nil
	}
	return "", connect.NewError(connect.CodeInvalidArgument, errors.New("which file?"))
}

func problemsToProto(problems []config.Problem) []*controlv1.ConfigProblem {
	out := make([]*controlv1.ConfigProblem, len(problems))
	for i, problem := range problems {
		out[i] = &controlv1.ConfigProblem{
			Key: problem.Key, Value: problem.Value, Why: problem.Why, Fix: problem.Fix,
		}
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
