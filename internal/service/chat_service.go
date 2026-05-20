package service

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"strings"
	"time"

	"github.com/UNagent-1D/conversation-chat/internal/apperrors"
	"github.com/UNagent-1D/conversation-chat/internal/channel"
	"github.com/UNagent-1D/conversation-chat/internal/clients/llm"
	"github.com/UNagent-1D/conversation-chat/internal/domain"
	"github.com/UNagent-1D/conversation-chat/internal/repository"
)

// TurnRequest is the body for POST /sessions/:sid/turns.
type TurnRequest struct {
	UserMessage string `json:"user_message"`
	MessageID   string `json:"message_id"`
	ChannelKey  string `json:"channel_key"`
}

// TurnResponse is returned by ProcessTurn.
type TurnResponse struct {
	SessionID string `json:"session_id"`
	Message   struct {
		Text string `json:"text"`
	} `json:"message"`
}

// User-facing messages for sessions that can no longer run the bot loop.
// They tell the user how to recover (the Telegram side handles /start).
const (
	sessionExpiredMsg = "Tu sesión expiró por inactividad. Envía /start para comenzar de nuevo."
	sessionClosedMsg  = "Esta conversación finalizó. Envía /start para comenzar una nueva."
	noOperatorMsg     = "Por ahora no hay un operador disponible. Puedes seguir conversando conmigo y con gusto te ayudo."
)

// textResponse builds a TurnResponse carrying a single text message.
// An empty text is a valid, intentional "stay silent" response.
func textResponse(sessionID, text string) *TurnResponse {
	r := &TurnResponse{SessionID: sessionID}
	r.Message.Text = text
	return r
}

// ChatService owns the per-turn LLM loop, tool execution, and escalation state machine.
type ChatService struct {
	redis      *repository.RedisRepo
	sessions   *repository.SessionRepo
	llmClient  llm.LLMClient
	entrypoint *EntrypointService
	logger     *slog.Logger
}

// NewChatService creates a new ChatService.
func NewChatService(
	redis *repository.RedisRepo,
	sessions *repository.SessionRepo,
	llmClient llm.LLMClient,
	entrypoint *EntrypointService,
	logger *slog.Logger,
) *ChatService {
	return &ChatService{
		redis:      redis,
		sessions:   sessions,
		llmClient:  llmClient,
		entrypoint: entrypoint,
		logger:     logger,
	}
}

