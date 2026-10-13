package control

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"log/slog"
	"maps"
	"sync"
	"time"

	"connectrpc.com/connect"
	"google.golang.org/protobuf/types/known/timestamppb"

	controlv1 "github.com/KoukeNeko/JingClaw/core/gen/go/jingclaw/control/v1"
	"github.com/KoukeNeko/JingClaw/core/gen/go/jingclaw/control/v1/controlv1connect"
	"github.com/KoukeNeko/JingClaw/core/internal/config"
	"github.com/KoukeNeko/JingClaw/core/internal/home"
	"github.com/KoukeNeko/JingClaw/core/internal/secret"
	"github.com/KoukeNeko/JingClaw/core/internal/storage"
)

// credentialSlot is what this service knows about a slot beyond the settings
// that name its file: which process reads it, and when the settings make it
// matter.
type credentialSlot struct {
	slot controlv1.CredentialSlot

	// id is the config.CredentialFile it is the credential of.
	id      string
	process controlv1.CredentialProcess
	need    func(config.Config) controlv1.CredentialNeed
}

// needWhen is need when the settings select what reads the credential, and
// nothing reads it otherwise.
func needWhen(selected bool, need controlv1.CredentialNeed) controlv1.CredentialNeed {
	if selected {
		return need
	}
	return controlv1.CredentialNeed_CREDENTIAL_NEED_UNUSED
}

// credentialSlots are the places a credential is read from, in the order they
// are listed.
//
// Each need is what the code that reads the credential does with it: the
// hosted providers will not start without theirs, while a local model server or
// an embedding endpoint usually has none and uses one when it is there. The
// search key is asked for when the search backend is brave and web reading is
// on, which is when the daemon looks for it. The platform a gateway serves is
// gateway.platform, which a gateway's own --platform can override.
var credentialSlots = []credentialSlot{
	{
		slot: controlv1.CredentialSlot_CREDENTIAL_SLOT_PROVIDER_GEMINI, id: "provider.gemini",
		process: controlv1.CredentialProcess_CREDENTIAL_PROCESS_DAEMON,
		need: func(c config.Config) controlv1.CredentialNeed {
			return needWhen(c.Provider.Backend == "gemini", controlv1.CredentialNeed_CREDENTIAL_NEED_REQUIRED)
		},
	},
	{
		slot: controlv1.CredentialSlot_CREDENTIAL_SLOT_PROVIDER_ANTHROPIC, id: "provider.anthropic",
		process: controlv1.CredentialProcess_CREDENTIAL_PROCESS_DAEMON,
		need: func(c config.Config) controlv1.CredentialNeed {
			return needWhen(c.Provider.Backend == "anthropic", controlv1.CredentialNeed_CREDENTIAL_NEED_REQUIRED)
		},
	},
	{
		slot: controlv1.CredentialSlot_CREDENTIAL_SLOT_PROVIDER_OLLAMA, id: "provider.ollama",
		process: controlv1.CredentialProcess_CREDENTIAL_PROCESS_DAEMON,
		need: func(c config.Config) controlv1.CredentialNeed {
			return needWhen(c.Provider.Backend == "ollama", controlv1.CredentialNeed_CREDENTIAL_NEED_OPTIONAL)
		},
	},
	{
		slot: controlv1.CredentialSlot_CREDENTIAL_SLOT_PROVIDER_OPENAI_COMPAT, id: "provider.openai_compat",
		process: controlv1.CredentialProcess_CREDENTIAL_PROCESS_DAEMON,
		need: func(c config.Config) controlv1.CredentialNeed {
			return needWhen(c.Provider.Backend == "openai_compat", controlv1.CredentialNeed_CREDENTIAL_NEED_OPTIONAL)
		},
	},
	{
		slot: controlv1.CredentialSlot_CREDENTIAL_SLOT_MEMORY_EMBEDDING, id: "memory.embedding",
		process: controlv1.CredentialProcess_CREDENTIAL_PROCESS_DAEMON,
		need: func(c config.Config) controlv1.CredentialNeed {
			return needWhen(c.Memory.Enabled && c.Memory.Embedding.Model != "",
				controlv1.CredentialNeed_CREDENTIAL_NEED_OPTIONAL)
		},
	},
	{
		slot: controlv1.CredentialSlot_CREDENTIAL_SLOT_WEB_SEARCH, id: "web.search",
		process: controlv1.CredentialProcess_CREDENTIAL_PROCESS_DAEMON,
		need: func(c config.Config) controlv1.CredentialNeed {
			return needWhen(c.Web.Enabled && c.Web.Search.Backend == "brave",
				controlv1.CredentialNeed_CREDENTIAL_NEED_REQUIRED)
		},
	},
	{
		slot: controlv1.CredentialSlot_CREDENTIAL_SLOT_GATEWAY_DISCORD, id: "gateway.discord",
		process: controlv1.CredentialProcess_CREDENTIAL_PROCESS_GATEWAY,
		need: func(c config.Config) controlv1.CredentialNeed {
			return needWhen(c.Gateway.Platform == config.PlatformDiscord, controlv1.CredentialNeed_CREDENTIAL_NEED_REQUIRED)
		},
	},
	{
		slot: controlv1.CredentialSlot_CREDENTIAL_SLOT_GATEWAY_TELEGRAM, id: "gateway.telegram",
		process: controlv1.CredentialProcess_CREDENTIAL_PROCESS_GATEWAY,
		need: func(c config.Config) controlv1.CredentialNeed {
			return needWhen(c.Gateway.Platform == config.PlatformTelegram, controlv1.CredentialNeed_CREDENTIAL_NEED_REQUIRED)
		},
	},
}

