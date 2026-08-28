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
	"time"

	"go.uber.org/zap"

	"github.com/lush/blowball/internal/pkg/logger"
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
	mu       sync.RWMutex
	tools    map[string]*ToolSpec
	timeouts map[string]time.Duration
}

// NewRegistry returns an empty Registry.
func NewRegistry() *Registry {
	return &Registry{tools: make(map[string]*ToolSpec)}
}

// SetTimeouts configures the per-tool execution timeout map applied by Call
// (capability: tool-execution-timeout). A tool name mapped to a positive
// duration is bounded by that duration on every Call; absent entries and
// zero/negative durations are unbounded (the prior behavior). It is intended to
// be called once at wiring time, before any Call. Safe for concurrent use.
func (r *Registry) SetTimeouts(m map[string]time.Duration) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.timeouts = m
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
// used by the agent loop's tool-call dispatcher. When a positive timeout is
// configured for name via SetTimeouts, the Execute runs under a child context
// bounded by that duration; otherwise execution is unbounded (the prior
// behavior). The timeout composes with any native tool timeout as a looser
// outer backstop (capability: tool-execution-timeout).
//
// Every call emits the central dispatch log (tool-call-log-correlation
// capability) through logger.FromContext — one structured line carrying
// event=tool_call, the tool name, the full execution duration (timeout
// wrapping included), and a truncated args preview; INFO on success, WARN
// with the error on failure. This is the single choke point shared by all
// three agents' registry tools and the per-turn mcp_* family, so slow or
// failing tool calls (remote MCP round trips included) are observable per
// session/trace without touching each tool.
func (r *Registry) Call(ctx context.Context, name string, args json.RawMessage) (any, error) {
	spec, ok := r.Get(name)
	if !ok {
		return nil, fmt.Errorf("tool registry: unknown tool %q", name)
	}
	start := time.Now()
	if d, hasTimeout := r.timeoutFor(name); hasTimeout {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, d)
		defer cancel()
	}
	result, err := spec.Execute(ctx, args)
	logToolCall(ctx, name, args, time.Since(start), err)
	return result, err
}

// argsPreviewLimit is the maximum number of runes written into the dispatch
// log's args_preview field. Tool arguments are model-authored content already
// stored in full (llm_raw_log request rows, messages table); the preview only
// needs enough to recognize the call.
const argsPreviewLimit = 200

// truncateArgsPreview renders args (raw JSON bytes) as a string cut to
// argsPreviewLimit runes, appending "…" when truncation occurred — the same
// shape as the agent package's truncatePreview, kept local so the tool layer
// never imports internal/agent.
func truncateArgsPreview(args json.RawMessage) string {
	s := string(args)
	if s == "" {
		return ""
	}
	runes := []rune(s)
	if len(runes) <= argsPreviewLimit {
		return s
	}
	return string(runes[:argsPreviewLimit]) + "…"
}

// logToolCall emits the central registry dispatch log. Failure of the log
// itself can never affect the dispatch: zap writes are non-blocking sinks and
// the emission sits strictly after Execute returned. The result body is
// deliberately NOT logged — it can be huge and already lands in the
// messages table via the tool_result event.
func logToolCall(ctx context.Context, name string, args json.RawMessage, dur time.Duration, err error) {
	log := logger.FromContext(ctx).With(
		zap.String("event", "tool_call"),
		zap.String("tool", name),
		zap.Duration("duration", dur),
		zap.String("args_preview", truncateArgsPreview(args)),
	)
	if err != nil {
		log.Warn("registry tool call failed", zap.Error(err))
		return
	}
	log.Info("registry tool call completed")
}

// timeoutFor returns the configured execution timeout for name and whether one
// applies. Absent entries and zero/negative durations are treated as unbounded
// (no timeout), preserving the prior behavior for unmapped tools. Safe for
// concurrent use with SetTimeouts.
func (r *Registry) timeoutFor(name string) (time.Duration, bool) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	if r.timeouts == nil {
		return 0, false
	}
	d := r.timeouts[name]
	if d <= 0 {
		return 0, false
	}
	return d, true
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
