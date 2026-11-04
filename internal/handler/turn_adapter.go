package handler

import (
	"context"
	"time"

	"github.com/lush/blowball/internal/model"
	turnexec "github.com/lush/blowball/internal/turn"
)

type runResult = turnexec.Result

type startedTurn struct {
	resultCh <-chan runResult
	turnDone <-chan struct{}
	aborted  error
}

// turnRequest remains the HTTP/queue adapter's resolved request shape.
type turnRequest struct {
	sessionID        string
	userID           string
	traceID          string
	content          string
	attachments      []model.MessageAttachmentRef
	sel              modelSelection
	rawToken         string
	userMsgTime      time.Time
	contextCompacted bool
	metaCreatedAt    time.Time
	fromQueue        bool
}

var errShutdownDuringStartup = turnexec.ErrShutdownDuringStartup

func (h *MessageStreamHandler) startTurn(ctx context.Context, req *turnRequest) *startedTurn {
	executor := h.turns.WithServices(h.sessSvc, h.msgSvc, h.titleSvc, h.compSvc, h.memSvc, h.orch)
	started, err := executor.Start(ctx, &turnexec.Request{
		SessionID:        req.sessionID,
		UserID:           req.userID,
		RunID:            req.traceID,
		Content:          req.content,
		Attachments:      req.attachments,
		Selection:        req.sel.turnSelection(),
		RawToken:         req.rawToken,
		UserMsgTime:      req.userMsgTime,
		ContextCompacted: req.contextCompacted,
		MetaCreatedAt:    req.metaCreatedAt,
		FromQueue:        req.fromQueue,
	})
	if err != nil {
		return &startedTurn{aborted: err}
	}
	return &startedTurn{resultCh: started.ResultCh, turnDone: started.TurnDone}
}

func (s modelSelection) turnSelection() turnexec.Selection {
	return turnexec.Selection{
		Model:               s.Model,
		Thinking:            s.Thinking,
		Vision:              s.Vision,
		Effort:              s.Effort,
		MaxContextTokens:    s.MaxContextTokens,
		MaxCompletionTokens: s.MaxCompletionTokens,
		LengthContinue:      s.LengthContinue,
	}
}
