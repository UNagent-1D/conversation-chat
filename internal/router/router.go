package router

import (
	"log/slog"

	"github.com/UNagent-1D/conversation-chat/internal/handler"
	"github.com/UNagent-1D/conversation-chat/internal/middleware"
	"github.com/gin-gonic/gin"
)

// Handlers groups all HTTP handler instances.
type Handlers struct {
	Health      *handler.HealthHandler
	Entrypoint  *handler.EntrypointHandler
	Chat        *handler.ChatHandler
}

// New creates the Gin engine with all routes and middleware configured.
func New(ginMode string, authCfg middleware.AuthConfig, logger *slog.Logger, h Handlers) *gin.Engine {
	gin.SetMode(ginMode)

	r := gin.New()
	r.Use(gin.Recovery())
	r.Use(middleware.RequestID())
	r.Use(middleware.Logger(logger))

	api := r.Group("/api/v1")

	// ── Health (public) ──────────────────────────────────────────────────────
	api.GET("/health", h.Health.Check)

	// ── Authenticated routes ─────────────────────────────────────────────────
	auth := api.Group("")
	auth.Use(middleware.Auth(authCfg))

	// ── Conversation / Entrypoint ────────────────────────────────────────────
	// POST   /sessions                  → open a new session (internal Orchestrator JWT)
	// GET    /sessions/:sid             → read session metadata (tenant_admin, tenant_operator)
	// POST   /sessions/:sid/close       → close session (internal Orchestrator JWT)
	auth.POST("/sessions",
		middleware.RequireRole("app_admin", "internal"),
		h.Entrypoint.OpenSession,
	)
	auth.GET("/sessions/:sid",
		middleware.RequireRole("app_admin", "tenant_admin", "tenant_operator"),
		h.Entrypoint.GetSession,
	)
	auth.POST("/sessions/:sid/close",
		middleware.RequireRole("app_admin", "internal"),
		h.Entrypoint.CloseSession,
	)

	// ── Conversation / Chat ──────────────────────────────────────────────────
	// POST   /sessions/:sid/turns           → submit user turn (internal Orchestrator JWT)
	// GET    /sessions/:sid/history         → full history (tenant_admin, tenant_operator)
	// GET    /sessions/:sid/state           → current state (tenant_admin, tenant_operator)
	// POST   /sessions/:sid/operator-accept → claim escalated session (tenant_operator)
	// POST   /sessions/:sid/operator-resolve→ resolve session (tenant_operator)
	auth.POST("/sessions/:sid/turns",
		middleware.RequireRole("app_admin", "internal"),
		h.Chat.ProcessTurn,
	)
	auth.GET("/sessions/:sid/history",
		middleware.RequireRole("app_admin", "tenant_admin", "tenant_operator"),
		h.Chat.GetHistory,
	)
	auth.GET("/sessions/:sid/state",
		middleware.RequireRole("app_admin", "tenant_admin", "tenant_operator"),
		h.Chat.GetState,
	)
	auth.POST("/sessions/:sid/operator-accept",
		middleware.RequireRole("tenant_operator", "tenant_admin", "app_admin"),
		h.Chat.OperatorAccept,
	)
	auth.POST("/sessions/:sid/operator-resolve",
		middleware.RequireRole("tenant_operator", "tenant_admin", "app_admin"),
		h.Chat.OperatorResolve,
	)

	return r
}
