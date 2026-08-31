package builtin

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/KoukeNeko/JingClaw/core/internal/domain"
	"github.com/KoukeNeko/JingClaw/core/internal/tool"
)

// TestEveryToolThatTakesArgumentsDeclaresThem is what makes a wrong call
// answerable.
//
// A tool with no schema is not merely unvalidated. The schema is also what the
// model is shown, so a tool without one is one whose arguments have to be
// guessed from its description — and a guess that is wrong arrives here as an
// empty struct, which reads as "you gave me nothing" rather than "that field
// is called something else".
//
// This exists because it happened: an edit to a description took the schema
// out with it, and the symptom was a model calling the tool four times with
// the wrong field name and then giving up.
func TestEveryToolThatTakesArgumentsDeclaresThem(t *testing.T) {
	for _, one := range []tool.Tool{
		&Investigate{},
		&ReadFile{},
		&GlobFiles{},
		&Grep{},
		&SkillLoad{},
		&TodoUpdate{},
		&AskUser{},
		&ExecCommand{},
		&GitDiff{},
		&ReadArtifact{},
	} {
		spec := one.Spec()

		if len(spec.InputSchema) == 0 {
			t.Errorf("%s declares no arguments; if that is true say so with an "+
				"empty object schema, and if it is not the model cannot call it",
				spec.Name)
			continue
		}

		var declared struct {
			Type       string                     `json:"type"`
			Properties map[string]json.RawMessage `json:"properties"`
			Required   []string                   `json:"required"`
		}
		if err := json.Unmarshal(spec.InputSchema, &declared); err != nil {
			t.Errorf("%s: schema is not readable: %v", spec.Name, err)
			continue
		}
		if declared.Type != "object" {
			t.Errorf("%s: schema is %q, not an object", spec.Name, declared.Type)
		}

		// Every argument the description talks about by name has to be in the
		// schema. The description is prose and can drift; this is the part the
		// model is actually handed.
		for _, named := range declared.Required {
			if _, ok := declared.Properties[named]; !ok {
				t.Errorf("%s requires %q but never describes it", spec.Name, named)
			}
		}
	}
}

// TestInvestigateNamesItsArgumentWhereTheModelWillRead is narrower and worth
// having: this is the tool whose whole argument is one field, and a model that
// guesses the name gets an error that does not say what the name is.
func TestInvestigateNamesItsArgumentWhereTheModelWillRead(t *testing.T) {
	spec := (&Investigate{}).Spec()

	var declared struct {
		Properties map[string]struct {
			Type        string `json:"type"`
			Description string `json:"description"`
		} `json:"properties"`
		Required             []string `json:"required"`
		AdditionalProperties *bool    `json:"additionalProperties"`
	}
	if err := json.Unmarshal(spec.InputSchema, &declared); err != nil {
		t.Fatalf("schema: %v", err)
	}

	question, ok := declared.Properties["question"]
	if !ok {
		t.Fatalf("investigate does not declare a question: %s", spec.InputSchema)
	}
	if question.Type != "string" || strings.TrimSpace(question.Description) == "" {
		t.Errorf("the question is not a described string: %+v", question)
	}
	if len(declared.Required) != 1 || declared.Required[0] != "question" {
		t.Errorf("required: %v", declared.Required)
	}
	// Closed, so a model that calls this with the wrong field name is told the
	// field name rather than being told it sent nothing.
	if declared.AdditionalProperties == nil || *declared.AdditionalProperties {
		t.Error("the schema accepts fields it does not understand")
	}
}

// handsBackWhatItDidNotCompose is every tool whose result is text somebody or
// something else produced.
//
// Listed rather than derived, because there is no capability that means it: a
// tool that reads the artifact store touches no workspace and no network and
// still hands back a page it did not write. What is derived is the other
// direction — see the test below, which catches a tool added later.
var handsBackWhatItDidNotCompose = []tool.Tool{
	&ReadFile{}, &GlobFiles{}, &Grep{}, &GitStatus{}, &GitDiff{},
	&ExecCommand{}, &ReadArtifact{}, &WebRead{}, &WebSearch{},
	&Investigate{}, &StartProcess{}, &ProcessIO{}, &SkillLoad{},
}

// TestAnythingThatReadsSaysWhoseWordsComeBack is what stops a tool being
// added without anyone thinking about it.
//
// Left at the zero value a tool claims to return the operator's own words,
// which is how a command's output ends up eligible to become a standing
// instruction — the hole this was built to close.
func TestAnythingThatReadsSaysWhoseWordsComeBack(t *testing.T) {
	for _, one := range handsBackWhatItDidNotCompose {
		spec := one.Spec()
		if spec.Capabilities.Provenance.IsOperator() {
			t.Errorf("%s hands back text it did not compose and claims to return "+
				"the operator's own words; say local_unknown for this machine or "+
				"external for somebody else's", spec.Name)
		}
	}
}

// TestAToolThatReachesIsOnThatList is the half that catches a new one.
//
// Reaching the filesystem or the network is not what makes a result somebody
// else's words, but it does mean somebody had to decide — so a tool that
// reaches and is not on the list above is one nobody has thought about.
func TestAToolThatReachesIsOnThatList(t *testing.T) {
	listed := make(map[string]bool, len(handsBackWhatItDidNotCompose))
	for _, one := range handsBackWhatItDidNotCompose {
		listed[one.Spec().Name] = true
	}

	for _, one := range everyBuiltin() {
		spec := one.Spec()
		if !spec.Capabilities.ReadFS && !spec.Capabilities.Network {
			continue
		}
		if !listed[spec.Name] {
			t.Errorf("%s reaches outside itself and nobody has said whose words "+
				"it hands back; add it to handsBackWhatItDidNotCompose", spec.Name)
		}
	}
}

// everyBuiltin is what this package offers, for the check above.
func everyBuiltin() []tool.Tool {
	return []tool.Tool{
		&ReadFile{}, &GlobFiles{}, &Grep{}, &GitStatus{}, &GitDiff{},
		&ExecCommand{}, &ReadArtifact{}, &WebRead{}, &WebSearch{},
		&Investigate{}, &StartProcess{}, &ProcessIO{}, &StopProcess{},
		&ListProcesses{}, &SkillLoad{}, &TodoUpdate{}, &AskUser{},
	}
}

// TestSomebodyElsesWordsAreSaidToBeSomebodyElses keeps the two axes agreeing.
//
// A tool that returns a stranger's text has to say both: Foreign, which is
// shown to whoever is deciding whether to allow a call, and external, which
// is what a privileged sink consults. They are read by different people and
// neither implies the other, so a tool declaring one and not the other is a
// tool that is right in one place and wrong in the other.
func TestSomebodyElsesWordsAreSaidToBeSomebodyElses(t *testing.T) {
	for _, one := range []tool.Tool{&WebRead{}, &WebSearch{}} {
		spec := one.Spec()

		if !spec.Capabilities.ForeignContent {
			t.Errorf("%s returns somebody else's words and does not say so where "+
				"an approval would show it", spec.Name)
		}
		if spec.Capabilities.Provenance != domain.ProvenanceExternal {
			t.Errorf("%s returns somebody else's words and records them as %q",
				spec.Name, spec.Capabilities.Provenance)
		}
	}
}
