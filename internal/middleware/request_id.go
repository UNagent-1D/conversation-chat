package middleware

import (
	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
)

const CtxKeyRequestID = "request_id"

// RequestID injects a unique request ID into every request.
// Uses the X-Request-ID header if present; otherwise generates a new UUID.
func RequestID() gin.HandlerFunc {
	return func(c *gin.Context) {
		rid := c.GetHeader("X-Request-ID")
		if rid == "" {
			rid = uuid.NewString()
		}
		c.Set(CtxKeyRequestID, rid)
		c.Header("X-Request-ID", rid)
		c.Next()
	}
}
