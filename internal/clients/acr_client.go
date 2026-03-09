package clients

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"

	"github.com/UNagent-1D/conversation-chat/internal/domain"
)

// ACRConfig is the response from GET /api/v1/tenants/:id/profiles/:pid/configs/active.
type ACRConfig struct {
	ID                 string                  `json:"id"`
	Version            int                     `json:"version"`
	Status             string                  `json:"status"`
	ConversationPolicy json.RawMessage         `json:"conversation_policy"`
	EscalationRules    json.RawMessage         `json:"escalation_rules"`
	ToolPermissions    []domain.ToolPermission `json:"tool_permissions"`
	LLMParams          LLMParams               `json:"llm_params"`
	ChannelFormatRules map[string]domain.ChannelFormat `json:"channel_format_rules"`
	ActivatedAt        string                  `json:"activated_at"`
}

// LLMParams holds the model-level parameters from the ACR active config.
type LLMParams struct {
	Model        string  `json:"model"`
	Temperature  float64 `json:"temperature"`
	MaxTokens    int     `json:"max_tokens"`
	SystemPrompt string  `json:"system_prompt"`
}

// ACREscalationRules is the shape of escalation_rules in ACR.
type ACREscalationRules struct {
	Triggers           []ACRTrigger `json:"triggers"`
	OperatorTTLSeconds int          `json:"operator_ttl_seconds"`
	TTLFallback        string       `json:"ttl_fallback"`
}

type ACRTrigger struct {
	Condition string `json:"condition"`
	Action    string `json:"action"`
}

// ACRClient fetches active agent configs from the Agent Config Registry.
type ACRClient struct {
	baseURL    string
	httpClient *http.Client
	token      string // service-to-service JWT
}

// NewACRClient creates a new ACRClient.
func NewACRClient(baseURL, token string) *ACRClient {
	return &ACRClient{
		baseURL:    baseURL,
		httpClient: &http.Client{},
		token:      token,
	}
}

// GetActiveConfig fetches the active agent config for a tenant profile.
// Calls: GET /api/v1/tenants/:id/profiles/:pid/configs/active
func (c *ACRClient) GetActiveConfig(ctx context.Context, tenantID, profileID string) (*ACRConfig, error) {
	url := fmt.Sprintf("%s/api/v1/tenants/%s/profiles/%s/configs/active", c.baseURL, tenantID, profileID)

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, fmt.Errorf("build acr request: %w", err)
	}
	req.Header.Set("Authorization", "Bearer "+c.token)
	req.Header.Set("Content-Type", "application/json")

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("acr request: %w", err)
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("read acr response: %w", err)
	}

	if resp.StatusCode == http.StatusNotFound {
		return nil, fmt.Errorf("no active config for profile %s", profileID)
	}
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("acr returned %d: %s", resp.StatusCode, string(body))
	}

	var cfg ACRConfig
	if err := json.Unmarshal(body, &cfg); err != nil {
		return nil, fmt.Errorf("unmarshal acr config: %w", err)
	}
	return &cfg, nil
}

// ToAgentRuntime converts an ACRConfig into the domain AgentRuntime layer.
func (cfg *ACRConfig) ToAgentRuntime() domain.AgentRuntime {
	return domain.AgentRuntime{
		AgentConfigID:      cfg.ID,
		Model:              cfg.LLMParams.Model,
		Temperature:        cfg.LLMParams.Temperature,
		MaxTokens:          cfg.LLMParams.MaxTokens,
		SystemPrompt:       cfg.LLMParams.SystemPrompt,
		ToolPermissions:    cfg.ToolPermissions,
		ChannelFormatRules: cfg.ChannelFormatRules,
	}
}

// ToEscalationRules parses the raw escalation_rules JSON from ACR into the domain type.
func (cfg *ACRConfig) ToEscalationRules() (domain.EscalationRules, error) {
	var raw ACREscalationRules
	if err := json.Unmarshal(cfg.EscalationRules, &raw); err != nil {
		return domain.EscalationRules{}, fmt.Errorf("parse escalation rules: %w", err)
	}

	triggers := make([]domain.EscalationTrigger, len(raw.Triggers))
	for i, t := range raw.Triggers {
		triggers[i] = domain.EscalationTrigger{Condition: t.Condition, Action: t.Action}
	}

	ttl := raw.OperatorTTLSeconds
	if ttl == 0 {
		ttl = 120 // default: 2 minutes
	}
	fallback := raw.TTLFallback
	if fallback == "" {
		fallback = "bot_resume"
	}

	return domain.EscalationRules{
		Triggers:           triggers,
		OperatorTTLSeconds: ttl,
		TTLFallback:        fallback,
	}, nil
}
