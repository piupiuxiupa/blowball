package service

import (
	"context"
	"fmt"
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

// const titleSystemPrompt = "Generate a short title (max 20 chars, no quotes) summarizing this conversation. Reply with the title only."
const text = `Create a concise title for an AI coding-assistant session from the supplied human messages.
Return only the title on one line, in plain text of natural language, with no quotes,
prefix, explanation, Markdown, XML, or terminal control codes. No code is allowed.
Use the language of the messages.
Aim for about %d words in non-CJK languages or %d CJK characters.`

var titleSystemPrompt = fmt.Sprintf(text, maxTitleRunes, maxTitleRunes)

// TitleService generates a short session title asynchronously from the
// session's first user message plus the message that triggered the generation
// (title-generation-cadence: the trigger fires at send time, every 3rd user
// message starting with the 1st). Generation is fire-and-forget: callers run
// it in a goroutine. Any failure (LLM error, network, parse) degrades to the
// first 20 runes of the triggering user message so the session-list UI always
// has something.
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
//
// firstUserMsg is the session's first user message (the topic anchor) and
// currentUserMsg the message that triggered this generation (the latest-topic
// signal); on the first message (n=1) both carry the same text. Assistant
// content is deliberately NOT part of the input (title-generation-cadence:
// generation fires at send time, before any reply exists).
func (s *TitleService) GenerateTitle(ctx context.Context, sessionID, firstUserMsg, currentUserMsg string) {
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
	// stay correlated; session_id + agent name are attached so the raw-capture
	// sink attributes the title LLM call to the right session and agent.
	bg := context.Background()
	if tid := trace.FromContext(ctx); tid != "" {
		bg = trace.WithContext(bg, tid)
	}
	bg = agent.WithSessionID(bg, sessionID)
	bg = agent.WithAgentName(bg, "title")
	s.generate(bg, sessionID, firstUserMsg, currentUserMsg)
}

// generate runs the LLM call, computes the final title (LLM output or
// fallback), and upserts the row.
func (s *TitleService) generate(ctx context.Context, sessionID, firstUserMsg, currentUserMsg string) {
	tid := trace.FromContext(ctx)
	log := logger.L().With(zap.String("op", "title.generate"), zap.String("session_id", sessionID))
	if tid != "" {
		log = log.With(zap.String("trace_id", tid))
	}

	// If the user has manually set the title, do not overwrite it. This early
	// exit only trims the LLM cost — correctness lives in the upsert's SQL
	// manual-guard (a manual title set while this call is in flight survives
	// the upsert below unchanged).
	existing, err := s.mysql.GetTitle(ctx, sessionID)
	if err != nil {
		log.Error("get title failed; proceeding with generation", zap.Error(err))
	} else if existing != nil && existing.IsManual {
		log.Info("title is manual; skipping AI generation")
		return
	}

	title := s.callLLM(ctx, log, firstUserMsg, currentUserMsg)
	title = sanitizeTitle(title, currentUserMsg)

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

// callLLM invokes the LLM with the title-generation prompt. The input is the
// two user-side anchors (first question + latest question), never assistant
// content. On any error or an empty response, an empty string is returned so
// the caller can apply the fallback.
func (s *TitleService) callLLM(ctx context.Context, log *zap.Logger, firstUserMsg, currentUserMsg string) string {
	if s.llm == nil {
		log.Warn("llm client nil; falling back")
		return ""
	}

	// TitleModel is resolved at wiring time: openai.title_model when set,
	// else the default catalog entry (Config.TitleModelName). The title call
	// always runs on the non-thinking wire family — no reasoning_effort, plain
	// max_tokens.
	modelName := s.cfg.TitleModel
	if modelName == "" {
		log.Warn("llm model empty; falling back")
		return ""
	}

	req := agent.LLMRequest{
		Model: modelName,
		Messages: []agent.Message{
			{Role: "system", Content: titleSystemPrompt},
			{Role: "user", Content: "Generate the session title from the following human messages: \n\nFirst question: " + firstUserMsg + "\n\nLatest question: " + currentUserMsg},
		},
	}

	resp, err := s.llm.StreamChat(ctx, req, nil, nil)
	if err != nil {
		log.Warn("llm stream chat failed; falling back", zap.Error(err))
		return ""
	}
	return resp.Content
}

// SetManualTitle sanitizes and persists a user-edited session title. It marks
// the title as manual so subsequent AI title generation will not overwrite it,
// and touches the session's update_time so the session appears first in the
// list. The sanitized title is returned so the handler can echo it back.
func (s *TitleService) SetManualTitle(ctx context.Context, sessionID string, title string) (string, error) {
	title = sanitizeTitle(title, title)

	tid := trace.FromContext(ctx)
	if err := s.mysql.UpsertTitleManual(ctx, model.Title{
		SessionID: sessionID,
		Title:     title,
		TraceID:   tid,
		IsManual:  true,
	}); err != nil {
		return "", err
	}
	return title, s.mysql.UpdateSessionTime(ctx, sessionID)
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
		t = strings.TrimSpace(userMsg)
	}
	if utf8.RuneCountInString(t) <= maxTitleRunes {
		return t
	}
	runes := []rune(t)
	return string(runes[:maxTitleRunes])
}