// ProcessTurn runs one full LLM turn and returns the assistant message.
func (s *ChatService) ProcessTurn(ctx context.Context, sessionID string, req TurnRequest) (*TurnResponse, error) {
	// 1. Load ContextEnvelope from Redis
	env, err := s.redis.GetContext(ctx, sessionID)
	if err != nil {
		return nil, fmt.Errorf("load context: %w", err)
	}
	if env == nil {
		// The Redis context expired (idle timeout). Do not error — guide
		// the user to /start so the Telegram side can open a fresh session.
		return textResponse(sessionID, sessionExpiredMsg), nil
	}

	// 2. Load conversation history from Redis
	history, err := s.redis.GetHistory(ctx, sessionID)
	if err != nil {
		return nil, fmt.Errorf("load history: %w", err)
	}

	// 3. Check session state
	state, err := s.redis.GetState(ctx, sessionID)
	if err != nil {
		return nil, fmt.Errorf("load state: %w", err)
	}

	ttl := time.Duration(env.SessionMeta.IdleTimeoutSeconds+60) * time.Second

	switch state {
	case domain.StateClosed:
		// Session is over. Guide the user to /start instead of erroring.
		return textResponse(sessionID, sessionClosedMsg), nil
	case domain.StateOperatorActive:
		// A human operator owns the conversation. Save the user's message
		// so the operator sees it, and stay silent — the operator replies.
		s.appendAndFlush(ctx, env, sessionID, domain.Turn{
			Role:       domain.RoleUser,
			Content:    req.UserMessage,
			ChannelKey: req.ChannelKey,
			MessageID:  req.MessageID,
			Ts:         time.Now().UTC(),
		}, ttl)
		return textResponse(sessionID, ""), nil
	case domain.StateEscalationPending:
		// Save the user's message so an operator can read it on claim.
		s.appendAndFlush(ctx, env, sessionID, domain.Turn{
			Role:       domain.RoleUser,
			Content:    req.UserMessage,
			ChannelKey: req.ChannelKey,
			MessageID:  req.MessageID,
			Ts:         time.Now().UTC(),
		}, ttl)
		active, _ := s.redis.EscalationTTLActive(ctx, sessionID)
		if !active {
			// No operator claimed the chat in time. Tell the user once,
			// then hand the conversation back to the bot so it stays usable.
			_ = s.redis.RemoveFromOpQueue(ctx, env.TenantPolicy.TenantID, sessionID)
			_ = s.redis.SetState(ctx, sessionID, domain.StateBotActive, ttl)
			s.appendAndFlush(ctx, env, sessionID, domain.Turn{
				Role: domain.RoleAssistant, Content: noOperatorMsg, Ts: time.Now().UTC(),
			}, ttl)
			return textResponse(sessionID, noOperatorMsg), nil
		}
		// Operator handoff still pending: stay silent so the user is not
		// spammed with the same "connecting you" notice on every message.
		return textResponse(sessionID, ""), nil
	}

	// 4. Append new user turn to in-memory history
	userTurn := domain.Turn{
		Role:       domain.RoleUser,
		Content:    req.UserMessage,
		ChannelKey: req.ChannelKey,
		MessageID:  req.MessageID,
		Ts:         time.Now().UTC(),
	}
	history = append(history, userTurn)

	// 5. Run the LLM loop (may iterate for tool calls)
	assistantText, err := s.runLLMLoop(ctx, env, sessionID, history, ttl)
	if err != nil {
		return nil, err
	}

	return &TurnResponse{
		SessionID: sessionID,
		Message:   struct{ Text string `json:"text"` }{assistantText},
	}, nil
}

// runLLMLoop calls the LLM, handles tool calls (re-entering), and returns the final text.
func (s *ChatService) runLLMLoop(ctx context.Context, env *domain.ContextEnvelope, sessionID string, history []domain.Turn, ttl time.Duration) (string, error) {
	const maxToolIterations = 5

	for i := 0; i < maxToolIterations; i++ {
		llmResp, rawContent, err := s.callLLM(ctx, env, history)
		if err != nil {
			if errors.Is(err, apperrors.ErrLLMCircuitOpen) {
				// Circuit is open — provider is unavailable, fail fast with a
				// distinct message so the user knows to retry later.
				return "El servicio de IA no está disponible en este momento. Por favor intenta en unos minutos.", nil
			}
			return "Lo siento, ocurrió un error. Por favor intenta de nuevo.", nil
		}

		s.logger.Info("llm response",
			slog.String("session_id", sessionID),
			slog.String("action", llmResp.Action),
			slog.String("raw", rawContent),
		)

		switch llmResp.Action {
		case "none", "close_session":
			text := s.applyFormatRules(llmResp.Message.Text, env)
			assistantTurn := domain.Turn{Role: domain.RoleAssistant, Content: text, Ts: time.Now().UTC()}
			s.appendAndFlush(ctx, env, sessionID, assistantTurn, ttl)

			if llmResp.Action == "close_session" {
				go func() {
					bgCtx := context.Background()
					_ = s.entrypoint.CloseSession(bgCtx, env.TenantPolicy.TenantID, sessionID)
				}()
			}
			return text, nil

		case "tool_call":
			text := s.applyFormatRules(llmResp.Message.Text, env)
			// Show user the "working on it" message
			assistantTurn := domain.Turn{Role: domain.RoleAssistant, Content: text, Ts: time.Now().UTC()}
			s.appendAndFlush(ctx, env, sessionID, assistantTurn, ttl)
			history = append(history, assistantTurn)

			// Execute the tool
			toolResult, err := s.executeTool(ctx, env, llmResp.Message.Tool)
			toolTurn := domain.Turn{
				Role:     domain.RoleTool,
				ToolName: llmResp.Message.Tool.ToolName,
				Result:   toolResult,
				Ts:       time.Now().UTC(),
			}
			if err != nil {
				s.logger.Warn("tool execution failed",
					slog.String("session_id", sessionID),
					slog.String("tool", llmResp.Message.Tool.ToolName),
					slog.String("error", err.Error()),
				)
				errBytes, _ := json.Marshal(map[string]string{"error": err.Error()})
				toolTurn.Result = errBytes
			}
			s.appendAndFlush(ctx, env, sessionID, toolTurn, ttl)
			history = append(history, toolTurn)
			// Loop back to call LLM again with tool result in context

		case "escalate":
			text := s.applyFormatRules(llmResp.Message.Text, env)
			s.handleEscalation(ctx, env, sessionID, llmResp.Message.Escalation, ttl)
			assistantTurn := domain.Turn{Role: domain.RoleAssistant, Content: text, Ts: time.Now().UTC()}
			s.appendAndFlush(ctx, env, sessionID, assistantTurn, ttl)
			return text, nil
		}
	}

	return "Lo siento, no pude completar la operación. Por favor intenta de nuevo.", nil
}

