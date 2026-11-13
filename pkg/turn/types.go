package turn

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"time"

	"github.com/lush/blowball/pkg/agent"
	"github.com/lush/blowball/pkg/artifact"
	"github.com/lush/blowball/pkg/config"
	"github.com/lush/blowball/pkg/memory"
	"github.com/lush/blowball/pkg/model"
	"github.com/lush/blowball/pkg/run"
	"github.com/lush/blowball/pkg/service"
	"github.com/lush/blowball/pkg/stream"
)

// Selection is the fully resolved model configuration for one turn.
type Selection struct {
	Model               string
	Thinking            bool
	Vision              bool
	Effort              string
	MaxContextTokens    int
	MaxCompletionTokens int
	LengthContinue      config.LengthContinueConfig
}

func (s Selection) Override() agent.ModelOverride {
	return agent.ModelOverride{
		Model:               s.Model,
		Thinking:            s.Thinking,
		ReasoningEffort:     s.Effort,
		MaxContextTokens:    s.MaxContextTokens,
		MaxCompletionTokens: s.MaxCompletionTokens,
		LengthContinue:      s.LengthContinue,
	}
}

// Request is the transport-independent, start-ready form of one turn. RawToken
// is used only through the in-memory context and is never serialized.
type Request struct {
	SessionID        string
	UserID           string
	RunID            string
	Content          string
	Attachments      []model.MessageAttachmentRef
	Selection        Selection
	RawToken         string
	UserMsgTime      time.Time
	ContextCompacted bool
	MetaCreatedAt    time.Time
	FromQueue        bool
	// Source selects the conversation table group. Empty means interactive.
	// Scheduled turns require the scheduled services in Dependencies; there is
	// no fallback to the ordinary tables when they are missing.
	Source model.SessionSource
	// LLMContextPrefix is optional runtime-only context prepended to the
	// CURRENT turn's user message for the model. It is never persisted, never
	// included in titles or memory capture, and never part of the stored
	// Task.prompt. Scheduled execution uses it for the pinned resource
	// manifest pointer.
	LLMContextPrefix string

	// BeforeStart is the background startup fence. It runs after read-only
	// preamble work and before title generation or the first execution write.
	BeforeStart func(context.Context) error
	// OnTerminal runs after terminal message/usage persistence and before the
	// session claim is released.
	OnTerminal func(context.Context, TerminalResult)
}

// Result is the completed orchestration result reported over Started.ResultCh.
type Result struct {
	Events []stream.StreamEvent
	Usage  map[string]any
	Err    error
}

// TerminalResult is the durable terminal notification for background owners.
type TerminalResult struct {
	RunID     string
	SessionID string
	UserID    string
	Status    string
	// MessagesDurable reports whether the terminal message/tool batch (and the
	// user row it may carry) was written successfully within the bounded retry
	// budget. Scheduled execution owners use it to refuse a terminal receipt
	// that would claim completion without durable content.
	MessagesDurable bool
	Events          []stream.StreamEvent
	Usage           map[string]any
	Err             error
}

// Started is a running turn's completion surface; no SSE subscriber is needed.
type Started struct {
	ResultCh <-chan Result
	TurnDone <-chan struct{}
}

var (
	ErrShutdownDuringStartup = errors.New("shutdown during turn startup")
	ErrSessionBusy           = errors.New("session busy")
	ErrAdmissionUnavailable  = errors.New("background admission unavailable")
)

// SubmitOutcome describes whether a headless submission crossed the startup
// boundary. Only SubmitStarted may have invoked the agent.
type SubmitOutcome string

const (
	SubmitStarted         SubmitOutcome = "started"
	SubmitSessionBusy     SubmitOutcome = "session_busy"
	SubmitAdmissionFailed SubmitOutcome = "admission_failed"
	SubmitRejected        SubmitOutcome = "rejected"
	SubmitShutdown        SubmitOutcome = "shutdown"
)

// Submission is the explicit result of a fail-closed background submission.
type Submission struct {
	Started *Started
	Outcome SubmitOutcome
	Err     error
}

// Dependencies are the shared services needed by the full turn pipeline.
type Dependencies struct {
	Sessions      *service.SessionService
	Messages      *service.MessageService
	Titles        *service.TitleService
	Compaction    *service.CompactionService
	Memory        *memory.Service
	Orchestrator  OrchestratorRunner
	DataDir       string
	Runs          *run.Manager
	Artifacts     *artifact.Service
	RuntimeStatus RuntimeStatusStore
	Shutdown      *ShutdownGate
	// Scheduled carries the scheduled-source services. Nil keeps the
	// interactive-only behavior; a scheduled request then fails closed.
	Scheduled *ScheduledServices
}

// ScheduledServices is the scheduled-source view of the turn's persistence
// services. Every field is bound to the scheduled table group; a scheduled
// turn never falls back to the interactive services.
type ScheduledServices struct {
	Sessions   *service.SessionService
	Messages   *service.MessageService
	Titles     *service.TitleService
	Compaction *service.CompactionService
	// Snapshots is the scheduled sub-agent transcript store injected into the
	// orchestrator per turn (agent.WithSubAgentSnapshotStore).
	Snapshots agent.SubAgentSnapshotStore
}

