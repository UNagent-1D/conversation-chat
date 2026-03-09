package handler

import (
	"net/http"

	"github.com/UNagent-1D/conversation-chat/internal/apperrors"
	"github.com/UNagent-1D/conversation-chat/internal/middleware"
	"github.com/UNagent-1D/conversation-chat/internal/service"
	"github.com/gin-gonic/gin"
)

// EntrypointHandler exposes the session lifecycle endpoints.
type EntrypointHandler struct {
	svc *service.EntrypointService
}

// NewEntrypointHandler creates a new EntrypointHandler.
func NewEntrypointHandler(svc *service.EntrypointService) *EntrypointHandler {
	return &EntrypointHandler{svc: svc}
}

// OpenSession handles POST /api/v1/sessions.
// Called by the Orchestrator once per new session. Returns session_id and welcome message.
func (h *EntrypointHandler) OpenSession(c *gin.Context) {
	var req service.OpenSessionRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}

	result, err := h.svc.CreateSession(c.Request.Context(), req)
	if err != nil {
		status := apperrors.HTTPStatus(err)
		c.JSON(status, gin.H{
			"error":      err.Error(),
			"request_id": c.GetString(middleware.CtxKeyRequestID),
		})
		return
	}

	c.JSON(http.StatusCreated, result)
}

// GetSession handles GET /api/v1/sessions/:sid.
func (h *EntrypointHandler) GetSession(c *gin.Context) {
	sessionID := c.Param("sid")
	tenantSlug := c.GetString(middleware.CtxKeyTenantSlug)

	session, err := h.svc.GetSession(c.Request.Context(), tenantSlug, sessionID)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{
			"error":      "internal server error",
			"request_id": c.GetString(middleware.CtxKeyRequestID),
		})
		return
	}
	if session == nil {
		c.JSON(http.StatusNotFound, gin.H{"error": "session not found"})
		return
	}

	c.JSON(http.StatusOK, gin.H{
		"session_id":       session.ID,
		"tenant_id":        session.TenantID,
		"agent_profile_id": session.AgentProfileID,
		"end_user_id":      session.EndUserID,
		"channel_type":     session.ChannelType,
		"state":            session.State,
		"opened_at":        session.OpenedAt,
		"closed_at":        session.ClosedAt,
	})
}

// CloseSession handles POST /api/v1/sessions/:sid/close.
// Called by the Orchestrator to explicitly close a session.
func (h *EntrypointHandler) CloseSession(c *gin.Context) {
	sessionID := c.Param("sid")
	tenantSlug := c.GetString(middleware.CtxKeyTenantSlug)

	if err := h.svc.CloseSession(c.Request.Context(), tenantSlug, sessionID); err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{
			"error":      "internal server error",
			"request_id": c.GetString(middleware.CtxKeyRequestID),
		})
		return
	}

	c.JSON(http.StatusOK, gin.H{"session_id": sessionID, "state": "closed"})
}
