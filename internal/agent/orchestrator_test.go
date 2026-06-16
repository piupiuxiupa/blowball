package agent

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/lush/blowball/internal/config"
	"github.com/lush/blowball/internal/stream"
	"github.com/lush/blowball/internal/tool"
	"github.com/lush/blowball/internal/tool/skill"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.uber.org/goleak"
)

// newTestOrchestrator constructs an Orchestrator against a fake client and a
// minimal config. The Chongzhi agent built inside the factory has tools
// bound to a temporary workspace via xizhi.RegisterAll; we don't exercise
// them here, but the factory must not error.
func newTestOrchestrator(t *testing.T, client LLMClient) *Orchestrator {
	t.Helper()
	cfg := &config.Config{
		OpenAI: config.OpenAIConfig{APIKey: "test", Model: "gpt-test"},
		Agents: config.AgentsConfig{
			Confuse: config.AgentConfig{
				Name:         "Confuse",
				Model:        "gpt-test",
				SystemPrompt: "you are confuse",
				MaxTokens:    256,
				Tools:        []string{},
			},
			Chongzhi: config.AgentConfig{
				Name:         "Chongzhi",
				Model:        "gpt-test",
				SystemPrompt: "you are chongzhi",
				MaxTokens:    256,
				Tools:        []string{"xizhi_write_file", "xizhi_read_file", "xizhi_modify_file"},
			},
			Liang: config.AgentConfig{
				Name:         "Liang",
				Model:        "gpt-test",
				SystemPrompt: "you are liang",
				MaxTokens:    256,
			},
		},
	}
	o, err := NewOrchestrator(client, cfg, nil, nil, skill.NewLoader("", nil), nil)
	require.NoError(t, err)
	return o
}

func TestOrchestrator_Handle_FullFlow(t *testing.T) {
	defer goleak.VerifyNone(t)
	client := newFake(
		fakeResponse{
			tokens:       []string{"hello"},
			content:      "hello",
			finishReason: "stop",
			usage:        Usage{PromptTokens: 10, CompletionTokens: 1, TotalTokens: 11},
		},
	)
	o := newTestOrchestrator(t, client)

	hub := stream.NewHub(0)
	ctx, cancel := context.WithTimeout(context.Background(), 4*time.Second)
	defer cancel()

	type res struct{ err error }
	resCh := make(chan res, 1)
	go func() {
		resCh <- res{err: o.Handle(ctx, t.TempDir(), "user-1", "hi", hub)}
	}()

	var events []stream.StreamEvent
consumer:
	for {
		select {
		case e := <-hub.Events():
			events = append(events, e)
		case r := <-resCh:
			require.NoError(t, r.err)
		drain:
			for {
				select {
				case e := <-hub.Events():
					events = append(events, e)
				default:
					break drain
				}
			}
			break consumer
		case <-time.After(3 * time.Second):
			t.Fatal("orchestrator did not finish")
		}
	}
	hub.Close()

	// Event sequence: agent_start (Confuse), token (Confuse), agent_end
	// (Confuse), done.
	types := eventTypes(events)
	assert.Equal(t, stream.EventAgentStart, types[0])
	assert.Equal(t, stream.EventDone, types[len(types)-1])

	// The done event must carry Meta.usage with token totals.
	var doneEvent *stream.StreamEvent
	for i := range events {
		if events[i].Type == stream.EventDone {
			doneEvent = &events[i]
		}
	}
	require.NotNil(t, doneEvent, "expected a done event")
	usage, ok := doneEvent.Meta[stream.MetaUsage].(map[string]any)
	require.True(t, ok, "done event Meta.usage must be map[string]any; got %T", doneEvent.Meta[stream.MetaUsage])
	assert.Equal(t, 11, usage["total_tokens"])
	assert.Equal(t, 10, usage["prompt_tokens"])
	assert.Equal(t, 1, usage["completion_tokens"])

	// Round 1 must have included Confuse's system prompt + user message.
	req := client.lastRequest()
	require.NotEmpty(t, req.Messages)
	assert.Equal(t, "system", req.Messages[0].Role)
	assert.Contains(t, req.Messages[0].Content, "confuse")
	assert.Equal(t, "user", req.Messages[1].Role)
	assert.Equal(t, "hi", req.Messages[1].Content)
}

