package domain

import "time"

// SessionState represents the current lifecycle state of a session.
type SessionState string

const (
	StateBotActive         SessionState = "bot_active"
	StateEscalationPending SessionState = "escalation_pending"
	StateOperatorActive    SessionState = "operator_active"
	StateClosed            SessionState = "closed"
)

// ContextEnvelope is assembled once at session open and cached in Redis for the session lifetime.
// It has four distinct layers, each with a clear source and ownership.
type ContextEnvelope struct {
	SessionID    string       `json:"session_id"    bson:"session_id"`
	SessionMeta  SessionMeta  `json:"session_meta"  bson:"session_meta"`
	EndUser      EndUser      `json:"end_user"      bson:"end_user"`
	AgentRuntime AgentRuntime `json:"agent_runtime" bson:"agent_runtime"`
	TenantPolicy TenantPolicy `json:"tenant_policy" bson:"tenant_policy"`
}

// SessionMeta — layer 1: session lifecycle configuration.
type SessionMeta struct {
	ChannelType        string    `json:"channel_type"         bson:"channel_type"`
	Language           string    `json:"language"             bson:"language"`
	Timezone           string    `json:"timezone"             bson:"timezone"`
	IdleTimeoutSeconds int       `json:"idle_timeout_seconds" bson:"idle_timeout_seconds"`
	WelcomeMessage     string    `json:"welcome_message"      bson:"welcome_message"`
	SessionStart       time.Time `json:"session_start"        bson:"session_start"`
}

// EndUser — layer 2: resolved patient identity from Tenant Service.
type EndUser struct {
	ID              string `json:"id"               bson:"id"`
	FullName        string `json:"full_name"        bson:"full_name"`
	Cellphone       string `json:"cellphone"        bson:"cellphone"`
	ExternalRef     string `json:"external_ref"     bson:"external_ref"`
	IsAuthenticated bool   `json:"is_authenticated" bson:"is_authenticated"`
}

// AgentRuntime — layer 3: active LLM config from ACR.
type AgentRuntime struct {
	AgentConfigID      string                    `json:"agent_config_id"      bson:"agent_config_id"`
	Model              string                    `json:"model"                bson:"model"`
	Temperature        float64                   `json:"temperature"          bson:"temperature"`
	MaxTokens          int                       `json:"max_tokens"           bson:"max_tokens"`
	SystemPrompt       string                    `json:"system_prompt"        bson:"system_prompt"`
	ToolPermissions    []ToolPermission          `json:"tool_permissions"     bson:"tool_permissions"`
	ChannelFormatRules map[string]ChannelFormat  `json:"channel_format_rules" bson:"channel_format_rules"`
}

// ToolPermission is a single tool the agent is allowed to invoke.
type ToolPermission struct {
	ToolName    string         `json:"tool_name"   bson:"tool_name"`
	Constraints map[string]any `json:"constraints" bson:"constraints"`
}

// ChannelFormat controls message formatting per channel type.
type ChannelFormat struct {
	MaxChars   int  `json:"max_chars"   bson:"max_chars"`
	NoMarkdown bool `json:"no_markdown" bson:"no_markdown"`
}

// TenantPolicy — layer 4: business rules and data source routing from Tenant Service.
type TenantPolicy struct {
	TenantID           string                  `json:"tenant_id"           bson:"tenant_id"`
	Plan               string                  `json:"plan"                bson:"plan"`
	AllowedSpecialties []string                `json:"allowed_specialties" bson:"allowed_specialties"`
	AllowedLocations   []string                `json:"allowed_locations"   bson:"allowed_locations"`
	EscalationRules    EscalationRules         `json:"escalation_rules"    bson:"escalation_rules"`
	RouteConfigs       map[string]RouteConfig  `json:"route_configs"       bson:"route_configs"`
}

// EscalationRules defines when and how to escalate to a human operator.
type EscalationRules struct {
	Triggers           []EscalationTrigger `json:"triggers"             bson:"triggers"`
	OperatorTTLSeconds int                 `json:"operator_ttl_seconds" bson:"operator_ttl_seconds"`
	TTLFallback        string              `json:"ttl_fallback"         bson:"ttl_fallback"` // "bot_resume" | "close"
}

// EscalationTrigger maps a condition to an action.
type EscalationTrigger struct {
	Condition string `json:"condition" bson:"condition"`
	Action    string `json:"action"    bson:"action"`
}

// RouteConfig describes how to call an external data source tool.
type RouteConfig struct {
	Method  string `json:"method"   bson:"method"`
	BaseURL string `json:"base_url" bson:"base_url"`
	Path    string `json:"path"     bson:"path"`
}

// EscalationEntry records a single escalation event in the session history.
type EscalationEntry struct {
	TriggeredAt  time.Time  `json:"triggered_at"  bson:"triggered_at"`
	Reason       string     `json:"reason"        bson:"reason"`
	OperatorNote string     `json:"operator_note" bson:"operator_note"`
	OperatorID   *string    `json:"operator_id"   bson:"operator_id"`
	ResolvedAt   *time.Time `json:"resolved_at"   bson:"resolved_at"`
}

// SessionRecord is the MongoDB document for a single conversation session.
type SessionRecord struct {
	ID             string            `bson:"_id"`
	TenantID       string            `bson:"tenant_id"`
	TenantSlug     string            `bson:"tenant_slug"`
	AgentProfileID string            `bson:"agent_profile_id"`
	AgentConfigID  string            `bson:"agent_config_id"`
	EndUserID      string            `bson:"end_user_id"`
	ChannelType    string            `bson:"channel_type"`
	ChannelKey     string            `bson:"channel_key"`
	State          SessionState      `bson:"state"`
	ContextEnvelope ContextEnvelope  `bson:"context_envelope"`
	Turns          []Turn            `bson:"turns"`
	EscalationLog  []EscalationEntry `bson:"escalation_log"`
	OpenedAt       time.Time         `bson:"opened_at"`
	ClosedAt       *time.Time        `bson:"closed_at"`
}
