package main

import (
	"context"
	"log"
	"log/slog"
	"os"

	"github.com/UNagent-1D/conversation-chat/internal/clients"
	"github.com/UNagent-1D/conversation-chat/internal/clients/llm"
	"github.com/UNagent-1D/conversation-chat/internal/config"
	"github.com/UNagent-1D/conversation-chat/internal/db"
	"github.com/UNagent-1D/conversation-chat/internal/handler"
	"github.com/UNagent-1D/conversation-chat/internal/middleware"
	"github.com/UNagent-1D/conversation-chat/internal/repository"
	"github.com/UNagent-1D/conversation-chat/internal/router"
	"github.com/UNagent-1D/conversation-chat/internal/service"
)

func main() {
	cfg := config.Load()

	logger := slog.New(slog.NewJSONHandler(os.Stdout, &slog.HandlerOptions{
		Level: slog.LevelInfo,
	}))

	ctx := context.Background()

	// ── Redis ──────────────────────────────────────────────────────────────────
	redisClient, err := db.NewRedisClient(ctx, cfg.RedisURL)
	if err != nil {
		log.Fatalf("connect to redis: %v", err)
	}
	defer redisClient.Close()

	// ── MongoDB ────────────────────────────────────────────────────────────────
	mongoClient, err := db.NewMongoClient(ctx, cfg.MongoURI)
	if err != nil {
		log.Fatalf("connect to mongodb: %v", err)
	}
	defer func() {
		if err := mongoClient.Disconnect(ctx); err != nil {
			logger.Warn("mongo disconnect error", slog.String("error", err.Error()))
		}
	}()
	mongoDB := mongoClient.Database(cfg.MongoDB)

	// ── Repositories ───────────────────────────────────────────────────────────
	redisRepo := repository.NewRedisRepo(redisClient)
	sessionRepo := repository.NewSessionRepo(mongoDB)

	// ── External clients ───────────────────────────────────────────────────────
	// Service-to-service token: in production this should be a long-lived internal JWT.
	// For MVP/dev, AUTH_STUB_USER_ID is used as a placeholder.
	internalToken := cfg.AuthStubClaims.UserID

	acrClient := clients.NewACRClient(cfg.ACRServiceURL, internalToken)
	tenantClient := clients.NewTenantClient(cfg.TenantServiceURL, internalToken)
	llmClient := llm.NewOpenAIClient(cfg.OpenAIAPIKey)

	// ── Services ───────────────────────────────────────────────────────────────
	entrypointSvc := service.NewEntrypointService(
		redisRepo,
		sessionRepo,
		acrClient,
		tenantClient,
		cfg.DefaultIdleTimeoutSeconds,
	)

	chatSvc := service.NewChatService(
		redisRepo,
		sessionRepo,
		llmClient,
		entrypointSvc,
		logger,
	)

	// ── Handlers ───────────────────────────────────────────────────────────────
	h := router.Handlers{
		Health:     handler.NewHealthHandler(redisRepo, sessionRepo, cfg.AppVersion),
		Entrypoint: handler.NewEntrypointHandler(entrypointSvc),
		Chat:       handler.NewChatHandler(chatSvc),
	}

	// ── Auth middleware config ─────────────────────────────────────────────────
	stubTenantID := cfg.AuthStubClaims.TenantID
	stubSlug := cfg.AuthStubClaims.TenantSlug

	var stubTenantIDPtr, stubSlugPtr *string
	if stubTenantID != "" {
		stubTenantIDPtr = &stubTenantID
	}
	if stubSlug != "" {
		stubSlugPtr = &stubSlug
	}

	authCfg := middleware.AuthConfig{
		ServiceURL: cfg.AuthServiceURL,
		Stub:       cfg.AuthStub,
		StubClaims: middleware.Claims{
			UserID:     cfg.AuthStubClaims.UserID,
			Role:       cfg.AuthStubClaims.Role,
			TenantID:   stubTenantIDPtr,
			TenantSlug: stubSlugPtr,
			Email:      cfg.AuthStubClaims.Email,
		},
	}

	// ── Start server ───────────────────────────────────────────────────────────
	r := router.New(cfg.GinMode, authCfg, logger, h)

	addr := ":" + cfg.ServerPort
	logger.Info("conversation-chat service starting",
		slog.String("addr", addr),
		slog.String("version", cfg.AppVersion),
	)
	if err := r.Run(addr); err != nil {
		log.Fatalf("server error: %v", err)
	}
}
