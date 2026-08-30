package skill

import (
	"strings"
	"testing"
)

var two = []Skill{
	{
		Name:        "kubernetes-rollout",
		Description: "Inspect and operate Kubernetes rollouts.",
		Body:        "Run kubectl rollout status and read what it says.",
	},
	{
		Name:        "release",
		Description: "How this repository is released.",
		Body:        "Tag, then push the tag; CI does the rest.",
	},
}

// The whole point of the arrangement: the model is told what exists without
// being told everything they say.
func TestTheCatalogueCarriesNoInstructions(t *testing.T) {
	catalogue := Catalogue(two)

	for _, one := range two {
		if !strings.Contains(catalogue, one.Name) {
			t.Errorf("%q is not in the catalogue", one.Name)
		}
		if !strings.Contains(catalogue, one.Description) {
			t.Errorf("the description of %q is not in the catalogue", one.Name)
		}
		if strings.Contains(catalogue, one.Body) {
			t.Errorf("the catalogue carries the instructions of %q", one.Name)
		}
	}
}

// A deployment with no skills carries no paragraph explaining that it has
// none.
func TestNoSkillsIsNothingAtAll(t *testing.T) {
	if catalogue := Catalogue(nil); catalogue != "" {
		t.Errorf("with no skills the catalogue is %q", catalogue)
	}
}

// It says what a skill is for, so a model reading it does not take a listing
// of names as a list of things it may now do.
func TestTheCatalogueSaysWhatASkillIsNot(t *testing.T) {
	catalogue := strings.ToLower(Catalogue(two))

	for _, expected := range []string{"not a new ability", "not permission"} {
		if !strings.Contains(catalogue, expected) {
			t.Errorf("the catalogue does not say %q:\n%s", expected, catalogue)
		}
	}
}
