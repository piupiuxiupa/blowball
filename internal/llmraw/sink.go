package llmraw

import (
	"context"
	"encoding/json"
	"time"

	"go.uber.org/zap"

	"github.com/lush/blowball/internal/agent"
	"github.com/lush/blowball/internal/model"
	"github.com/lush/blowball/internal/pkg/logger"
)

// pushTimeout bounds a single buffer push. The sink is invoked on the LLM
// streaming hot path, so a dead Redis must not stall a turn by the client's
// multi-second dial timeout — anything slower than this is dropped.
const pushTimeout = 500 * time.Millisecond

// Sink is the production agent.RawCaptureSink: it serialises each capture
// record onto the Redis write-behind buffer and nudges the flusher so a burst
// crosses the batch threshold without waiting for the interval ticker. It
// implements Capture in the sink's own bounded context (request cancellation
// is deliberately ignored — a client disconnect should not drop the tail of a
// turn's capture).
type Sink struct {
	buf    Buffer
	notify chan struct{}
}

// NewSink builds a Sink over buf. notify (optional) is the wakeup channel
// shared with the flusher; when non-nil the sink sends a non-blocking signal
// after each successful push. A nil channel simply disables the nudge.
func NewSink(buf Buffer, notify chan struct{}) *Sink {
	return &Sink{buf: buf, notify: notify}
}

// Notify exposes the wakeup channel so the wiring can hand the same channel
// to NewFlusher. Sends on it are the sink's responsibility.
func (s *Sink) Notify() chan struct{} { return s.notify }

// Capture implements agent.RawCaptureSink. It never returns an error and
// never panics the turn: marshal and push failures are logged and the record
// dropped, per the spec's best-effort write-behind requirement. It runs once
// per SSE frame on the streaming hot path, so the happy path allocates only
// the record JSON — logger fields are built solely on failure.
func (s *Sink) Capture(ctx context.Context, rec agent.RawCaptureRecord) {
	// The push must survive request cancellation (a disconnecting client
	// should still leave the turn's raw log intact) but stay time-bounded.
	pushCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), pushTimeout)
	defer cancel()

	raw, err := json.Marshal(toModel(rec))
	if err != nil {
		logger.L().Warn("marshal raw capture record failed; dropping",
			zap.String("op", "llmraw.capture"),
			zap.String("call_id", rec.CallID),
			zap.String("kind", rec.Kind),
			zap.Error(err))
		return
	}
	if err := s.buf.PushRawLog(pushCtx, raw); err != nil {
		logger.L().Warn("raw capture buffer push failed; dropping",
			zap.String("op", "llmraw.capture"),
			zap.String("call_id", rec.CallID),
			zap.String("kind", rec.Kind),
			zap.Int("seq", rec.Seq),
			zap.Int("raw_bytes", len(raw)),
			zap.Error(err))
		return
	}

	// Wake the flusher (non-blocking; a full channel means a wakeup is
	// already pending).
	if s.notify != nil {
		select {
		case s.notify <- struct{}{}:
		default:
		}
	}
}

// toModel converts the agent-package capture record into the persistence
// model. The two types are kept separate so the agent package does not import
// internal/model.
func toModel(rec agent.RawCaptureRecord) model.LLMRawLog {
	return model.LLMRawLog{
		CallID:       rec.CallID,
		Seq:          rec.Seq,
		FrameIndex:   rec.FrameIndex,
		TraceID:      rec.TraceID,
		SessionID:    rec.SessionID,
		UserID:       rec.UserID,
		Agent:        rec.Agent,
		Kind:         rec.Kind,
		Model:        rec.Model,
		FinishReason: rec.FinishReason,
		HTTPStatus:   rec.HTTPStatus,
		DurationMS:   rec.DurationMS,
		Raw:          rec.Raw,
		MsgTime:      rec.MsgTime,
	}
}
