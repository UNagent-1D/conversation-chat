package router

import (
	"log/slog"
	"net/http"

	"github.com/UNagent-1D/conversation-chat/internal/channel"
	"github.com/UNagent-1D/conversation-chat/internal/handler"
	"github.com/UNagent-1D/conversation-chat/internal/middleware"
	"github.com/gin-gonic/gin"
)

// corsMiddleware lets the browser-based operator console call this service
// directly. The frontend is a different origin (port 3000), so credentialed
// cross-origin requests need these headers and an OPTIONS preflight reply.
func corsMiddleware() gin.HandlerFunc {
	return func(c *gin.Context) {
		origin := c.GetHeader("Origin")
		if origin == "" {
			origin = "*"
		}
		c.Header("Access-Control-Allow-Origin", origin)
		c.Header("Access-Control-Allow-Credentials", "true")
		c.Header("Access-Control-Allow-Methods", "GET, POST, PATCH, PUT, DELETE, OPTIONS")
		c.Header("Access-Control-Allow-Headers", "Origin, Content-Type, Authorization, X-Request-Id")
		if c.Request.Method == http.MethodOptions {
			c.AbortWithStatus(http.StatusNoContent)
			return
		}
		c.Next()
	}
}

// Handlers groups all HTTP handler instances.
type Handlers struct {
	Health     *handler.HealthHandler
	Entrypoint *handler.EntrypointHandler
	Chat       *handler.ChatHandler
}

// New creates the Gin engine with all routes and middleware configured.
func New(ginMode string, authCfg middleware.AuthConfig, logger *slog.Logger, h Handlers) *gin.Engine {
	gin.SetMode(ginMode)

	r := gin.New()
	r.Use(gin.Recovery())
	r.Use(corsMiddleware())
	r.Use(middleware.RequestID())
	r.Use(middleware.Logger(logger))

	api := r.Group("/api/v1")

	// ── Health (public, plaintext) ───────────────────────────────────────────
	// Health probes don't share the channel key, so the secure-channel
	// middleware must be mounted on the auth group only.
	api.GET("/health", h.Health.Check)

	// ── Operator outbound drain (internal, plaintext) ────────────────────────
	// chat-orch's Telegram loop polls this to deliver operator messages to the
	// end user. Internal Docker network only — no auth, no secure channel.
	api.POST("/outbound/drain", h.Chat.DrainOutbound)

	// ── Authenticated routes ─────────────────────────────────────────────────
	// Secure channel decrypts the body before auth so the bearer/claims are
	// read from plaintext; the writer is wrapped so the response is sealed
	// when the caller used the envelope on the way in.
	auth := api.Group("")
	auth.Use(channel.Middleware())
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

	// ── Operator console ─────────────────────────────────────────────────────
	// GET  /escalations                     → sessions waiting for an operator
	// POST /sessions/:sid/operator-message   → operator reply to the end user
	auth.GET("/escalations",
		middleware.RequireRole("tenant_operator", "tenant_admin", "app_admin"),
		h.Chat.ListEscalations,
	)
	auth.POST("/sessions/:sid/operator-message",
		middleware.RequireRole("tenant_operator", "tenant_admin", "app_admin"),
		h.Chat.OperatorMessage,
	)

	return r
}
