package control

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"maps"
	"os"
	"path/filepath"
	"time"
	"unicode/utf8"

	"connectrpc.com/connect"
	"google.golang.org/protobuf/types/known/timestamppb"

	controlv1 "github.com/KoukeNeko/JingClaw/core/gen/go/jingclaw/control/v1"
	"github.com/KoukeNeko/JingClaw/core/gen/go/jingclaw/control/v1/controlv1connect"
	"github.com/KoukeNeko/JingClaw/core/internal/home"
	"github.com/KoukeNeko/JingClaw/core/internal/skill"
)

// skillTextLimit is the most of one file a reply carries. A file in a skill may
// be bigger than a reply should be (skill.MaxFileBytes), so what is shown of
// one is bounded here and the reply says when that cut something off. SKILL.md
// is the exception: it is returned whole, and a skill whose SKILL.md is bigger
// than this cannot be staged.
const skillTextLimit = skill.MaxSkillMDBytes

// OfferedSkills is what the catalogue in the prompt said when the daemon
// started: each skill's name, and the description it was offered under.
//
// The prompt is built once. A skill installed, removed or re-described after
// that is on disk and not in what the model was told, and comparing this with
// the disk is how the service says so.
type OfferedSkills map[string]string

// OfferedBy is what the catalogue built from these skills tells the model.
func OfferedBy(skills []skill.Skill) OfferedSkills {
	offered := make(OfferedSkills, len(skills))
	for _, one := range skills {
		offered[one.Name] = one.Description
	}
	return offered
}

// SkillServer lets a person bring skills into the deployment and take them out.
//
// The skills directory is the deployment's, resolved on every call. Everything
// that changes it goes through skill.Installer, which holds the lock the agent's
// own tools and the rest of this process take; a skill installed from here,
// by the agent, and from the command line ends up recorded the same way.
type SkillServer struct {
	offered OfferedSkills
	logger  *slog.Logger
}

var _ controlv1connect.SkillServiceHandler = (*SkillServer)(nil)

// NewSkillServer serves the skills of the deployment. offered is what the
// running agent was told about them.
func NewSkillServer(offered OfferedSkills, logger *slog.Logger) *SkillServer {
	return &SkillServer{offered: offered, logger: logger}
}

func (s *SkillServer) ListSkills(
	_ context.Context,
	_ *connect.Request[controlv1.ListSkillsRequest],
) (*connect.Response[controlv1.ListSkillsResponse], error) {
	root, err := skillsDirectory()
	if err != nil {
		return nil, err
	}

	installed, rejected, err := skill.Installed(root)
	if err != nil {
		return nil, toConnectError(err)
	}
	lock, err := skill.ReadLock(root)
	if err != nil {
		return nil, unreadableRecord(err)
	}
	waiting, err := skill.StagedSkills(root)
	if err != nil {
		return nil, toConnectError(err)
	}
	drifts := skill.Drifts(lock, installed)

	reply := &controlv1.ListSkillsResponse{RestartRequired: s.restartRequired(installed)}
	for _, one := range installed {
		entry, recorded := lock.Entry(one.Name)
		if !recorded {
			reply.Installed = append(reply.Installed, s.describe(one, nil, skill.DriftNone))
			continue
		}
		reply.Installed = append(reply.Installed, s.describe(one, &entry, drifts[one.Name]))
	}
	for _, one := range rejected {
		reply.Rejected = append(reply.Rejected, &controlv1.RejectedSkill{Directory: one.Name, Reason: one.Reason})
	}
	for _, recorded := range lock.Skills {
		if drifts[recorded.Name] == skill.DriftMissing {
			reply.Missing = append(reply.Missing, &controlv1.MissingSkill{
				Name:        recorded.Name,
				Source:      sourceOf(recorded.From),
				InstalledAt: timeOf(recorded.InstalledAt),
			})
		}
	}
	for _, one := range waiting {
		reply.Staged = append(reply.Staged, stagedOf(root, one))
	}
	return connect.NewResponse(reply), nil
}

