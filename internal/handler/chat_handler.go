package handler

import (
	"net/http"

	"github.com/UNagent-1D/conversation-chat/internal/apperrors"
	"github.com/UNagent-1D/conversation-chat/internal/middleware"
	"github.com/UNagent-1D/conversation-chat/internal/service"
	"github.com/gin-gonic/gin"
)

// ChatHandler exposes the per-turn conversation endpoints.
type ChatHandler struct {
	svc *service.ChatService
}

// NewChatHandler creates a new ChatHandler.
func NewChatHandler(svc *service.ChatService) *ChatHandler {
	return &ChatHandler{svc: svc}
}

// ProcessTurn handles POST /api/v1/sessions/:sid/turns.
// Called by the Orchestrator for every user message. Runs the full LLM loop.
func (h *ChatHandler) ProcessTurn(c *gin.Context) {
	sessionID := c.Param("sid")

	var req service.TurnRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}

	if req.UserMessage == "" {
		c.JSON(http.StatusBadRequest, gin.H{"error": "user_message is required"})
		return
	}

	result, err := h.svc.ProcessTurn(c.Request.Context(), sessionID, req)
	if err != nil {
		if err.Error() == "session not found or expired" || err.Error() == "session is closed" {
			c.JSON(http.StatusNotFound, gin.H{"error": err.Error()})
			return
		}
		status := apperrors.HTTPStatus(err)
		c.JSON(status, gin.H{
			"error":      err.Error(),
			"request_id": c.GetString(middleware.CtxKeyRequestID),
		})
		return
	}

	c.JSON(http.StatusOK, result)
}

// GetHistory handles GET /api/v1/sessions/:sid/history.
// Returns the full conversation history for the operator dashboard.
func (h *ChatHandler) GetHistory(c *gin.Context) {
	sessionID := c.Param("sid")
	tenantSlug := c.GetString(middleware.CtxKeyTenantSlug)

	turns, err := h.svc.GetHistory(c.Request.Context(), tenantSlug, sessionID)
	if err != nil {
		c.JSON(http.StatusNotFound, gin.H{"error": err.Error()})
		return
	}

	c.JSON(http.StatusOK, gin.H{
		"session_id": sessionID,
		"turns":      turns,
	})
}

// GetState handles GET /api/v1/sessions/:sid/state.
func (h *ChatHandler) GetState(c *gin.Context) {
	sessionID := c.Param("sid")

	state, err := h.svc.GetState(c.Request.Context(), sessionID)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{
			"error":      "internal server error",
			"request_id": c.GetString(middleware.CtxKeyRequestID),
		})
		return
	}
	if state == "" {
		c.JSON(http.StatusNotFound, gin.H{"error": "session not found"})
		return
	}

	c.JSON(http.StatusOK, gin.H{"session_id": sessionID, "state": state})
}

// OperatorAccept handles POST /api/v1/sessions/:sid/operator-accept.
// Transitions escalation_pending → operator_active.
func (h *ChatHandler) OperatorAccept(c *gin.Context) {
	sessionID := c.Param("sid")
	tenantID := c.GetString(middleware.CtxKeyTenantID)
	tenantSlug := c.GetString(middleware.CtxKeyTenantSlug)
	operatorID := c.GetString(middleware.CtxKeyUserID)

	if err := h.svc.OperatorAccept(c.Request.Context(), tenantID, tenantSlug, sessionID, operatorID); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}

	c.JSON(http.StatusOK, gin.H{"session_id": sessionID, "state": "operator_active"})
}

// OperatorResolve handles POST /api/v1/sessions/:sid/operator-resolve.
// Body: { "resolve_action": "close" | "bot_resume" }
func (h *ChatHandler) OperatorResolve(c *gin.Context) {
	sessionID := c.Param("sid")
	tenantID := c.GetString(middleware.CtxKeyTenantID)
	tenantSlug := c.GetString(middleware.CtxKeyTenantSlug)
	operatorID := c.GetString(middleware.CtxKeyUserID)

	var body struct {
		ResolveAction string `json:"resolve_action"`
	}
	if err := c.ShouldBindJSON(&body); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}

	if err := h.svc.OperatorResolve(c.Request.Context(), tenantID, tenantSlug, sessionID, operatorID, body.ResolveAction); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}

	c.JSON(http.StatusOK, gin.H{"session_id": sessionID, "resolve_action": body.ResolveAction})
}

// ListEscalations handles GET /api/v1/escalations.
// Returns the sessions waiting for an operator, for the operator console.
func (h *ChatHandler) ListEscalations(c *gin.Context) {
	// Prefer an explicit tenant_id query param; fall back to the token claim.
	tenantID := c.Query("tenant_id")
	if tenantID == "" {
		tenantID = c.GetString(middleware.CtxKeyTenantID)
	}
	if tenantID == "" {
		c.JSON(http.StatusBadRequest, gin.H{"error": "tenant_id is required (query param or token)"})
		return
	}

	items, err := h.svc.ListEscalations(c.Request.Context(), tenantID)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{
			"error":      err.Error(),
			"request_id": c.GetString(middleware.CtxKeyRequestID),
		})
		return
	}

	c.JSON(http.StatusOK, gin.H{"escalations": items})
}

// OperatorMessage handles POST /api/v1/sessions/:sid/operator-message.
// Body: { "text": "..." } — an operator's reply to the end user.
func (h *ChatHandler) OperatorMessage(c *gin.Context) {
	sessionID := c.Param("sid")

	var body struct {
		Text string `json:"text"`
	}
	if err := c.ShouldBindJSON(&body); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}

	if err := h.svc.OperatorMessage(c.Request.Context(), sessionID, body.Text); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}

	c.JSON(http.StatusOK, gin.H{"session_id": sessionID, "delivered": true})
}

// DrainOutbound handles POST /api/v1/outbound/drain.
// Internal: chat-orch's Telegram loop fetches operator messages to deliver.
// Body: { "session_ids": [...] }
func (h *ChatHandler) DrainOutbound(c *gin.Context) {
	var body struct {
		SessionIDs []string `json:"session_ids"`
	}
	if err := c.ShouldBindJSON(&body); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}

	msgs, err := h.svc.DrainOutbound(c.Request.Context(), body.SessionIDs)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}

	c.JSON(http.StatusOK, gin.H{"messages": msgs})
}