// CredentialServer lets a person put a credential where the agent will find it,
// and never read one back.
//
// The caller names a slot. Which file that is comes from the saved config, read
// afresh on every call, and has to pass the check the config itself is held to,
// and one more: no two slots may be given the same file. So no call here reaches
// a file the settings could not have named.
//
// Nothing a reply carries comes out of a file but its modification time: the
// replies have no field to hold a value, a size, a hash or a mask, and the
// service does not log one.
type CredentialServer struct {
	documents storage.DocumentStore
	logger    *slog.Logger

	// candidate is how the saved text becomes the settings a start would use. The
	// daemon's own, so that what it was given on its command line counts here as
	// it does there: a slot is needed because of the backend that will run, not
	// the one the text names.
	candidate func(text []byte) (config.Config, error)

	// begun is when this process started. A file modified after it is one the
	// process has not read.
	begun time.Time

	// mu is held around anything that changes a file, so that two saves cannot
	// interleave their checks and their writes, and around changed.
	mu sync.Mutex

	// changed are the slots this process has changed the file of since it began,
	// which a modification time cannot say for a delete or a chmod.
	changed map[controlv1.CredentialSlot]bool
}

var _ controlv1connect.CredentialServiceHandler = (*CredentialServer)(nil)

// NewCredentialServer serves the credentials of the deployment whose settings
// are in documents. candidate turns the saved text into the settings a start
// would use, and begun is when this process started.
func NewCredentialServer(
	documents storage.DocumentStore,
	candidate func(text []byte) (config.Config, error),
	begun time.Time,
	logger *slog.Logger,
) *CredentialServer {
	return &CredentialServer{
		documents: documents,
		candidate: candidate,
		logger:    logger,
		begun:     begun,
		changed:   make(map[controlv1.CredentialSlot]bool),
	}
}

func (s *CredentialServer) ListCredentials(
	ctx context.Context,
	_ *connect.Request[controlv1.ListCredentialsRequest],
) (*connect.Response[controlv1.ListCredentialsResponse], error) {
	cfg, err := s.saved(ctx)
	if err != nil {
		return nil, err
	}

	s.mu.Lock()
	changed := maps.Clone(s.changed)
	s.mu.Unlock()

	listed := make([]*controlv1.Credential, len(credentialSlots))
	for i, slot := range credentialSlots {
		listed[i] = s.describe(cfg, slot, changed[slot.slot])
	}
	return connect.NewResponse(&controlv1.ListCredentialsResponse{Credentials: listed}), nil
}

