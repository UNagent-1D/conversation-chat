package middleware

import (
	"log/slog"
	"time"

	"github.com/gin-gonic/gin"
)

// Logger writes a structured JSON log line after each request completes.
func Logger(logger *slog.Logger) gin.HandlerFunc {
	return func(c *gin.Context) {
		start := time.Now()
		c.Next()

		tenantID, _ := c.Get(CtxKeyTenantID)
		userID, _ := c.Get(CtxKeyUserID)

		logger.Info("request",
			slog.String("request_id", c.GetString(CtxKeyRequestID)),
			slog.String("method", c.Request.Method),
			slog.String("path", c.Request.URL.Path),
			slog.Int("status", c.Writer.Status()),
			slog.Int64("latency_ms", time.Since(start).Milliseconds()),
			slog.Any("tenant_id", tenantID),
			slog.Any("user_id", userID),
		)
	}
}