// callLLM sends one completion request and validates the response. Retries once on parse error.
func (s *ChatService) callLLM(ctx context.Context, env *domain.ContextEnvelope, history []domain.Turn) (domain.LLMResponse, string, error) {
	req := llm.CompletionRequest{
		Model:        env.AgentRuntime.Model,
		Temperature:  env.AgentRuntime.Temperature,
		MaxTokens:    env.AgentRuntime.MaxTokens,
		SystemPrompt: env.AgentRuntime.SystemPrompt,
		Messages:     history,
	}

	resp, err := s.llmClient.Complete(ctx, req)
	if err != nil {
		return domain.LLMResponse{}, "", fmt.Errorf("llm call: %w", err)
	}

	if err := resp.Response.Validate(); err != nil {
		// Retry once with a correction instruction
		correctionHistory := append(history, domain.Turn{
			Role:    domain.RoleAssistant,
			Content: resp.RawContent,
			Ts:      time.Now().UTC(),
		}, domain.Turn{
			Role:    domain.RoleUser,
			Content: "Your previous response was invalid. Please correct it: " + err.Error(),
			Ts:      time.Now().UTC(),
		})
		req.Messages = correctionHistory
		retryResp, retryErr := s.llmClient.Complete(ctx, req)
		if retryErr != nil {
			return domain.LLMResponse{}, "", fmt.Errorf("llm retry: %w", retryErr)
		}
		if err2 := retryResp.Response.Validate(); err2 != nil {
			return domain.LLMResponse{}, retryResp.RawContent, fmt.Errorf("llm response invalid after retry: %w", err2)
		}
		return retryResp.Response, retryResp.RawContent, nil
	}

	// Validate tool_name is in allowed permissions
	if resp.Response.Action == "tool_call" && resp.Response.Message.Tool != nil {
		if !s.isToolAllowed(resp.Response.Message.Tool.ToolName, env.AgentRuntime.ToolPermissions) {
			return domain.LLMResponse{}, resp.RawContent, fmt.Errorf("tool %s not in allowed permissions", resp.Response.Message.Tool.ToolName)
		}
	}

	return resp.Response, resp.RawContent, nil
}