func (s *CredentialServer) PutCredential(
	ctx context.Context,
	req *connect.Request[controlv1.PutCredentialRequest],
) (*connect.Response[controlv1.PutCredentialResponse], error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	at, err := s.target(ctx, req.Msg.GetSlot())
	if err != nil {
		return nil, err
	}

	var expect time.Time
	if given := req.Msg.GetExpectModifiedAt(); given != nil {
		expect = given.AsTime()
	}
	write := func() error { return at.dir.Write(at.file.Name, req.Msg.GetValue(), expect) }
	if req.Msg.GetExpectAbsent() {
		write = func() error { return at.dir.WriteNew(at.file.Name, req.Msg.GetValue()) }
	}
	if err := write(); err != nil {
		return nil, credentialError(err, at.file.Name)
	}

	s.changed[at.slot.slot] = true
	s.logger.Info("credential saved", "slot", at.slot.id)

	return connect.NewResponse(&controlv1.PutCredentialResponse{
		Credential: s.describe(at.cfg, at.slot, true),
	}), nil
}

func (s *CredentialServer) DeleteCredential(
	ctx context.Context,
	req *connect.Request[controlv1.DeleteCredentialRequest],
) (*connect.Response[controlv1.DeleteCredentialResponse], error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	at, err := s.target(ctx, req.Msg.GetSlot())
	if err != nil {
		return nil, err
	}

	removed, err := at.dir.Remove(at.file.Name)
	if err != nil {
		return nil, credentialError(err, at.file.Name)
	}
	if removed {
		s.changed[at.slot.slot] = true
		s.logger.Info("credential deleted", "slot", at.slot.id)
	}

	return connect.NewResponse(&controlv1.DeleteCredentialResponse{
		Credential: s.describe(at.cfg, at.slot, s.changed[at.slot.slot]),
	}), nil
}

func (s *CredentialServer) RestrictCredentialFile(
	ctx context.Context,
	req *connect.Request[controlv1.RestrictCredentialFileRequest],
) (*connect.Response[controlv1.RestrictCredentialFileResponse], error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	at, err := s.target(ctx, req.Msg.GetSlot())
	if err != nil {
		return nil, err
	}

	restricted, err := at.dir.Restrict(at.file.Name)
	if err != nil {
		return nil, credentialError(err, at.file.Name)
	}
	if restricted {
		s.changed[at.slot.slot] = true
		s.logger.Info("credential file restricted", "slot", at.slot.id)
	}

	return connect.NewResponse(&controlv1.RestrictCredentialFileResponse{
		Credential: s.describe(at.cfg, at.slot, s.changed[at.slot.slot]),
	}), nil
}

// ── helpers ────────────────────────────────────────────────────────────

// saved is the settings the next start will use: the saved config with the
// defaults under it, and the environment and the flags over it.
//
// The saved text and not what is running, because a credential is written for
// the next start, and a name the running process was given is not the name the
// next one will look for if the settings were changed since.
func (s *CredentialServer) saved(ctx context.Context) (config.Config, error) {
	doc, err := s.documents.Document(ctx, storage.DocumentConfig)
	if err != nil {
		return config.Config{}, toConnectError(err)
	}

	cfg, err := s.candidate([]byte(doc.Text))
	if err != nil {
		return config.Config{}, connect.NewError(connect.CodeFailedPrecondition, fmt.Errorf(
			"the saved settings cannot be read, so which file each credential is kept in is not known; "+
				"correct the settings first: %w", err))
	}
	return cfg, nil
}

// credentialTarget is a slot the settings say how to reach: the file it is kept
// in, and the directory that file is in.
type credentialTarget struct {
	cfg  config.Config
	slot credentialSlot
	file config.CredentialFile
	dir  secret.Directory
}

