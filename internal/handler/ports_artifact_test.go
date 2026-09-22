package handler

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/lush/blowball/internal/agent"
	"github.com/lush/blowball/internal/stream"
)

// fakeInnerRunner is a canned innerRunner for adapter tests: it emits the
// scripted events then a done event, mimicking the orchestrator's terminal
// sequence.
type fakeInnerRunner struct {
	emit []stream.StreamEvent
}

func (f *fakeInnerRunner) Handle(_ context.Context, _, _, _ string, _ []agent.Message, hub *stream.Hub, _ agent.RoundHook, _ agent.ModelOverride) error {
	for _, e := range f.emit {
		hub.Send(e)
	}
	hub.Send(stream.DoneEvent(map[string]any{"total": map[string]any{"total_tokens": 1}}))
	return nil
}

// collectHub drains a hub until it closes and returns the received events.
func collectHub(h *stream.Hub) []stream.StreamEvent {
	var got []stream.StreamEvent
	events := h.Events()
	done := h.Done()
	for {
		select {
		case e := <-events:
			got = append(got, e)
		case <-done:
			for {
				select {
				case e := <-events:
					got = append(got, e)
				default:
					return got
				}
			}
		}
	}
}

func TestAdapter_EmitsArtifactEventsBeforeDone(t *testing.T) {
	inner := &fakeInnerRunner{emit: []stream.StreamEvent{
		stream.TokenEvent(stream.AgentConfucius, "see "),
		stream.TokenEvent(stream.AgentConfucius, "[a](blowball://workspace/a.md)"),
	}}
	adapter := &orchestratorAdapter{inner: inner}

	hub := stream.NewHub(stream.DefaultHubBufferSize)
	received := make(chan []stream.StreamEvent, 1)
	go func() { received <- collectHub(hub) }()

	hooks := TurnHooks{
		FinalizeTurn: func(ctx context.Context) []stream.ArtifactInfo {
			return []stream.ArtifactInfo{
				{Path: "a.md", VersionID: "v-1", Size: 3, Mime: "text/markdown", Op: "create"},
			}
		},
	}
	events, usage, err := adapter.Handle(context.Background(), "/ws", "/skills", "u1", nil, hub, hooks, agent.ModelOverride{})
	hub.Close()
	require.NoError(t, err)
	require.NotNil(t, usage)

	got := <-received
	require.Len(t, got, 4) // 2 tokens + artifact + done

	// Order: tokens, artifact, done.
	assert.Equal(t, stream.EventToken, got[0].Type)
	assert.Equal(t, stream.EventToken, got[1].Type)
	assert.Equal(t, stream.EventArtifact, got[2].Type)
	assert.Equal(t, stream.EventDone, got[3].Type)

	// Artifact event content carries the pin data.
	var ai stream.ArtifactInfo
	require.NoError(t, json.Unmarshal([]byte(got[2].Content), &ai))
	assert.Equal(t, "a.md", ai.Path)
	assert.Equal(t, "v-1", ai.VersionID)
	assert.Equal(t, "create", ai.Op)

	// done carries the live summary but is NOT in the persisted slice.
	arts, ok := got[3].Meta[stream.MetaArtifacts].([]stream.ArtifactInfo)
	require.True(t, ok, "done meta must carry []stream.ArtifactInfo summary")
	require.Len(t, arts, 1)
	assert.Equal(t, "v-1", arts[0].VersionID)

	// Persisted events: tokens + artifact, no done.
	require.Len(t, events, 3)
	assert.Equal(t, stream.EventArtifact, events[2].Type)
}

func TestAdapter_NoFinalizeHookIsZeroChange(t *testing.T) {
	inner := &fakeInnerRunner{emit: []stream.StreamEvent{
		stream.TokenEvent(stream.AgentConfucius, "hi"),
	}}
	adapter := &orchestratorAdapter{inner: inner}

	hub := stream.NewHub(stream.DefaultHubBufferSize)
	received := make(chan []stream.StreamEvent, 1)
	go func() { received <- collectHub(hub) }()

	events, _, err := adapter.Handle(context.Background(), "/ws", "/skills", "u1", nil, hub, TurnHooks{}, agent.ModelOverride{})
	hub.Close()
	require.NoError(t, err)

	got := <-received
	require.Len(t, got, 2) // token + done
	assert.Equal(t, stream.EventToken, got[0].Type)
	assert.Equal(t, stream.EventDone, got[1].Type)
	_, hasArtifacts := got[1].Meta[stream.MetaArtifacts]
	assert.False(t, hasArtifacts)
	require.Len(t, events, 1)
}

func TestAdapter_EmptyArtifactsStillTagDone(t *testing.T) {
	inner := &fakeInnerRunner{}
	adapter := &orchestratorAdapter{inner: inner}

	hub := stream.NewHub(stream.DefaultHubBufferSize)
	received := make(chan []stream.StreamEvent, 1)
	go func() { received <- collectHub(hub) }()

	hooks := TurnHooks{FinalizeTurn: func(ctx context.Context) []stream.ArtifactInfo { return nil }}
	events, _, err := adapter.Handle(context.Background(), "/ws", "/skills", "u1", nil, hub, hooks, agent.ModelOverride{})
	hub.Close()
	require.NoError(t, err)

	got := <-received
	require.Len(t, got, 1) // done only
	arts, ok := got[0].Meta[stream.MetaArtifacts].([]stream.ArtifactInfo)
	require.True(t, ok)
	assert.Empty(t, arts)
	assert.Empty(t, events)
}
