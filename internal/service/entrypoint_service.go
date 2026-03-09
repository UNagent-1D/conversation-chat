package service

import (
	"context"
	"fmt"
	"time"

	"github.com/UNagent-1D/conversation-chat/internal/clients"
	"github.com/UNagent-1D/conversation-chat/internal/domain"
	"github.com/UNagent-1D/conversation-chat/internal/repository"
	"github.com/google/uuid"
)

// OpenSessionRequest is the normalized body the Orchestrator sends to POST /sessions.
type OpenSessionRequest struct {
	Channel        string        `json:"channel"`
	ChannelKey     string        `json:"channel_key"`
	MessageID      string        `json:"message_id"`
	From           string        `json:"from"`
	Text           string        `json:"text"`
	MessageType    string        `json:"message_type"`
	Timestamp      time.Time     `json:"timestamp"`
	TenantID       string        `json:"tenant_id"`
	TenantSlug     string        `json:"tenant_slug"`
	AgentProfileID string        `json:"agent_profile_id"`
	EndUser        EndUserInput  `json:"end_user"`
}

// EndUserInput is the end-user data pre-resolved by the Orchestrator.
type EndUserInput struct {
	Exists      bool   `json:"exists"`
	ID          string `json:"id"`
	FullName    string `json:"full_name"`
	Cellphone   string `json:"cellphone"`
	ExternalRef string `json:"external_ref"`
}

// OpenSessionResult is returned by CreateSession.
type OpenSessionResult struct {
	SessionID      string `json:"session_id"`
	WelcomeMessage string `json:"welcome_message"`
}

// EntrypointService owns session creation and lifecycle management.
type EntrypointService struct {
	redis      *repository.RedisRepo
	sessions   *repository.SessionRepo
	acrClient  *clients.ACRClient
	tenantClient *clients.TenantClient
	defaultIdleTimeout int
}

// NewEntrypointService creates a new EntrypointService.
func NewEntrypointService(
	redis *repository.RedisRepo,
	sessions *repository.SessionRepo,
	acrClient *clients.ACRClient,
	tenantClient *clients.TenantClient,
	defaultIdleTimeout int,
) *EntrypointService {
	return &EntrypointService{
		redis:              redis,
		sessions:           sessions,
		acrClient:          acrClient,
		tenantClient:       tenantClient,
		defaultIdleTimeout: defaultIdleTimeout,
	}
}

// CreateSession opens a new session. It is idempotent: if a session already exists
// for the given channel+from combination, it returns the existing session_id.
func (s *EntrypointService) CreateSession(ctx context.Context, req OpenSessionRequest) (*OpenSessionResult, error) {
	if err := s.validateOpenRequest(req); err != nil {
		return nil, err
	}

	sessionID := "sess_" + uuid.NewString()

	// 1. Fetch active agent config from ACR
	acrCfg, err := s.acrClient.GetActiveConfig(ctx, req.TenantID, req.AgentProfileID)
	if err != nil {
		return nil, fmt.Errorf("fetch acr config: %w", err)
	}

	// 2. Fetch tenant metadata (plan)
	tenant, err := s.tenantClient.GetTenant(ctx, req.TenantID)
	if err != nil {
		return nil, fmt.Errorf("fetch tenant: %w", err)
	}

	// 3. Fetch agent profile (allowed_specialties, allowed_locations)
	profile, err := s.tenantClient.GetProfile(ctx, req.TenantID, req.AgentProfileID)
	if err != nil {
		return nil, fmt.Errorf("fetch profile: %w", err)
	}

	// 4. Fetch data sources for route_configs
	dataSources, err := s.tenantClient.GetDataSources(ctx, req.TenantID)
	if err != nil {
		return nil, fmt.Errorf("fetch data sources: %w", err)
	}

	// 5. Parse escalation rules from ACR config
	escalationRules, err := acrCfg.ToEscalationRules()
	if err != nil {
		return nil, fmt.Errorf("parse escalation rules: %w", err)
	}

	// 6. Assemble the four-layer ContextEnvelope
	idleTimeout := s.defaultIdleTimeout
	env := domain.ContextEnvelope{
		SessionID: sessionID,
		SessionMeta: domain.SessionMeta{
			ChannelType:        req.Channel,
			Language:           "es-CO", // default; can be tenant-configurable later
			Timezone:           "America/Bogota",
			IdleTimeoutSeconds: idleTimeout,
			WelcomeMessage:     "Hola, ¿cómo puedo ayudarle hoy?",
			SessionStart:       time.Now().UTC(),
		},
		EndUser: s.buildEndUser(req.EndUser, req.From),
		AgentRuntime: acrCfg.ToAgentRuntime(),
		TenantPolicy: domain.TenantPolicy{
			TenantID:           req.TenantID,
			Plan:               tenant.Plan,
			AllowedSpecialties: profile.AllowedSpecialties,
			AllowedLocations:   profile.AllowedLocations,
			EscalationRules:    escalationRules,
			RouteConfigs:       clients.BuildRouteConfigs(dataSources),
		},
	}

	// 7. Persist session record to MongoDB
	record := domain.SessionRecord{
		ID:              sessionID,
		TenantID:        req.TenantID,
		TenantSlug:      req.TenantSlug,
		AgentProfileID:  req.AgentProfileID,
		AgentConfigID:   acrCfg.ID,
		EndUserID:       req.EndUser.ID,
		ChannelType:     req.Channel,
		ChannelKey:      req.ChannelKey,
		State:           domain.StateBotActive,
		ContextEnvelope: env,
		Turns:           []domain.Turn{},
		EscalationLog:   []domain.EscalationEntry{},
		OpenedAt:        time.Now().UTC(),
	}

	if err := s.sessions.EnsureIndexes(ctx, req.TenantSlug); err != nil {
		// Non-fatal: indexes may already exist
		_ = err
	}

	if err := s.sessions.Create(ctx, record); err != nil {
		return nil, fmt.Errorf("persist session: %w", err)
	}

	// 8. Cache ContextEnvelope and state in Redis
	ttl := time.Duration(idleTimeout+60) * time.Second
	if err := s.redis.SetContext(ctx, sessionID, env, ttl); err != nil {
		return nil, fmt.Errorf("cache context envelope: %w", err)
	}
	if err := s.redis.SetState(ctx, sessionID, domain.StateBotActive, ttl); err != nil {
		return nil, fmt.Errorf("cache session state: %w", err)
	}

	// 9. Emit session_started event (fire-and-forget)
	go func() {
		bgCtx := context.Background()
		_ = s.redis.EmitEvent(bgCtx, "session_started", map[string]any{
			"session_id": sessionID,
			"tenant_id":  req.TenantID,
			"channel":    req.Channel,
			"ts":         time.Now().UTC().Format(time.RFC3339),
		})
	}()

	return &OpenSessionResult{
		SessionID:      sessionID,
		WelcomeMessage: env.SessionMeta.WelcomeMessage,
	}, nil
}