// target settles which file a call is about, or why there is none.
//
// Every refusal here is a failed precondition: the request is well formed, and
// the settings or the machine are not in a state to take it.
func (s *CredentialServer) target(ctx context.Context, which controlv1.CredentialSlot) (credentialTarget, error) {
	slot, ok := slotOf(which)
	if !ok {
		return credentialTarget{}, connect.NewError(connect.CodeInvalidArgument, errors.New("which credential?"))
	}

	cfg, err := s.saved(ctx)
	if err != nil {
		return credentialTarget{}, err
	}
	file := fileOf(cfg, slot)

	if file.Name == "" {
		return credentialTarget{}, connect.NewError(connect.CodeFailedPrecondition, fmt.Errorf(
			"%s is empty, so this credential has no file; set it first, for example to %q",
			file.Key, file.Suggest))
	}
	if problem, found := problemOf(cfg, file); found {
		return credentialTarget{}, connect.NewError(connect.CodeFailedPrecondition, fmt.Errorf(
			"%s %s. %s", problem.Key, problem.Why, problem.Fix))
	}

	root, found := home.Resolve()
	if !found {
		return credentialTarget{}, connect.NewError(connect.CodeFailedPrecondition, fmt.Errorf(
			"%s is %s, so there is no deployment directory to keep a credential file in", home.EnvVar, home.None))
	}
	dir, err := secret.OpenDirectory(root.Root)
	if errors.Is(err, fs.ErrNotExist) {
		return credentialTarget{}, connect.NewError(connect.CodeFailedPrecondition, fmt.Errorf(
			"the deployment directory %s does not exist", root.Root))
	}
	if err != nil {
		return credentialTarget{}, connect.NewError(connect.CodeFailedPrecondition, err)
	}

	return credentialTarget{cfg: cfg, slot: slot, file: file, dir: dir}, nil
}

// slotOf is the slot a request names.
func slotOf(which controlv1.CredentialSlot) (credentialSlot, bool) {
	for _, slot := range credentialSlots {
		if slot.slot == which {
			return slot, true
		}
	}
	return credentialSlot{}, false
}

// fileOf is the settings that name the file of a slot.
func fileOf(cfg config.Config, slot credentialSlot) config.CredentialFile {
	for _, file := range cfg.CredentialFiles() {
		if file.Slot == slot.id {
			return file
		}
	}
	// credentialSlots and CredentialFiles are the same eight; a test holds them
	// to it, so reaching here is a build that was changed in one place.
	panic("control: no credential file settings for " + slot.id)
}

// problemOf is what is wrong with the name a slot's setting holds: what the
// check at start says of it, and then that another slot has it too, which the
// check at start lets be.
func problemOf(cfg config.Config, file config.CredentialFile) (config.Problem, bool) {
	for _, problems := range [][]config.Problem{cfg.CredentialFileProblems(), cfg.SharedCredentialFileProblems()} {
		for _, problem := range problems {
			if problem.Key == file.Key {
				return problem, true
			}
		}
	}
	return config.Problem{}, false
}

