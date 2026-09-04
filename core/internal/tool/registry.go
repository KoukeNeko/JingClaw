package tool

import (
	"context"
	"encoding/json"
	"fmt"
	"sort"
	"sync"

	"github.com/santhosh-tekuri/jsonschema/v6"
	"golang.org/x/text/language"
	"golang.org/x/text/message"
)

// The schema library renders messages through a localiser and panics on a nil
// printer, so one is created up front. English keeps failures identical
// regardless of the daemon's locale, which matters because these strings are
// read by a model, not by a person.
var schemaPrinter = message.NewPrinter(language.English)

// Registry holds the tools available to a run and validates calls against
// their declared schemas.
//
// Validation happens here rather than inside each tool so that no tool can
// forget to do it, and so a malformed call becomes a structured observation
// the model can correct instead of a panic three layers down.
type Registry struct {
	mu       sync.RWMutex
	tools    map[string]Tool
	compiled map[string]*jsonschema.Schema
}

func NewRegistry() *Registry {
	return &Registry{
		tools:    make(map[string]Tool),
		compiled: make(map[string]*jsonschema.Schema),
	}
}

// Register adds a tool. Schemas are compiled at registration so a broken one
// fails at startup rather than the first time a model tries to use it.
func (r *Registry) Register(t Tool) error {
	spec := t.Spec()
	if spec.Name == "" {
		return fmt.Errorf("tool: registered with no name")
	}

	schema, err := compileSchema(spec)
	if err != nil {
		return err
	}

	r.mu.Lock()
	defer r.mu.Unlock()

	if _, exists := r.tools[spec.Name]; exists {
		return fmt.Errorf("tool: %s is already registered", spec.Name)
	}
	r.tools[spec.Name] = t
	r.compiled[spec.Name] = schema

	return nil
}

// MustRegister is Register for wiring that cannot meaningfully continue.
func (r *Registry) MustRegister(tools ...Tool) {
	for _, t := range tools {
		if err := r.Register(t); err != nil {
			panic(err)
		}
	}
}

// Specs lists every tool the model is shown, in a stable order so the prompt
// prefix stays byte-identical between runs and remains cacheable.
//
// A deferred tool is registered — Lookup and Execute know it — and is not
// here: it reaches the model only once a run has loaded it. DeferredSpecs is
// the other half.
func (r *Registry) Specs() []Spec {
	return r.specsWhere(func(spec Spec) bool { return !spec.Deferred })
}

// DeferredSpecs lists the tools kept out of the prompt until asked for.
func (r *Registry) DeferredSpecs() []Spec {
	return r.specsWhere(func(spec Spec) bool { return spec.Deferred })
}

func (r *Registry) specsWhere(keep func(Spec) bool) []Spec {
	r.mu.RLock()
	defer r.mu.RUnlock()

	specs := make([]Spec, 0, len(r.tools))
	for _, t := range r.tools {
		if spec := t.Spec(); keep(spec) {
			specs = append(specs, spec)
		}
	}
	sort.Slice(specs, func(i, j int) bool { return specs[i].Name < specs[j].Name })

	return specs
}

func (r *Registry) Lookup(name string) (Tool, bool) {
	r.mu.RLock()
	defer r.mu.RUnlock()

	t, ok := r.tools[name]
	return t, ok
}

// Execute validates and runs a call, always returning a Result the model can
// read. An unknown tool or bad arguments are observations, not failures of the
// run: the model asked for something impossible and needs to be told so.
func (r *Registry) Execute(ctx context.Context, call Call) Result {
	r.mu.RLock()
	t, known := r.tools[call.Name]
	schema := r.compiled[call.Name]
	r.mu.RUnlock()

	if !known {
		return Errorf(CodeNotFound,
			"Use one of the tools listed in this conversation.",
			"no tool named %q", call.Name).Result()
	}

	if err := validate(schema, call.Arguments); err != nil {
		return err.Result()
	}

	result, err := t.Execute(ctx, call)
	if err != nil {
		var toolErr *Error
		if ok := asToolError(err, &toolErr); ok {
			return toolErr.Result()
		}
		// An unclassified failure still has to reach the model, or it will
		// simply try the same thing again.
		return Errorf(CodeInternal, "", "%s failed: %v", call.Name, err).Result()
	}

	return result
}

func validate(schema *jsonschema.Schema, arguments json.RawMessage) *Error {
	if schema == nil {
		return nil
	}

	// A tool with no arguments may legitimately be called with nothing at all.
	raw := arguments
	if len(raw) == 0 {
		raw = json.RawMessage("{}")
	}

	var decoded any
	if err := json.Unmarshal(raw, &decoded); err != nil {
		return Errorf(CodeInvalidArguments,
			"Send the arguments as a JSON object matching the tool's schema.",
			"arguments are not valid JSON: %v", err)
	}

	if err := schema.Validate(decoded); err != nil {
		return Errorf(CodeInvalidArguments,
			"Correct the arguments and call the tool again.",
			"%s", schemaFailure(err))
	}

	return nil
}

// schemaFailure renders a validation failure compactly. The library's default
// rendering is a multi-line tree that would dominate the context window.
func schemaFailure(err error) string {
	var validation *jsonschema.ValidationError
	if !asValidationError(err, &validation) {
		return err.Error()
	}

	leaves := collectLeaves(validation, nil)
	if len(leaves) == 0 {
		return validation.Error()
	}

	const maxReported = 5
	if len(leaves) > maxReported {
		leaves = leaves[:maxReported]
	}

	message := leaves[0]
	for _, leaf := range leaves[1:] {
		message += "; " + leaf
	}
	return message
}

// collectLeaves walks to the most specific causes, which are the ones that say
// what is actually wrong rather than restating the schema structure.
func collectLeaves(err *jsonschema.ValidationError, into []string) []string {
	if len(err.Causes) == 0 {
		location := err.InstanceLocation
		if len(location) == 0 {
			return append(into, err.ErrorKind.LocalizedString(schemaPrinter))
		}
		path := ""
		for _, segment := range location {
			path += "/" + segment
		}
		return append(into, path+": "+err.ErrorKind.LocalizedString(schemaPrinter))
	}

	for _, cause := range err.Causes {
		into = collectLeaves(cause, into)
	}
	return into
}

func compileSchema(spec Spec) (*jsonschema.Schema, error) {
	if len(spec.InputSchema) == 0 {
		return nil, nil
	}

	doc, err := jsonschema.UnmarshalJSON(bytesReader(spec.InputSchema))
	if err != nil {
		return nil, fmt.Errorf("tool %s: input schema is not valid JSON: %w", spec.Name, err)
	}

	compiler := jsonschema.NewCompiler()
	resource := "jingclaw:tool/" + spec.Name
	if err := compiler.AddResource(resource, doc); err != nil {
		return nil, fmt.Errorf("tool %s: input schema: %w", spec.Name, err)
	}

	schema, err := compiler.Compile(resource)
	if err != nil {
		return nil, fmt.Errorf("tool %s: input schema: %w", spec.Name, err)
	}

	return schema, nil
}
