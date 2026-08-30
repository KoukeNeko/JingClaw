// Package skill reads the instruction packs an operator has installed.
//
// A skill is how to use something the agent already has — a note saying that
// this repository deploys with a particular command, or that a CLI on this
// machine takes these arguments. It is instructions and nothing else.
//
// What it is not is a way to get anything. A skill cannot register a tool,
// raise a permission, or skip an approval, and the one rule everything here
// exists to keep is that it never can:
//
//	A skill can make the model want to do something.
//	It can never make the runtime allow it.
//
// So a skill naming a tool that does not exist is a note with a mistake in
// it; the model may try, and the registry refuses. A skill saying to run
// something without asking is a note that is wrong about this program; the
// call goes to whoever decides, exactly as it would have.
package skill

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"gopkg.in/yaml.v3"
)

// FileName is the one file a skill must have.
const FileName = "SKILL.md"

// Skill is one installed pack, as it is on disk.
type Skill struct {
	// Name is the directory's name, not the frontmatter's.
	//
	// Where a skill lives is a fact; what it says about itself is a claim.
	// Two skills whose files both said "deploy" would be one skill with a
	// name nobody could use to pick between them.
	Name string

	// Description is what the model sees in the catalogue, and the only part
	// of a skill in front of it before the skill is asked for.
	Description string

	// Version is for a person reading a listing. It is not the identity: a
	// file edited without touching this line is a different skill wearing the
	// same number, which is what Digest is for.
	Version string

	// Body is everything after the frontmatter — the instructions themselves.
	Body string

	// Digest is what was actually read, so a run can be asked afterwards
	// which instructions it followed rather than which version claimed to be
	// installed.
	Digest string

	// Dir is where it lives, for reading the files beside it.
	Dir string
}

// frontmatter is the part of SKILL.md a machine reads.
//
// Deliberately small. Every field here is either how the skill is found or
// how it is described; there is nothing that could change what the agent is
// allowed to do, because there is nowhere for such a field to be honoured.
type frontmatter struct {
	Description string `yaml:"description"`
	Version     string `yaml:"version"`
}

// Rejected is a directory that looked like a skill and could not be read.
//
// Kept rather than dropped: a skill that silently does not appear is one an
// operator will spend an afternoon on, and the reason is always in the file.
type Rejected struct {
	Name   string
	Reason string
}

// Installed reads every skill in a directory.
//
// A missing directory is not an error; most deployments have no skills, and
// asking for a list of them should say so rather than fail.
func Installed(root string) ([]Skill, []Rejected, error) {
	entries, err := os.ReadDir(root)
	if os.IsNotExist(err) {
		return nil, nil, nil
	}
	if err != nil {
		return nil, nil, fmt.Errorf("skill: read %s: %w", root, err)
	}

	var (
		found    []Skill
		rejected []Rejected
	)

	for _, entry := range entries {
		if !entry.IsDir() {
			continue
		}

		one, err := Read(filepath.Join(root, entry.Name()))
		if err != nil {
			rejected = append(rejected, Rejected{Name: entry.Name(), Reason: err.Error()})
			continue
		}
		found = append(found, one)
	}

	// By name, so a catalogue reads the same twice running and a diff of two
	// prompts is about what changed rather than about map iteration.
	sort.Slice(found, func(i, j int) bool { return found[i].Name < found[j].Name })
	sort.Slice(rejected, func(i, j int) bool { return rejected[i].Name < rejected[j].Name })

	return found, rejected, nil
}

// Read loads one skill from its directory.
func Read(dir string) (Skill, error) {
	path := filepath.Join(dir, FileName)

	raw, err := os.ReadFile(path)
	if err != nil {
		return Skill{}, fmt.Errorf("no %s", FileName)
	}

	front, body, err := split(string(raw))
	if err != nil {
		return Skill{}, err
	}

	var declared frontmatter
	if err := yaml.Unmarshal([]byte(front), &declared); err != nil {
		return Skill{}, fmt.Errorf("frontmatter is not readable: %w", err)
	}
	if strings.TrimSpace(declared.Description) == "" {
		// Without one there is nothing to put in the catalogue, and a skill
		// the model cannot tell apart from the others is a skill it will
		// never ask for.
		return Skill{}, fmt.Errorf("no description")
	}

	sum := sha256.Sum256(raw)

	return Skill{
		Name:        filepath.Base(dir),
		Description: strings.TrimSpace(declared.Description),
		Version:     strings.TrimSpace(declared.Version),
		Body:        strings.TrimSpace(body),
		Digest:      "sha256:" + hex.EncodeToString(sum[:]),
		Dir:         dir,
	}, nil
}

// split separates the frontmatter from the instructions.
//
// The fence is three dashes on a line of their own, which is the convention
// every tool that reads these files already shares. A file without one is a
// file that has not said what it is.
func split(source string) (front, body string, err error) {
	const fence = "---"

	trimmed := strings.TrimLeft(source, "\ufeff \t\r\n")
	if !strings.HasPrefix(trimmed, fence) {
		return "", "", fmt.Errorf("does not start with %s", fence)
	}

	rest := trimmed[len(fence):]
	rest = strings.TrimLeft(rest, " \t\r")
	rest = strings.TrimPrefix(rest, "\n")

	end := strings.Index(rest, "\n"+fence)
	if end < 0 {
		return "", "", fmt.Errorf("the frontmatter is never closed with %s", fence)
	}

	front = rest[:end]
	after := rest[end+1+len(fence):]

	return front, strings.TrimPrefix(after, "\n"), nil
}
