package handler

import (
	"context"
	"net/http"
	"time"

	"github.com/gin-gonic/gin"
)

// Pinger is satisfied by any component that can check its own connectivity.
type Pinger interface {
	Ping(ctx context.Context) error
}

// HealthHandler handles GET /health.
type HealthHandler struct {
	redis   Pinger
	mongo   Pinger
	version string
}

// NewHealthHandler creates a new HealthHandler.
func NewHealthHandler(redis, mongo Pinger, version string) *HealthHandler {
	return &HealthHandler{redis: redis, mongo: mongo, version: version}
}

// Check returns liveness/readiness status for Redis and MongoDB.
// Always returns HTTP 200 — load balancers should monitor the status field.
func (h *HealthHandler) Check(c *gin.Context) {
	ctx, cancel := context.WithTimeout(c.Request.Context(), 3*time.Second)
	defer cancel()

	redisStatus := "ok"
	if err := h.redis.Ping(ctx); err != nil {
		redisStatus = "error: " + err.Error()
	}

	mongoStatus := "ok"
	if err := h.mongo.Ping(ctx); err != nil {
		mongoStatus = "error: " + err.Error()
	}

	overall := "ok"
	if redisStatus != "ok" || mongoStatus != "ok" {
		overall = "degraded"
	}

	c.JSON(http.StatusOK, gin.H{
		"status":    overall,
		"redis":     redisStatus,
		"mongo":     mongoStatus,
		"version":   h.version,
		"timestamp": time.Now().UTC().Format(time.RFC3339),
	})
}
