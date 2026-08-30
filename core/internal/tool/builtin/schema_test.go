package builtin

import (
	"encoding/json"
	"strings"
	"testing"

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
