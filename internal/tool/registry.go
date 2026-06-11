// Package tool provides a registry of function-calling tools.
//
// A ToolSpec describes one tool's name, description, JSON-Schema parameters and
// Execute callback. Agents look up their configured tool names through a
// *Registry; the registry also renders the OpenAI tools[] shape via
// OpenAITools so the agent layer can pass it straight to the model API.
package tool

import (
	"context"
	"encoding/json"
	"fmt"
	"sort"
	"sync"
)

// ToolSpec describes a single tool that an agent can invoke via function
// calling. ParametersJSON is a JSON Schema describing the args the model must
// emit; Execute parses those args (delivered as json.RawMessage by the agent
// loop) and returns a JSON-serializable result.
type ToolSpec struct {
	Name           string
	Description    string
	ParametersJSON json.RawMessage
	Execute        func(ctx context.Context, args json.RawMessage) (any, error)
}

// Registry holds the set of tools known to the process. Registration happens at
// startup; lookups happen per agent invocation. It is safe for concurrent use.
type Registry struct {
	mu    sync.RWMutex
	tools map[string]*ToolSpec
}

// NewRegistry returns an empty Registry.
func NewRegistry() *Registry {
	return &Registry{tools: make(map[string]*ToolSpec)}
}

// Register adds spec to the registry. It returns an error if spec.Name is empty
// or a tool with that name is already registered — duplicate registration would
// silently mask an earlier tool and is treated as a programming error.
func (r *Registry) Register(spec *ToolSpec) error {
	if spec == nil {
		return fmt.Errorf("tool registry: cannot register nil spec")
	}
	if spec.Name == "" {
		return fmt.Errorf("tool registry: spec.Name is empty")
	}
	if spec.Execute == nil {
		return fmt.Errorf("tool registry: %q has nil Execute", spec.Name)
	}

	r.mu.Lock()
	defer r.mu.Unlock()
	if _, exists := r.tools[spec.Name]; exists {
		return fmt.Errorf("tool registry: tool %q already registered", spec.Name)
	}
	r.tools[spec.Name] = spec
	return nil
}

// Get returns the spec registered under name and ok=true, or (nil, false) when
// no such tool exists.
func (r *Registry) Get(name string) (*ToolSpec, bool) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	spec, ok := r.tools[name]
	return spec, ok
}

// MustGet returns the spec registered under name, panicking if it is missing.
// Intended for use at startup where a missing tool is a fatal config error.
func (r *Registry) MustGet(name string) *ToolSpec {
	spec, ok := r.Get(name)
	if !ok {
		panic(fmt.Sprintf("tool registry: required tool %q not registered", name))
	}
	return spec
}

// List returns every registered spec sorted by name. The order is stable so
// the MCP tools-list endpoint is deterministic.
func (r *Registry) List() []*ToolSpec {
	r.mu.RLock()
	defer r.mu.RUnlock()
	out := make([]*ToolSpec, 0, len(r.tools))
	for _, spec := range r.tools {
		out = append(out, spec)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out
}

// ToolsFor resolves names to specs in the order given. Any unknown name causes
// an error that lists all missing names so misconfigured agents fail loudly at
// startup.
func (r *Registry) ToolsFor(names []string) ([]*ToolSpec, error) {
	specs := make([]*ToolSpec, 0, len(names))
	var missing []string
	for _, name := range names {
		spec, ok := r.Get(name)
		if !ok {
			missing = append(missing, name)
			continue
		}
		specs = append(specs, spec)
	}
	if len(missing) > 0 {
		return nil, fmt.Errorf("tool registry: unknown tools %v", missing)
	}
	return specs, nil
}

// Call looks up name and invokes its Execute with args. It is a convenience
// used by the agent loop's tool-call dispatcher.
func (r *Registry) Call(ctx context.Context, name string, args json.RawMessage) (any, error) {
	spec, ok := r.Get(name)
	if !ok {
		return nil, fmt.Errorf("tool registry: unknown tool %q", name)
	}
	return spec.Execute(ctx, args)
}

// openAITool is the per-entry shape rendered by OpenAITools. The "function"
// object matches OpenAI's tools API exactly.
type openAITool struct {
	Type     string         `json:"type"`
	Function openAIToolFunc `json:"function"`
}

type openAIToolFunc struct {
	Name        string          `json:"name"`
	Description string          `json:"description"`
	Parameters  json.RawMessage `json:"parameters"`
}

// OpenAITools renders the named tools in the OpenAI tools[] request shape:
//
//	[{"type":"function","function":{"name":"...","description":"...","parameters":{...}}}]
//
// The result is returned as []byte so callers can decode it into whatever
// OpenAI client shape they use. If any name is unknown, the error lists every
// missing name. A nil or empty parameters schema is rendered as "{}".
func (r *Registry) OpenAITools(names []string) ([]byte, error) {
	specs, err := r.ToolsFor(names)
	if err != nil {
		return nil, err
	}
	out := make([]openAITool, 0, len(specs))
	for _, spec := range specs {
		params := spec.ParametersJSON
		if len(params) == 0 {
			params = json.RawMessage(`{}`)
		}
		out = append(out, openAITool{
			Type: "function",
			Function: openAIToolFunc{
				Name:        spec.Name,
				Description: spec.Description,
				Parameters:  params,
			},
		})
	}
	b, err := json.Marshal(out)
	if err != nil {
		return nil, fmt.Errorf("tool registry: marshal openai tools: %w", err)
	}
	return b, nil
}