// Executor owns the transport-independent turn lifecycle.
type Executor struct {
	sessSvc       *service.SessionService
	msgSvc        *service.MessageService
	titleSvc      *service.TitleService
	compSvc       *service.CompactionService
	memSvc        *memory.Service
	orch          OrchestratorRunner
	runs          *run.Manager
	dataDir       string
	artifacts     *artifact.Service
	runtimeStatus RuntimeStatusStore
	newHub        func() *stream.Hub
	shutdown      *ShutdownGate
	queuePump     func(string)
	scheduled     *ScheduledServices
	snapshots     agent.SubAgentSnapshotStore
}

func NewExecutor(deps Dependencies) *Executor {
	gate := deps.Shutdown
	if gate == nil {
		gate = NewShutdownGate()
	}
	return &Executor{
		sessSvc: deps.Sessions, msgSvc: deps.Messages, titleSvc: deps.Titles,
		compSvc: deps.Compaction, memSvc: deps.Memory, orch: deps.Orchestrator,
		runs: deps.Runs, dataDir: deps.DataDir, artifacts: deps.Artifacts,
		runtimeStatus: deps.RuntimeStatus,
		newHub:        func() *stream.Hub { return stream.NewHub(stream.DefaultHubBufferSize) },
		shutdown:      gate,
		scheduled:     deps.Scheduled,
	}
}

// forSource returns the executor view whose persistence services are bound to
// the requested source. The interactive view is the executor itself; the
// scheduled view requires explicitly configured scheduled services and fails
// closed otherwise (never a silent interactive fallback).
func (e *Executor) forSource(source model.SessionSource) (*Executor, error) {
	switch source.Normalized() {
	case model.SessionSourceInteractive, model.SessionSourceLegacyInteractive:
		return e, nil
	case model.SessionSourceScheduled:
		s := e.scheduled
		if s == nil || s.Sessions == nil || s.Messages == nil || s.Compaction == nil {
			return nil, fmt.Errorf("turn: scheduled services are not configured")
		}
		clone := *e
		clone.sessSvc = s.Sessions
		clone.msgSvc = s.Messages
		clone.titleSvc = s.Titles
		clone.compSvc = s.Compaction
		clone.snapshots = s.Snapshots
		return &clone, nil
	default:
		return nil, fmt.Errorf("turn: unknown session source %q", source)
	}
}

func (e *Executor) SetArtifactService(s *artifact.Service) { e.artifacts = s }
func (e *Executor) SetQueuePump(pump func(string))         { e.queuePump = pump }

// WithServices returns a per-start view with replaceable runtime services.
// The shared executor remains immutable while concurrent turns are running.
func (e *Executor) WithServices(
	sessSvc *service.SessionService,
	msgSvc *service.MessageService,
	titleSvc *service.TitleService,
	compSvc *service.CompactionService,
	memSvc *memory.Service,
	orch OrchestratorRunner,
) *Executor {
	clone := *e
	clone.sessSvc = sessSvc
	clone.msgSvc = msgSvc
	clone.titleSvc = titleSvc
	clone.compSvc = compSvc
	clone.memSvc = memSvc
	clone.orch = orch
	return &clone
}

func (e *Executor) StopNewTurns()                    { e.shutdown.Stop() }
func (e *Executor) ShutdownContext() context.Context { return e.shutdown.Context() }
func (e *Executor) newTurnsStopped() bool            { return e.shutdown.Stopped() }

// Submit admits and starts a background turn without the interactive Redis
// failure degradation. A rejected start releases its temporary claim.
func (e *Executor) Submit(ctx context.Context, req *Request) Submission {
	if e.newTurnsStopped() {
		return Submission{Outcome: SubmitShutdown, Err: ErrShutdownDuringStartup}
	}
	holder, claimed, err := e.runs.Store.ClaimBackgroundSession(ctx, req.SessionID, req.RunID)
	if err != nil {
		return Submission{Outcome: SubmitAdmissionFailed, Err: fmt.Errorf("%w: %v", ErrAdmissionUnavailable, err)}
	}
	if !claimed {
		return Submission{Outcome: SubmitSessionBusy, Err: fmt.Errorf("%w: %s", ErrSessionBusy, holder)}
	}
	started, err := e.Start(ctx, req)
	if err != nil {
		releaseCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		_ = e.runs.Store.ReleaseSession(releaseCtx, req.SessionID, req.RunID)
		cancel()
		outcome := SubmitRejected
		if errors.Is(err, ErrShutdownDuringStartup) {
			outcome = SubmitShutdown
		}
		return Submission{Outcome: outcome, Err: err}
	}
	return Submission{Started: started, Outcome: SubmitStarted}
}

// ShutdownGate stops new queue/background starts and cancels preparations.
type ShutdownGate struct {
	stop   chan struct{}
	once   sync.Once
	ctx    context.Context
	cancel context.CancelFunc
}

func NewShutdownGate() *ShutdownGate {
	ctx, cancel := context.WithCancel(context.Background())
	return &ShutdownGate{stop: make(chan struct{}), ctx: ctx, cancel: cancel}
}

func (g *ShutdownGate) Stop() {
	g.once.Do(func() {
		close(g.stop)
		g.cancel()
	})
}

func (g *ShutdownGate) Stopped() bool {
	select {
	case <-g.stop:
		return true
	default:
		return false
	}
}

func (g *ShutdownGate) Context() context.Context { return g.ctx }
