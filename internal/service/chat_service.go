package service

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"strings"
	"time"

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
		return nil, fmt.Errorf("session not found or expired")
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
		return nil, fmt.Errorf("session is closed")
	case domain.StateOperatorActive:
		// Forward message to operator queue — operator handles response
		s.appendAndFlush(ctx, env, sessionID, domain.Turn{
			Role:       domain.RoleUser,
			Content:    req.UserMessage,
			ChannelKey: req.ChannelKey,
			MessageID:  req.MessageID,
			Ts:         time.Now().UTC(),
		}, ttl)
		return &TurnResponse{SessionID: sessionID, Message: struct{ Text string `json:"text"` }{"Un operador está atendiendo tu solicitud. Por favor espera."}}, nil
	case domain.StateEscalationPending:
		// Check if TTL expired (no operator claimed within the window)
		active, _ := s.redis.EscalationTTLActive(ctx, sessionID)
		if !active {
			// No operator responded in time — close session and notify user
			const noOperatorMsg = "Gracias por contactarnos. En este momento no tenemos un operador disponible. Un agente se comunicará contigo en los próximos días."
			s.handleEscalationTTLExpiry(ctx, env, sessionID)
			farewellTurn := domain.Turn{Role: domain.RoleAssistant, Content: noOperatorMsg, Ts: time.Now().UTC()}
			s.appendAndFlush(ctx, env, sessionID, farewellTurn, 60*time.Second)
			return &TurnResponse{SessionID: sessionID, Message: struct{ Text string `json:"text"` }{noOperatorMsg}}, nil
		}
		return &TurnResponse{SessionID: sessionID, Message: struct{ Text string `json:"text"` }{"Estamos conectándote con un operador. Por favor espera."}}, nil
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

	var bodyReader io.Reader
	if route.Method == "POST" || route.Method == "PATCH" || route.Method == "PUT" {
		bodyReader = strings.NewReader(string(tool.Parameters))
	}

	req, err := http.NewRequestWithContext(ctx, route.Method, targetURL, bodyReader)
	if err != nil {
		return nil, fmt.Errorf("build tool request: %w", err)
	}
	if bodyReader != nil {
		req.Header.Set("Content-Type", "application/json")
	}

	start := time.Now()
	resp, err := http.DefaultClient.Do(req)
	elapsed := time.Since(start)

	s.logger.Info("tool call",
		slog.String("tool", tool.ToolName),
		slog.String("method", route.Method),
		slog.String("url", targetURL),
		slog.Int64("latency_ms", elapsed.Milliseconds()),
	)

	if err != nil {
		return nil, fmt.Errorf("tool http request: %w", err)
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("read tool response: %w", err)
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

// handleEscalationTTLExpiry resolves an expired escalation according to ttl_fallback.
// handleEscalationTTLExpiry always closes the session when no operator claims it in time.
func (s *ChatService) handleEscalationTTLExpiry(ctx context.Context, env *domain.ContextEnvelope, sessionID string) {
	_ = s.redis.RemoveFromOpQueue(ctx, env.TenantPolicy.TenantID, sessionID)
	go func() {
		_ = s.entrypoint.CloseSession(context.Background(), env.TenantPolicy.TenantID, sessionID)
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
		_ = s.redis.SetState(ctx, sessionID, domain.StateClosed, 60*time.Second)
		go func() {
			_ = s.entrypoint.CloseSession(context.Background(), tenantSlug, sessionID)
			_ = s.sessions.UpdateEscalationOperator(context.Background(), tenantSlug, sessionID, operatorID, &now)
		}()
	case "bot_resume":
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

// GetHistory returns all turns for a session from MongoDB.
func (s *ChatService) GetHistory(ctx context.Context, tenantSlug, sessionID string) ([]domain.Turn, error) {
	session, err := s.sessions.GetByID(ctx, tenantSlug, sessionID)
	if err != nil || session == nil {
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