// executeTool calls the external data source HTTP endpoint for a tool.
// Routes through the secure channel: the request body (parsed JSON, not the
// raw RawMessage) is sealed when the channel is active, and the response is
// decrypted before being handed back to the LLM.
func (s *ChatService) executeTool(ctx context.Context, env *domain.ContextEnvelope, tool *domain.ToolCall) (json.RawMessage, error) {
	route, ok := env.TenantPolicy.RouteConfigs[tool.ToolName]
	if !ok {
		return nil, fmt.Errorf("no route config for tool %s", tool.ToolName)
	}

	// Substitute path parameters from tool parameters
	path := route.Path
	var params map[string]any
	if len(tool.Parameters) > 0 {
		_ = json.Unmarshal(tool.Parameters, &params)
		for k, v := range params {
			placeholder := fmt.Sprintf("{%s}", k)
			if strings.Contains(path, placeholder) {
				path = strings.ReplaceAll(path, placeholder, fmt.Sprintf("%v", v))
			}
		}
	}

	targetURL := route.BaseURL + path

	// Decode parameters into a typed value so channel.Do can re-marshal +
	// seal them. POST/PATCH/PUT carry the params as a body; GET does not.
	var reqBody any
	if route.Method == "POST" || route.Method == "PATCH" || route.Method == "PUT" {
		var parsed any
		if len(tool.Parameters) > 0 {
			if err := json.Unmarshal(tool.Parameters, &parsed); err != nil {
				return nil, fmt.Errorf("parse tool params: %w", err)
			}
			reqBody = parsed
		} else {
			reqBody = map[string]any{}
		}
	}

	start := time.Now()
	body, status, err := channel.Do(ctx, http.DefaultClient, channel.Request{
		Method: route.Method,
		URL:    targetURL,
		Body:   reqBody,
		Headers: map[string]string{
			// Internal bearer for tool data sources that gate on AUTH_STUB
			// (e.g. email-send). Hospital-mock ignores auth so this is
			// harmless there.
			"Authorization": "Bearer internal",
		},
	})
	elapsed := time.Since(start)

	s.logger.Info("tool call",
		slog.String("tool", tool.ToolName),
		slog.String("method", route.Method),
		slog.String("url", targetURL),
		slog.Int("status", status),
		slog.Int64("latency_ms", elapsed.Milliseconds()),
	)

	if err != nil {
		return nil, fmt.Errorf("tool http request: %w", err)
	}

	return json.RawMessage(body), nil
}

// handleEscalation transitions state to escalation_pending and notifies the operator queue.
func (s *ChatService) handleEscalation(ctx context.Context, env *domain.ContextEnvelope, sessionID string, info *domain.EscalationInfo, ttl time.Duration) {
	now := time.Now().UTC()
	operatorTTL := time.Duration(env.TenantPolicy.EscalationRules.OperatorTTLSeconds) * time.Second

	_ = s.redis.SetState(ctx, sessionID, domain.StateEscalationPending, ttl)
	_ = s.redis.AddToOpQueue(ctx, env.TenantPolicy.TenantID, sessionID, now)
	_ = s.redis.SetEscalationTTL(ctx, sessionID, operatorTTL)

	entry := domain.EscalationEntry{
		TriggeredAt:  now,
		Reason:       info.Reason,
		OperatorNote: info.OperatorNote,
	}

	go func() {
		bgCtx := context.Background()
		_ = s.sessions.AppendEscalationEntry(bgCtx, env.TenantPolicy.TenantID, sessionID, entry)
		_ = s.redis.EmitEvent(bgCtx, "escalation_triggered", map[string]any{
			"session_id": sessionID,
			"tenant_id":  env.TenantPolicy.TenantID,
			"reason":     info.Reason,
			"ts":         now.Format(time.RFC3339),
		})
	}()
}

// OperatorAccept claims an escalation_pending session for the given operator.
func (s *ChatService) OperatorAccept(ctx context.Context, tenantID, tenantSlug, sessionID, operatorID string) error {
	state, err := s.redis.GetState(ctx, sessionID)
	if err != nil {
		return fmt.Errorf("get state: %w", err)
	}
	if state != domain.StateEscalationPending {
		return fmt.Errorf("session is not in escalation_pending state (current: %s)", state)
	}

	env, err := s.redis.GetContext(ctx, sessionID)
	if err != nil || env == nil {
		return fmt.Errorf("load context: %w", err)
	}

	ttl := time.Duration(env.SessionMeta.IdleTimeoutSeconds+60) * time.Second
	_ = s.redis.SetState(ctx, sessionID, domain.StateOperatorActive, ttl)
	_ = s.redis.RemoveFromOpQueue(ctx, tenantID, sessionID)

	// Let the end user know a human has joined.
	_ = s.redis.PushOutbound(ctx, sessionID, "Un operador se ha unido a la conversación.", ttl)

	go func() {
		_ = s.sessions.UpdateState(context.Background(), tenantSlug, sessionID, domain.StateOperatorActive)
	}()

	return nil
}