func (s *SkillServer) StageSkill(
	ctx context.Context,
	req *connect.Request[controlv1.StageSkillRequest],
) (*connect.Response[controlv1.StageSkillResponse], error) {
	source, err := skill.ParseSource(req.Msg.GetSource())
	if err != nil {
		// Says what is wrong with the text without repeating a credential in it.
		return nil, connect.NewError(connect.CodeInvalidArgument, err)
	}

	root, err := skillsDirectory()
	if err != nil {
		return nil, err
	}

	staged, err := (&skill.Installer{Root: root, Now: time.Now}).Stage(ctx, source)
	if err != nil {
		return nil, toConnectError(err)
	}
	s.logger.Info("skill staged", "skill", staged.Name, "source", staged.Source.String(),
		"tree_digest", staged.TreeDigest)

	return connect.NewResponse(&controlv1.StageSkillResponse{Staged: stagedOf(root, staged)}), nil
}

func (s *SkillServer) GetStagedSkill(
	_ context.Context,
	req *connect.Request[controlv1.GetStagedSkillRequest],
) (*connect.Response[controlv1.GetStagedSkillResponse], error) {
	root, err := skillsDirectory()
	if err != nil {
		return nil, err
	}

	// One look at it, with nothing able to stage the name again between the
	// parts: the listing, the digest and the instructions below are all of one
	// skill. A staged tree with a link in it has no listing and no digest, and
	// is not something that can be activated: the reply is that, and not a half
	// of one.
	review, err := skill.ReviewStaged(root, req.Msg.GetName())
	if err != nil {
		return nil, toConnectError(err)
	}
	staged, files, raw := review.Staged, review.Files, review.SkillMD

	reply := &controlv1.GetStagedSkillResponse{
		Staged:       stagedOf(root, staged),
		SkillMd:      raw,
		SkillMdLines: countLines(raw),
		TreeMatches:  review.TreeMatches,
	}
	for _, file := range files {
		reply.Files = append(reply.Files, &controlv1.StagedSkillFile{
			Path: file.Path, Size: file.Size, Sha256: file.SHA256, IsText: file.Text,
		})
	}

	if asked := req.Msg.GetFilePath(); asked != "" {
		chosen := -1
		for index, file := range files {
			if file.Path == asked {
				chosen = index
			}
		}
		switch {
		case chosen < 0:
			// Only a path the listing has. What is read is never built from the
			// request, so it cannot name anything outside the skill.
			return nil, connect.NewError(connect.CodeInvalidArgument,
				fmt.Errorf("%q is not a file in this skill", asked))
		case !files[chosen].Text:
			return nil, connect.NewError(connect.CodeInvalidArgument,
				fmt.Errorf("%q is not text, so there is nothing to show of it", asked))
		}

		text, truncated, sum, err := readShown(filepath.Join(staged.Dir, filepath.FromSlash(asked)))
		if err != nil {
			return nil, toConnectError(err)
		}
		if sum != files[chosen].SHA256 {
			// Read after the listing was made, outside what holds the staged skill
			// still, so a replacement can have landed in between: what would be
			// shown is not what the listing says it is.
			return nil, connect.NewError(connect.CodeFailedPrecondition, fmt.Errorf(
				"%q changed while it was being read; read it again", asked))
		}
		if truncated {
			// A protobuf string is UTF-8, and a cut can fall inside a character.
			for !utf8.Valid(text) {
				text = text[:len(text)-1]
			}
		}
		reply.FilePath, reply.FileText, reply.FileTruncated = asked, string(text), truncated
	}

	reply.InstalledSkillMd, reply.InstalledSkillMdTruncated = installedSkillMD(root, staged.Name)
	return connect.NewResponse(reply), nil
}

func (s *SkillServer) ActivateSkill(
	_ context.Context,
	req *connect.Request[controlv1.ActivateSkillRequest],
) (*connect.Response[controlv1.ActivateSkillResponse], error) {
	if req.Msg.GetTreeDigest() == "" {
		return nil, connect.NewError(connect.CodeInvalidArgument, errors.New(
			"tree_digest is required: it is the digest of the staged skill the person deciding was shown"))
	}

	root, err := skillsDirectory()
	if err != nil {
		return nil, err
	}

	locked, err := (&skill.Installer{Root: root, Now: time.Now}).
		ActivateExpecting(req.Msg.GetName(), req.Msg.GetTreeDigest(), req.Msg.GetReplace())
	if err != nil {
		return nil, toConnectError(err)
	}
	s.logger.Info("skill activated", "skill", locked.Name, "source", locked.From.String(),
		"tree_digest", locked.TreeDigest, "replace", req.Msg.GetReplace())

	// Read back, so the reply is the skill as it is. The activation is done
	// whether or not this works, and is not reported as failed if it does not.
	dir := filepath.Join(root, locked.Name)
	one, err := skill.Read(dir)
	if err != nil {
		one = skill.Skill{Name: locked.Name, Version: locked.Version, Digest: locked.Digest, Dir: dir}
	}
	return connect.NewResponse(&controlv1.ActivateSkillResponse{
		Installed:       s.describe(one, &locked, skill.DriftNone),
		RestartRequired: s.restartRequiredOn(root),
	}), nil
}

