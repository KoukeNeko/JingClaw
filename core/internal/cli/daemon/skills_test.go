package daemon

import (
	"context"
	"log/slog"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"connectrpc.com/connect"

	controlv1 "github.com/KoukeNeko/JingClaw/core/gen/go/jingclaw/control/v1"
	"github.com/KoukeNeko/JingClaw/core/gen/go/jingclaw/control/v1/controlv1connect"
	"github.com/KoukeNeko/JingClaw/core/internal/home"
	"github.com/KoukeNeko/JingClaw/core/internal/skill"
)

func aSkillOn(t *testing.T, root, directory, name, description string) {
	t.Helper()

	dir := filepath.Join(root, home.SkillsName, directory)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	text := "---\nname: " + name + "\ndescription: " + description + "\n---\n\nDo the thing.\n"
	if err := os.WriteFile(filepath.Join(dir, skill.FileName), []byte(text), 0o644); err != nil {
		t.Fatal(err)
	}
}

func (d *running) skills() controlv1connect.SkillServiceClient {
	return controlv1connect.NewSkillServiceClient(http.DefaultClient, d.file.BaseURL,
		connect.WithInterceptors(bearer(d.file.Token)))
}

func (d *running) listSkills() *controlv1.ListSkillsResponse {
	d.t.Helper()

	resp, err := d.skills().ListSkills(context.Background(), connect.NewRequest(&controlv1.ListSkillsRequest{}))
	if err != nil {
		d.t.Fatalf("ListSkills: %v", err)
	}
	return resp.Msg
}

// What the model was told about skills is what the prompt of this start said,
// and the disk moves on from it. The service is asked at the other end of a
// real start, because the snapshot is taken where the prompt is built.
func TestTheSkillsServiceComparesTheDiskWithTheCatalogueOfThisStart(t *testing.T) {
	root := isolate(t)
	if _, err := home.Create(root); err != nil {
		t.Fatal(err)
	}
	aSkillOn(t, root, "release", "release", "How this repository is released.")

	d := start(t, root)

	byName := func(listed *controlv1.ListSkillsResponse) map[string]*controlv1.InstalledSkill {
		out := map[string]*controlv1.InstalledSkill{}
		for _, one := range listed.Installed {
			out[one.Name] = one
		}
		return out
	}

	got := d.listSkills()
	if got.RestartRequired || !byName(got)["release"].InCatalogue {
		t.Fatalf("a skill that was there at the start: %+v", got)
	}

	// Put there while it runs: on disk, and not in what the model was told.
	aSkillOn(t, root, "added", "added", "Installed after the start.")
	got = d.listSkills()
	if !got.RestartRequired || byName(got)["added"].InCatalogue || !byName(got)["release"].InCatalogue {
		t.Errorf("a skill added while the daemon ran: %+v", got)
	}

	d.stop()
	again := start(t, root)
	got = again.listSkills()
	if got.RestartRequired || !byName(got)["added"].InCatalogue || !byName(got)["release"].InCatalogue {
		t.Errorf("after the restart: %+v", got)
	}
}

// The catalogue and the snapshot are made from one read, so they cannot
// disagree about what was offered.
func TestTheCatalogueAndWhatWasOfferedAreOneRead(t *testing.T) {
	root := isolate(t)
	aSkillOn(t, root, "release", "release", "How this repository is released.")
	aSkillOn(t, root, "broken-directory", "x", "")
	aSkillOn(t, root, "renamed-directory", "triage", "How an incident is triaged.")

	catalogue, offered := skillCatalogue(slog.New(slog.DiscardHandler))

	if len(offered) != 2 || offered["release"] != "How this repository is released." ||
		offered["triage"] != "How an incident is triaged." {
		t.Errorf("offered %v", offered)
	}
	for name := range offered {
		if !strings.Contains(catalogue, name) {
			t.Errorf("%s is offered and the catalogue does not list it:\n%s", name, catalogue)
		}
	}
	if strings.Contains(catalogue, "broken-directory") || strings.Contains(catalogue, "- x") {
		t.Errorf("a skill that could not be read is in the catalogue:\n%s", catalogue)
	}
}

// A skill the agent installs is recorded as installed, with where it came from,
// like one installed from the command line or the dashboard. It used to be
// written down by the command line alone, so one the agent installed looked as
// though somebody had put it there by hand.
func TestASkillTheAgentActivatesIsRecordedLikeAnyOther(t *testing.T) {
	root := isolate(t)
	if _, err := home.Create(root); err != nil {
		t.Fatal(err)
	}

	repo := filepath.Join(t.TempDir(), "repo")
	if err := os.MkdirAll(filepath.Join(repo, "release"), 0o755); err != nil {
		t.Fatal(err)
	}
	text := "---\nname: proposed\ndescription: A skill the agent fetched.\n---\n\nDo it.\n"
	if err := os.WriteFile(filepath.Join(repo, "release", skill.FileName), []byte(text), 0o644); err != nil {
		t.Fatal(err)
	}
	var commit string
	for _, args := range [][]string{
		{"init", "--quiet", "--initial-branch=main"}, {"add", "-A"},
		{"-c", "user.name=t", "-c", "user.email=t@example.com", "commit", "--quiet", "-m", "a skill"},
		{"rev-parse", "HEAD"},
	} {
		command := exec.Command("git", args...)
		command.Dir = repo
		out, err := command.CombinedOutput()
		if err != nil {
			t.Skipf("git is not usable here (%v): %s", err, out)
		}
		commit = strings.TrimSpace(string(out))
	}

	source := skill.Source{Repository: repo, Commit: commit, Path: "release"}
	staged, err := deploymentSkills{}.Stage(context.Background(), source)
	if err != nil {
		t.Fatalf("stage: %v", err)
	}
	if _, err := (deploymentSkills{}).Activate(staged.Name); err != nil {
		t.Fatalf("activate: %v", err)
	}

	lock, err := skill.ReadLock(filepath.Join(root, home.SkillsName))
	if err != nil {
		t.Fatalf("read the record: %v", err)
	}
	entry, found := lock.Entry("proposed")
	if !found {
		t.Fatalf("a skill the agent activated is not recorded: %+v", lock)
	}
	if entry.From != source || entry.TreeDigest != staged.TreeDigest || entry.InstalledAt.IsZero() {
		t.Errorf("the record is %+v, want where it came from and what arrived", entry)
	}
}
