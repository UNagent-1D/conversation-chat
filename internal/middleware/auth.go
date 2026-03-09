package middleware

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"

	"github.com/gin-gonic/gin"
)

const (
	CtxKeyUserID     = "user_id"
	CtxKeyRole       = "role"
	CtxKeyTenantID   = "tenant_id"
	CtxKeyTenantSlug = "tenant_slug"
	CtxKeyEmail      = "email"
)

// Claims is the expected response body from the auth microservice /validate endpoint.
type Claims struct {
	UserID     string  `json:"user_id"`
	Role       string  `json:"role"`
	TenantID   *string `json:"tenant_id"`
	TenantSlug *string `json:"tenant_slug"`
	Email      string  `json:"email"`
}

// AuthConfig holds the settings needed by the Auth middleware.
type AuthConfig struct {
	ServiceURL string
	Stub       bool
	StubClaims Claims
}

// Auth validates the Bearer token by calling the auth microservice.
// When Stub is true it bypasses the call and injects StubClaims directly.
func Auth(cfg AuthConfig) gin.HandlerFunc {
	return func(c *gin.Context) {
		header := c.GetHeader("Authorization")
		if len(header) < 8 || header[:7] != "Bearer " {
			c.AbortWithStatusJSON(http.StatusUnauthorized, gin.H{"error": "missing or invalid token"})
			return
		}
		token := header[7:]

		var claims Claims
		if cfg.Stub {
			claims = cfg.StubClaims
		} else {
			var err error
			claims, err = validateToken(cfg.ServiceURL, token)
			if err != nil {
				c.AbortWithStatusJSON(http.StatusUnauthorized, gin.H{"error": "missing or invalid token"})
				return
			}
		}

		c.Set(CtxKeyUserID, claims.UserID)
		c.Set(CtxKeyRole, claims.Role)
		c.Set(CtxKeyEmail, claims.Email)
		if claims.TenantID != nil {
			c.Set(CtxKeyTenantID, *claims.TenantID)
		}
		if claims.TenantSlug != nil {
			c.Set(CtxKeyTenantSlug, *claims.TenantSlug)
		}
		c.Next()
	}
}

func validateToken(serviceURL, token string) (Claims, error) {
	body, _ := json.Marshal(map[string]string{"token": token})
	req, err := http.NewRequest(http.MethodPost, serviceURL+"/validate", bytes.NewReader(body))
	if err != nil {
		return Claims{}, err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+token)

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return Claims{}, fmt.Errorf("auth service unreachable: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return Claims{}, fmt.Errorf("auth service returned %d", resp.StatusCode)
	}

	raw, err := io.ReadAll(resp.Body)
	if err != nil {
		return Claims{}, err
	}

	var claims Claims
	if err := json.Unmarshal(raw, &claims); err != nil {
		return Claims{}, fmt.Errorf("invalid auth response: %w", err)
	}
	return claims, nil
}