func (s *SkillServer) DiscardStagedSkill(
	_ context.Context,
	req *connect.Request[controlv1.DiscardStagedSkillRequest],
) (*connect.Response[controlv1.DiscardStagedSkillResponse], error) {
	root, err := skillsDirectory()
	if err != nil {
		return nil, err
	}

	if err := (&skill.Installer{Root: root, Now: time.Now}).Discard(req.Msg.GetName()); err != nil {
		return nil, toConnectError(err)
	}
	s.logger.Info("staged skill discarded", "skill", req.Msg.GetName())

	return connect.NewResponse(&controlv1.DiscardStagedSkillResponse{}), nil
}

func (s *SkillServer) RemoveSkill(
	_ context.Context,
	req *connect.Request[controlv1.RemoveSkillRequest],
) (*connect.Response[controlv1.RemoveSkillResponse], error) {
	root, err := skillsDirectory()
	if err != nil {
		return nil, err
	}

	if err := (&skill.Installer{Root: root, Now: time.Now}).Remove(req.Msg.GetName()); err != nil {
		return nil, toConnectError(err)
	}
	s.logger.Info("skill removed", "skill", req.Msg.GetName())

	return connect.NewResponse(&controlv1.RemoveSkillResponse{
		RestartRequired: s.restartRequiredOn(root),
	}), nil
}

// ── helpers ────────────────────────────────────────────────────────────

// skillsDirectory is where this deployment keeps its skills. It need not exist.
func skillsDirectory() (string, error) {
	dir, found := home.Resolve()
	if !found {
		return "", connect.NewError(connect.CodeFailedPrecondition, fmt.Errorf(
			"%s is %s, so there is no deployment directory to keep skills in", home.EnvVar, home.None))
	}
	return dir.Skills(), nil
}

// unreadableRecord is installed.json that cannot be read. Said, and not worked
// around: without it every skill would be called placed by hand.
func unreadableRecord(err error) error {
	return connect.NewError(connect.CodeFailedPrecondition, fmt.Errorf(
		"the record of what was installed cannot be read, so where each skill came from is not known; "+
			"correct or remove it first: %w", err))
}

// restartRequired is whether the catalogue a start now would build is not the
// one the running agent was told.
func (s *SkillServer) restartRequired(installed []skill.Skill) bool {
	return !maps.Equal(s.offered, OfferedBy(installed))
}

// restartRequiredOn is restartRequired for the skills on disk now. When they
// cannot be read it cannot tell, and says yes: a page that is told to restart
// when it need not has lost a moment, and one that is told it need not when it
// must has an agent that does not do what the page says.
func (s *SkillServer) restartRequiredOn(root string) bool {
	installed, _, err := skill.Installed(root)
	return err != nil || s.restartRequired(installed)
}

// describe is an installed skill. recorded is its entry in the record, nil for
// one nobody installed.
func (s *SkillServer) describe(one skill.Skill, recorded *skill.Locked, drift skill.Drift) *controlv1.InstalledSkill {
	out := &controlv1.InstalledSkill{
		Name:        one.Name,
		Directory:   filepath.Base(one.Dir),
		Description: one.Description,
		Version:     one.Version,
		Digest:      one.Digest,
		Drift:       driftOf(drift),
	}
	if offered, told := s.offered[one.Name]; told && offered == one.Description {
		out.InCatalogue = true
	}
	// Empty for a directory with a link in it, which has no digest.
	if tree, _, err := skill.TreeDigest(one.Dir); err == nil {
		out.TreeDigest = tree
	}

	if recorded == nil {
		out.PlacedByHand = true
		return out
	}
	out.Source = sourceOf(recorded.From)
	out.InstalledAt = timeOf(recorded.InstalledAt)
	out.RecordedDigest = recorded.Digest
	out.RecordedTreeDigest = recorded.TreeDigest
	return out
}