// OperatorResolve closes or resumes a session after an operator finishes.
func (s *ChatService) OperatorResolve(ctx context.Context, tenantID, tenantSlug, sessionID, operatorID, resolveAction string) error {
	state, err := s.redis.GetState(ctx, sessionID)
	if err != nil {
		return fmt.Errorf("get state: %w", err)
	}
	if state != domain.StateOperatorActive {
		return fmt.Errorf("session is not in operator_active state (current: %s)", state)
	}

	env, err := s.redis.GetContext(ctx, sessionID)
	if err != nil || env == nil {
		return fmt.Errorf("load context: %w", err)
	}

	ttl := time.Duration(env.SessionMeta.IdleTimeoutSeconds+60) * time.Second
	now := time.Now().UTC()

	switch resolveAction {
	case "close":
		_ = s.redis.PushOutbound(ctx, sessionID, "La conversación ha finalizado. Gracias por contactarnos.", ttl)
		_ = s.redis.SetState(ctx, sessionID, domain.StateClosed, 60*time.Second)
		go func() {
			_ = s.entrypoint.CloseSession(context.Background(), tenantSlug, sessionID)
			_ = s.sessions.UpdateEscalationOperator(context.Background(), tenantSlug, sessionID, operatorID, &now)
		}()
	case "bot_resume":
		_ = s.redis.PushOutbound(ctx, sessionID, "Continuaremos con el asistente virtual. ¿En qué más puedo ayudarte?", ttl)
		_ = s.redis.SetState(ctx, sessionID, domain.StateBotActive, ttl)
		go func() {
			_ = s.sessions.UpdateState(context.Background(), tenantSlug, sessionID, domain.StateBotActive)
			_ = s.sessions.UpdateEscalationOperator(context.Background(), tenantSlug, sessionID, operatorID, &now)
		}()
	default:
		return fmt.Errorf("invalid resolve_action: must be 'close' or 'bot_resume'")
	}

	return nil
}

// EscalationSummary describes one session waiting for a human operator.
type EscalationSummary struct {
	SessionID    string `json:"session_id"`
	WaitingSince int64  `json:"waiting_since"` // Unix seconds
	Preview      string `json:"preview"`       // last user message
	EndUser      string `json:"end_user"`      // name or cellphone, if known
}

// ListEscalations returns the sessions of a tenant that are waiting for an
// operator to claim them, oldest first.
func (s *ChatService) ListEscalations(ctx context.Context, tenantID string) ([]EscalationSummary, error) {
	entries, err := s.redis.ListOpQueue(ctx, tenantID)
	if err != nil {
		return nil, fmt.Errorf("list operator queue: %w", err)
	}
	summaries := make([]EscalationSummary, 0, len(entries))
	for _, e := range entries {
		sid, _ := e.Member.(string)
		if sid == "" {
			continue
		}
		// Skip sessions that are no longer genuinely pending (claimed,
		// resumed, or closed since they were enqueued).
		if state, _ := s.redis.GetState(ctx, sid); state != domain.StateEscalationPending {
			continue
		}
		sum := EscalationSummary{SessionID: sid, WaitingSince: int64(e.Score)}
		if env, _ := s.redis.GetContext(ctx, sid); env != nil {
			sum.EndUser = env.EndUser.FullName
			if sum.EndUser == "" {
				sum.EndUser = env.EndUser.Cellphone
			}
		}
		if hist, _ := s.redis.GetHistory(ctx, sid); len(hist) > 0 {
			for i := len(hist) - 1; i >= 0; i-- {
				if hist[i].Role == domain.RoleUser {
					sum.Preview = hist[i].Content
					break
				}
			}
		}
		summaries = append(summaries, sum)
	}
	return summaries, nil
}

