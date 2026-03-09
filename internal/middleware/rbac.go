package middleware

import (
	"net/http"

	"github.com/gin-gonic/gin"
)

// RequireRole aborts with 403 if the caller's role is not in the allowed list.
func RequireRole(roles ...string) gin.HandlerFunc {
	allowed := make(map[string]bool, len(roles))
	for _, r := range roles {
		allowed[r] = true
	}
	return func(c *gin.Context) {
		role := c.GetString(CtxKeyRole)
		if !allowed[role] {
			c.AbortWithStatusJSON(http.StatusForbidden, gin.H{"error": "insufficient role"})
			return
		}
		c.Next()
	}
}

// RequireTenantAccess ensures the :id URL param matches the caller's tenant_id.
// app_admin bypasses this check and can access any tenant.
func RequireTenantAccess() gin.HandlerFunc {
	return func(c *gin.Context) {
		role := c.GetString(CtxKeyRole)
		if role == "app_admin" {
			c.Next()
			return
		}
		paramID := c.Param("id")
		callerTenantID := c.GetString(CtxKeyTenantID)
		if paramID == "" || callerTenantID == "" || paramID != callerTenantID {
			c.AbortWithStatusJSON(http.StatusForbidden, gin.H{"error": "access denied: tenant mismatch"})
			return
		}
		c.Next()
	}
}