func driftOf(drift skill.Drift) controlv1.SkillDrift {
	switch drift {
	case skill.DriftEdited:
		return controlv1.SkillDrift_SKILL_DRIFT_EDITED
	case skill.DriftTreeEdited:
		return controlv1.SkillDrift_SKILL_DRIFT_TREE_EDITED
	}
	return controlv1.SkillDrift_SKILL_DRIFT_NONE
}

// sourceOf is a source as a client may be told it. The address is redacted
// because the record may be older than the refusal of addresses with a
// credential in them.
func sourceOf(source skill.Source) *controlv1.SkillSource {
	return &controlv1.SkillSource{
		Repository: skill.Redact(source.Repository),
		Commit:     source.Commit,
		Path:       source.Path,
	}
}

func timeOf(at time.Time) *timestamppb.Timestamp {
	if at.IsZero() {
		return nil
	}
	return timestamppb.New(at)
}

// stagedOf is a staged skill, and whether activating it would replace something.
func stagedOf(root string, staged skill.Staged) *controlv1.StagedSkill {
	_, err := os.Lstat(filepath.Join(root, staged.Name))

	return &controlv1.StagedSkill{
		Name:        staged.Name,
		Description: staged.Description,
		Version:     staged.Version,
		Source:      sourceOf(staged.Source),
		Digest:      staged.Digest,
		TreeDigest:  staged.TreeDigest,
		SizeBytes:   staged.Size,
		Replaces:    err == nil,
	}
}

// installedSkillMD is SKILL.md of what is in the skills directory under a name,
// if there is a directory there with a plain file of that name in it. Looked at
// without following a link, because a link at either place would have this read
// a file from anywhere.
func installedSkillMD(root, name string) ([]byte, bool) {
	dir := filepath.Join(root, name)
	if info, err := os.Lstat(dir); err != nil || !info.IsDir() {
		return nil, false
	}
	file := filepath.Join(dir, skill.FileName)
	if info, err := os.Lstat(file); err != nil || !info.Mode().IsRegular() {
		return nil, false
	}

	raw, truncated, err := readCapped(file)
	if err != nil {
		return nil, false
	}
	return raw, truncated
}

// readCapped reads a file up to skillTextLimit bytes, and says whether there
// was more.
func readCapped(path string) ([]byte, bool, error) {
	file, err := os.Open(path)
	if err != nil {
		return nil, false, err
	}
	defer file.Close()

	raw, err := io.ReadAll(io.LimitReader(file, skillTextLimit+1))
	if err != nil {
		return nil, false, err
	}
	if len(raw) > skillTextLimit {
		return raw[:skillTextLimit], true, nil
	}
	return raw, false, nil
}

// readShown reads a file for the reply: the first skillTextLimit bytes of it, and
// the hash of all of it, so that what is shown can be held to what was listed.
func readShown(path string) (head []byte, truncated bool, sum string, err error) {
	file, err := os.Open(path)
	if err != nil {
		return nil, false, "", err
	}
	defer file.Close()

	hash := sha256.New()
	head, err = io.ReadAll(io.TeeReader(io.LimitReader(file, skillTextLimit+1), hash))
	if err != nil {
		return nil, false, "", err
	}
	if truncated = len(head) > skillTextLimit; truncated {
		head = head[:skillTextLimit]
	}
	// The rest, if there is any. A file in a skill is no bigger than
	// skill.MaxFileBytes, so this is bounded.
	if _, err := io.Copy(hash, file); err != nil {
		return nil, false, "", err
	}
	return head, truncated, hex.EncodeToString(hash.Sum(nil)), nil
}

// countLines is how many lines a file has: a last line with no line break after
// it counts.
func countLines(raw []byte) int32 {
	lines := bytes.Count(raw, []byte{'\n'})
	if len(raw) > 0 && raw[len(raw)-1] != '\n' {
		lines++
	}
	return int32(lines)
}