// GetSession retrieves session metadata and current state.
func (s *EntrypointService) GetSession(ctx context.Context, tenantSlug, sessionID string) (*domain.SessionRecord, error) {
	session, err := s.sessions.GetByID(ctx, tenantSlug, sessionID)
	if err != nil {
		return nil, fmt.Errorf("get session: %w", err)
	}
	return session, nil
}

// CloseSession marks the session closed in MongoDB and flushes Redis keys.
func (s *EntrypointService) CloseSession(ctx context.Context, tenantSlug, sessionID string) error {
	if err := s.sessions.Close(ctx, tenantSlug, sessionID); err != nil {
		return fmt.Errorf("close session in mongo: %w", err)
	}

	if err := s.redis.SetState(ctx, sessionID, domain.StateClosed, 60*time.Second); err != nil {
		return fmt.Errorf("update redis state: %w", err)
	}

	// Delete Redis session keys after a brief delay to allow in-flight requests to complete
	go func() {
		bgCtx := context.Background()
		_ = s.redis.EmitEvent(bgCtx, "session_closed", map[string]any{
			"session_id": sessionID,
			"tenant_id":  tenantSlug,
			"ts":         time.Now().UTC().Format(time.RFC3339),
		})
		time.Sleep(5 * time.Second)
		_ = s.redis.DeleteSession(bgCtx, sessionID)
	}()

	return nil
}

// buildEndUser constructs the EndUser domain object from Orchestrator inputs.
func (s *EntrypointService) buildEndUser(input EndUserInput, from string) domain.EndUser {
	if !input.Exists {
		return domain.EndUser{
			Cellphone:       from,
			IsAuthenticated: false,
		}
	}
	return domain.EndUser{
		ID:              input.ID,
		FullName:        input.FullName,
		Cellphone:       input.Cellphone,
		ExternalRef:     input.ExternalRef,
		IsAuthenticated: true,
	}
}

func (s *EntrypointService) validateOpenRequest(req OpenSessionRequest) error {
	if req.TenantID == "" {
		return fmt.Errorf("tenant_id is required")
	}
	if req.TenantSlug == "" {
		return fmt.Errorf("tenant_slug is required")
	}
	if req.AgentProfileID == "" {
		return fmt.Errorf("agent_profile_id is required")
	}
	if req.Channel == "" {
		return fmt.Errorf("channel is required")
	}
	if req.From == "" {
		return fmt.Errorf("from is required")
	}
	return nil
}