func TestOrchestrator_ExternalMCPTool(t *testing.T) {
	defer goleak.VerifyNone(t)

	var toolCalled bool
	baseReg := tool.NewRegistry()
	require.NoError(t, baseReg.Register(
		&tool.ToolSpec{
			Name:           "external_greet",
			Description:    "external greeting tool",
			ParametersJSON: json.RawMessage(`{"type":"object","properties":{"name":{"type":"string"}},"required":["name"]}`),
			Execute: func(ctx context.Context, args json.RawMessage) (any, error) {
				toolCalled = true
				return "hello from external", nil
			},
		}))

	client := newFake(
		fakeResponse{
			tokens:       []string{"call"},
			content:      "",
			finishReason: "tool_calls",
			toolCalls: []ToolCall{{
				ID: "tc_1",
				Function: ToolCallFunction{
					Name:      "external_greet",
					Arguments: `{"name":"world"}`,
				},
			}},
			usage: Usage{PromptTokens: 10, CompletionTokens: 1, TotalTokens: 11},
		},
		fakeResponse{
			tokens:       []string{"done"},
			content:      "done",
			finishReason: "stop",
			usage:        Usage{PromptTokens: 5, CompletionTokens: 1, TotalTokens: 6},
		},
	)

	cfg := &config.Config{
		OpenAI: config.OpenAIConfig{APIKey: "test", Model: "gpt-test"},
		Agents: config.AgentsConfig{
			Confuse: config.AgentConfig{
				Name:         "Confuse",
				Model:        "gpt-test",
				SystemPrompt: "you are confuse",
				MaxTokens:    256,
				Tools:        []string{"external_greet"},
			},
			Chongzhi: config.AgentConfig{
				Name:         "Chongzhi",
				Model:        "gpt-test",
				SystemPrompt: "you are chongzhi",
				MaxTokens:    256,
				Tools:        []string{"xizhi_write_file"},
			},
			Liang: config.AgentConfig{
				Name:         "Liang",
				Model:        "gpt-test",
				SystemPrompt: "you are liang",
				MaxTokens:    256,
			},
		},
	}
	o, err := NewOrchestrator(client, cfg, baseReg, nil, skill.NewLoader("", nil), nil)
	require.NoError(t, err)

	hub := stream.NewHub(0)
	ctx, cancel := context.WithTimeout(context.Background(), 4*time.Second)
	defer cancel()

	err = o.Handle(ctx, t.TempDir(), "user-1", "hi", hub)
	require.NoError(t, err)
	hub.Close()

	require.True(t, toolCalled, "external MCP proxy tool should have been called")
}

