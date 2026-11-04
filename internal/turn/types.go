package turn

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"time"

	"github.com/lush/blowball/internal/agent"
	"github.com/lush/blowball/internal/artifact"
	"github.com/lush/blowball/internal/config"
	"github.com/lush/blowball/internal/memory"
	"github.com/lush/blowball/internal/model"
	"github.com/lush/blowball/internal/run"
	"github.com/lush/blowball/internal/service"
	"github.com/lush/blowball/internal/stream"
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
	Events    []stream.StreamEvent
	Usage     map[string]any
	Err       error
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
	Sessions     *service.SessionService
	Messages     *service.MessageService
	Titles       *service.TitleService
	Compaction   *service.CompactionService
	Memory       *memory.Service
	Orchestrator OrchestratorRunner
	DataDir      string
	Runs         *run.Manager
	Artifacts    *artifact.Service
	Shutdown     *ShutdownGate
}

// Executor owns the transport-independent turn lifecycle.
type Executor struct {
	sessSvc   *service.SessionService
	msgSvc    *service.MessageService
	titleSvc  *service.TitleService
	compSvc   *service.CompactionService
	memSvc    *memory.Service
	orch      OrchestratorRunner
	runs      *run.Manager
	dataDir   string
	artifacts *artifact.Service
	newHub    func() *stream.Hub
	shutdown  *ShutdownGate
	queuePump func(string)
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
		newHub:   func() *stream.Hub { return stream.NewHub(stream.DefaultHubBufferSize) },
		shutdown: gate,
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
