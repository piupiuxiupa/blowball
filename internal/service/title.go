package service

import (
	"context"
	"strings"
	"unicode/utf8"

	"go.uber.org/zap"

	"github.com/lush/blowball/internal/agent"
	"github.com/lush/blowball/internal/config"
	"github.com/lush/blowball/internal/model"
	"github.com/lush/blowball/internal/pkg/logger"
	"github.com/lush/blowball/internal/pkg/trace"
)

// maxTitleRunes is the spec-mandated upper bound on generated titles. The LLM
// is asked to stay under it but we enforce it defensively on the way out.
const maxTitleRunes = 20

// titleSystemPrompt is the tiny instruction we prepend to the Q&A exchange.
// Asking for "title only" without quotes keeps the LLM from wrapping the
// answer in punctuation that would leak into the stored value.
const titleSystemPrompt = "Generate a short title (max 20 chars, no quotes) summarizing this conversation. Reply with the title only."

// TitleService generates a short session title asynchronously from the first
// user/assistant exchange. Generation is fire-and-forget: callers run it in a
// goroutine. Any failure (LLM error, network, parse) degrades to the first 20
// runes of the user message so the session-list UI always has something.
type TitleService struct {
	llm   agent.LLMClient
	mysql MySQLStore
	cfg   config.OpenAIConfig
}

// NewTitleService wires TitleService with its LLM client, MySQL store and the
// OpenAI model/parameters used for the title-generation call.
func NewTitleService(llm agent.LLMClient, mysqlStore MySQLStore, cfg config.OpenAIConfig) *TitleService {
	return &TitleService{llm: llm, mysql: mysqlStore, cfg: cfg}
}

// GenerateTitle is intended to be called from a goroutine (go svc.GenerateTitle(...)).
// It MUST NOT panic the host process under any failure mode: a top-level
// recover converts any panic into a logged error.
func (s *TitleService) GenerateTitle(ctx context.Context, sessionID, userMsg, assistantMsg string) {
	defer func() {
		if r := recover(); r != nil {
			logger.L().Error("title.generate panicked",
				zap.String("op", "title.generate"),
				zap.String("session_id", sessionID),
				zap.Any("panic", r),
			)
		}
	}()

	// Use a fresh context derived from background so a cancelled HTTP request
	// does not abort title generation. The trace_id is copied across so logs
	// stay correlated.
	bg := context.Background()
	if tid := trace.FromContext(ctx); tid != "" {
		bg = trace.WithContext(bg, tid)
	}
	s.generate(bg, sessionID, userMsg, assistantMsg)
}

// generate runs the LLM call, computes the final title (LLM output or
// fallback), and upserts the row.
func (s *TitleService) generate(ctx context.Context, sessionID, userMsg, assistantMsg string) {
	tid := trace.FromContext(ctx)
	log := logger.L().With(zap.String("op", "title.generate"), zap.String("session_id", sessionID))
	if tid != "" {
		log = log.With(zap.String("trace_id", tid))
	}

	title := s.callLLM(ctx, log, userMsg, assistantMsg)
	title = sanitizeTitle(title, userMsg)

	if err := s.mysql.UpsertTitle(ctx, model.Title{
		SessionID: sessionID,
		Title:     title,
		TraceID:   tid,
	}); err != nil {
		log.Error("upsert title failed", zap.String("title", title), zap.Error(err))
		return
	}
	log.Info("title generated", zap.String("title", title))
}

// callLLM invokes the LLM with the title-generation prompt. On any error or an
// empty response, an empty string is returned so the caller can apply the
// fallback.
func (s *TitleService) callLLM(ctx context.Context, log *zap.Logger, userMsg, assistantMsg string) string {
	if s.llm == nil {
		log.Warn("llm client nil; falling back")
		return ""
	}

	modelName := s.cfg.Model
	if modelName == "" {
		log.Warn("llm model empty; falling back")
		return ""
	}

	req := agent.LLMRequest{
		Model: modelName,
		Messages: []agent.Message{
			{Role: "system", Content: titleSystemPrompt},
			{Role: "user", Content: "User: " + userMsg + "\n\nAssistant: " + assistantMsg},
		},
	}

	resp, err := s.llm.StreamChat(ctx, req, nil, nil)
	if err != nil {
		log.Warn("llm stream chat failed; falling back", zap.Error(err))
		return ""
	}
	return resp.Content
}

// sanitizeTitle trims surrounding whitespace/quotes and truncates defensively
// to maxTitleRunes runes. If the LLM output (post-trim) is empty, the first
// maxTitleRunes runes of userMsg are used as the fallback per the spec's
// "Title generation failure" scenario.
func sanitizeTitle(raw, userMsg string) string {
	t := strings.TrimSpace(raw)
	t = strings.Trim(t, `"'`+"`")
	t = strings.TrimSpace(t)
	if t == "" {
		t = userMsg
	}
	if utf8.RuneCountInString(t) <= maxTitleRunes {
		return t
	}
	runes := []rune(t)
	return string(runes[:maxTitleRunes])
}