func TestOrchestrator_MCPToolFiltering(t *testing.T) {
	defer goleak.VerifyNone(t)

	baseReg := tool.NewRegistry()
	require.NoError(t, baseReg.Register(&tool.ToolSpec{
		Name:        "remote_search",
		Description: "search the web",
		Execute:     func(ctx context.Context, args json.RawMessage) (any, error) { return "", nil },
	}))
	require.NoError(t, baseReg.Register(&tool.ToolSpec{
		Name:        "remote_fetch",
		Description: "fetch a url",
		Execute:     func(ctx context.Context, args json.RawMessage) (any, error) { return "", nil },
	}))

	client := newFake(fakeResponse{
		tokens:       []string{"done"},
		content:      "done",
		finishReason: "stop",
		usage:        Usage{PromptTokens: 10, CompletionTokens: 1, TotalTokens: 11},
	})

	cfg := &config.Config{
		OpenAI: config.OpenAIConfig{APIKey: "test", Model: "gpt-test"},
		Agents: config.AgentsConfig{
			Confuse: config.AgentConfig{
				Name:         "Confuse",
				Model:        "gpt-test",
				SystemPrompt: "you are confuse",
				MaxTokens:    256,
				MCP: config.AgentMCPConfig{
					Servers: []config.AgentMCPServerConfig{{
						Name:  "remote",
						Tools: []string{"remote_search"},
					}},
				},
			},
			Chongzhi: config.AgentConfig{
				Name:         "Chongzhi",
				Model:        "gpt-test",
				SystemPrompt: "you are chongzhi",
				MaxTokens:    256,
				Tools:        []string{"xizhi_write_file"},
			},
			Liang: config.AgentConfig{
				Name:         "Liang",
				Model:        "gpt-test",
				SystemPrompt: "you are liang",
				MaxTokens:    256,
			},
		},
	}
	serverTools := map[string][]string{"remote": {"remote_search", "remote_fetch"}}
	o, err := NewOrchestrator(client, cfg, baseReg, serverTools, skill.NewLoader("", nil), nil)
	require.NoError(t, err)

	hub := stream.NewHub(0)
	ctx, cancel := context.WithTimeout(context.Background(), 4*time.Second)
	defer cancel()

	err = o.Handle(ctx, t.TempDir(), "user-1", "hi", hub)
	require.NoError(t, err)
	hub.Close()

	req := client.lastRequest()
	require.NotNil(t, req.Tools)
	var tools []struct {
		Function struct {
			Name string `json:"name"`
		} `json:"function"`
	}
	require.NoError(t, json.Unmarshal(req.Tools, &tools))
	names := make(map[string]bool, len(tools))
	for _, t2 := range tools {
		names[t2.Function.Name] = true
	}
	assert.True(t, names["remote_search"], "allowed tool must be present")
	assert.False(t, names["remote_fetch"], "disallowed tool must be absent")
	assert.False(t, names["remote_fetch"], "disallowed tool must be absent")

	// System prompt should mention allowed MCP tool but not the disallowed one.
	prompt := req.Messages[0].Content
	assert.Contains(t, prompt, "remote_search")
	assert.NotContains(t, prompt, "remote_fetch")
}

func TestOrchestrator_SystemPromptIncludesSkills(t *testing.T) {
	defer goleak.VerifyNone(t)

	skillDir := t.TempDir()
	require.NoError(t, os.MkdirAll(filepath.Join(skillDir, "coding-style"), 0o755))
	require.NoError(t, os.WriteFile(filepath.Join(skillDir, "coding-style", "SKILL.md"), []byte("---\nname: coding-style\ndescription: Global coding conventions\n---\n# Coding Style\n"), 0o644))

	client := newFake(fakeResponse{
		tokens:       []string{"done"},
		content:      "done",
		finishReason: "stop",
		usage:        Usage{PromptTokens: 10, CompletionTokens: 1, TotalTokens: 11},
	})

	cfg := &config.Config{
		OpenAI: config.OpenAIConfig{APIKey: "test", Model: "gpt-test"},
		Agents: config.AgentsConfig{
			Confuse: config.AgentConfig{
				Name:         "Confuse",
				Model:        "gpt-test",
				SystemPrompt: "you are confuse",
				MaxTokens:    256,
				Skills:       []string{"coding-style"},
			},
			Chongzhi: config.AgentConfig{
				Name:         "Chongzhi",
				Model:        "gpt-test",
				SystemPrompt: "you are chongzhi",
				MaxTokens:    256,
			},
			Liang: config.AgentConfig{
				Name:         "Liang",
				Model:        "gpt-test",
				SystemPrompt: "you are liang",
				MaxTokens:    256,
			},
		},
	}
	baseReg := tool.NewRegistry()
	loader := skill.NewLoader(skillDir, nil)
	require.NoError(t, skill.RegisterReadSkill(baseReg, loader))
	o, err := NewOrchestrator(client, cfg, baseReg, nil, loader, nil)
	require.NoError(t, err)

	hub := stream.NewHub(0)
	ctx, cancel := context.WithTimeout(context.Background(), 4*time.Second)
	defer cancel()

	err = o.Handle(ctx, t.TempDir(), "user-1", "hi", hub)
	require.NoError(t, err)
	hub.Close()

	req := client.lastRequest()
	prompt := req.Messages[0].Content
	assert.Contains(t, prompt, "coding-style")
	assert.Contains(t, prompt, "Global coding conventions")
	assert.Contains(t, prompt, "read_skill")
}