// describe is a slot as the next start would find it.
//
// changed is whether this process has changed the file since it began.
func (s *CredentialServer) describe(cfg config.Config, slot credentialSlot, changed bool) *controlv1.Credential {
	file := fileOf(cfg, slot)

	// Only what can be the name of a variable. The saved text is not checked
	// before it is listed, so that a daemon with no agent can say what is wrong
	// with it, and what a setting that lists names holds may be a key somebody
	// pasted there. It is neither listed nor looked up.
	var env []string
	for _, name := range file.Env {
		if config.IsEnvName(name) {
			env = append(env, name)
		}
	}

	out := &controlv1.Credential{
		Slot:        slot.slot,
		Process:     slot.process,
		Need:        slot.need(cfg),
		FileSetting: file.Key,
		EnvSetting:  file.EnvKey,
		EnvNames:    env,
	}

	if problem, found := problemOf(cfg, file); found {
		// Not named back: the setting is what to correct, and a name that is not
		// allowed may have been a key pasted in the wrong place.
		out.FileState = controlv1.CredentialFileState_CREDENTIAL_FILE_STATE_INVALID_NAME
		out.FileDetail = problem.Why + ". " + problem.Fix
		out.Source = controlv1.CredentialSource_CREDENTIAL_SOURCE_BLOCKED
		return out
	}

	var files []string
	if file.Name == "" {
		out.FileState = controlv1.CredentialFileState_CREDENTIAL_FILE_STATE_NO_NAME
	} else {
		out.FileName = file.Name
		if root, found := home.Resolve(); found {
			files = []string{root.SecretFile(file.Name)}
		}
	}

	decided := secret.Inspect(secret.LoadOptions{EnvVars: env, Files: files}, s.begun)
	itself := decided
	if decided.Source == secret.SourceEnvironment && len(files) > 0 {
		// The environment decided first and never looked at the file, which is
		// still there to be restricted or replaced.
		itself = secret.Inspect(secret.LoadOptions{Files: files}, s.begun)
	}

	switch decided.Source {
	case secret.SourceEnvironment:
		out.Source = controlv1.CredentialSource_CREDENTIAL_SOURCE_ENVIRONMENT
		out.EnvInUse = decided.EnvVar
	case secret.SourceFile:
		out.Source = controlv1.CredentialSource_CREDENTIAL_SOURCE_FILE
	case secret.SourceBlocked:
		out.Source = controlv1.CredentialSource_CREDENTIAL_SOURCE_BLOCKED
	default:
		out.Source = controlv1.CredentialSource_CREDENTIAL_SOURCE_NONE
	}

	if file.Name != "" {
		out.FileState = fileStateOf(itself.File)
		out.FileDetail = itself.Detail
		if !itself.ModifiedAt.IsZero() {
			out.ModifiedAt = timestamppb.New(itself.ModifiedAt)
		}
	}

	out.RestartNeeded = out.Need != controlv1.CredentialNeed_CREDENTIAL_NEED_UNUSED &&
		decided.Source != secret.SourceEnvironment && (changed || itself.RestartNeeded)
	return out
}

func fileStateOf(state secret.FileState) controlv1.CredentialFileState {
	switch state {
	case secret.FileEmpty:
		return controlv1.CredentialFileState_CREDENTIAL_FILE_STATE_EMPTY
	case secret.FilePresent:
		return controlv1.CredentialFileState_CREDENTIAL_FILE_STATE_PRESENT
	case secret.FileWrongMode:
		return controlv1.CredentialFileState_CREDENTIAL_FILE_STATE_WRONG_MODE
	case secret.FileNotRegular:
		return controlv1.CredentialFileState_CREDENTIAL_FILE_STATE_NOT_REGULAR
	case secret.FileUnreadable:
		return controlv1.CredentialFileState_CREDENTIAL_FILE_STATE_UNREADABLE
	}
	return controlv1.CredentialFileState_CREDENTIAL_FILE_STATE_ABSENT
}

// credentialError is what a failed call is answered with. name is the file's
// name from the settings, which is safe to say; nothing here repeats a value.
func credentialError(err error, name string) error {
	switch {
	case errors.Is(err, secret.ErrInvalidValue):
		return connect.NewError(connect.CodeInvalidArgument, err)
	case errors.Is(err, secret.ErrNotCredentialFile):
		return connect.NewError(connect.CodeFailedPrecondition, fmt.Errorf("%s %w", name, err))
	case errors.Is(err, secret.ErrChanged):
		return connect.NewError(connect.CodeFailedPrecondition, fmt.Errorf(
			"%s was modified, or removed, after it was last listed; list the credentials again before saving", name))
	case errors.Is(err, secret.ErrNoFile):
		return connect.NewError(connect.CodeFailedPrecondition, fmt.Errorf("%s does not exist, so there is nothing to restrict", name))
	}
	return connect.NewError(connect.CodeInternal, err)
}