// OperatorMessage delivers a human operator's message to the end user. It
// records the message in history and queues it for the channel adapter.
func (s *ChatService) OperatorMessage(ctx context.Context, sessionID, text string) error {
	if text == "" {
		return fmt.Errorf("message text is required")
	}
	state, err := s.redis.GetState(ctx, sessionID)
	if err != nil {
		return fmt.Errorf("get state: %w", err)
	}
	if state != domain.StateOperatorActive {
		return fmt.Errorf("session is not operator_active (current: %s)", state)
	}
	env, err := s.redis.GetContext(ctx, sessionID)
	if err != nil || env == nil {
		return fmt.Errorf("load context: %w", err)
	}
	ttl := time.Duration(env.SessionMeta.IdleTimeoutSeconds+60) * time.Second
	s.appendAndFlush(ctx, env, sessionID, domain.Turn{
		Role: domain.RoleAssistant, Content: text, Ts: time.Now().UTC(),
	}, ttl)
	return s.redis.PushOutbound(ctx, sessionID, text, ttl)
}

// DrainOutbound returns and clears the operator messages queued for delivery
// on each of the given sessions. Called by chat-orch's Telegram loop.
func (s *ChatService) DrainOutbound(ctx context.Context, sessionIDs []string) (map[string][]string, error) {
	return s.redis.DrainOutbound(ctx, sessionIDs)
}

// GetHistory returns all turns for a session. It reads the live Redis
// history first — that is the most up to date during an active operator
// conversation and needs no tenant slug — and falls back to the persisted
// copy in MongoDB.
func (s *ChatService) GetHistory(ctx context.Context, tenantSlug, sessionID string) ([]domain.Turn, error) {
	turns, redisErr := s.redis.GetHistory(ctx, sessionID)
	if redisErr == nil && len(turns) > 0 {
		return turns, nil
	}
	session, err := s.sessions.GetByID(ctx, tenantSlug, sessionID)
	if err != nil || session == nil {
		if redisErr == nil {
			// No Mongo record but Redis answered (possibly empty) — not an error.
			return turns, nil
		}
		return nil, fmt.Errorf("session not found")
	}
	return session.Turns, nil
}

// GetState returns the current session state.
func (s *ChatService) GetState(ctx context.Context, sessionID string) (domain.SessionState, error) {
	return s.redis.GetState(ctx, sessionID)
}

// appendAndFlush appends a turn to Redis history and async-flushes to MongoDB.
func (s *ChatService) appendAndFlush(ctx context.Context, env *domain.ContextEnvelope, sessionID string, turn domain.Turn, ttl time.Duration) {
	_ = s.redis.AppendTurn(ctx, sessionID, turn, ttl)
	_ = s.redis.RefreshContextTTL(ctx, sessionID, ttl)

	go func() {
		bgCtx := context.Background()
		_ = s.sessions.AppendTurn(bgCtx, env.TenantPolicy.TenantID, sessionID, turn)
	}()
}

// applyFormatRules enforces max_chars and no_markdown rules before sending to user.
func (s *ChatService) applyFormatRules(text string, env *domain.ContextEnvelope) string {
	rules, ok := env.AgentRuntime.ChannelFormatRules[env.SessionMeta.ChannelType]
	if !ok {
		return text
	}
	if rules.NoMarkdown {
		text = stripMarkdown(text)
	}
	if rules.MaxChars > 0 && len(text) > rules.MaxChars {
		text = text[:rules.MaxChars-3] + "..."
	}
	return text
}

// isToolAllowed checks if a tool_name is in the agent's tool_permissions list.
func (s *ChatService) isToolAllowed(toolName string, permissions []domain.ToolPermission) bool {
	for _, p := range permissions {
		if p.ToolName == toolName {
			return true
		}
	}
	return false
}

// stripMarkdown removes basic markdown formatting (bold, italic, code, headers).
func stripMarkdown(text string) string {
	replacements := [][2]string{
		{"**", ""}, {"__", ""}, {"*", ""}, {"_", ""},
		{"```", ""}, {"`", ""},
		{"### ", ""}, {"## ", ""}, {"# ", ""},
	}
	for _, r := range replacements {
		text = strings.ReplaceAll(text, r[0], r[1])
	}
	return text
}
