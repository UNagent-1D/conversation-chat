package domain

import (
	"encoding/json"
	"time"
)

// TurnRole identifies who produced a given turn in the conversation.
type TurnRole string

const (
	RoleUser      TurnRole = "user"
	RoleAssistant TurnRole = "assistant"
	RoleTool      TurnRole = "tool"
)

// Turn is a single message in the conversation history, stored in both Redis and MongoDB.
type Turn struct {
	Role       TurnRole        `json:"role"                 bson:"role"`
	Content    string          `json:"content"              bson:"content"`
	ToolName   string          `json:"tool_name,omitempty"  bson:"tool_name,omitempty"`
	Result     json.RawMessage `json:"result,omitempty"     bson:"result,omitempty"`
	ChannelKey string          `json:"channel_key,omitempty" bson:"channel_key,omitempty"`
	MessageID  string          `json:"message_id,omitempty"  bson:"message_id,omitempty"`
	Ts         time.Time       `json:"ts"                   bson:"ts"`
}

// LLMResponse is the enforced structured output from every LLM call.
// action is the fast-parse signal; message carries the full detail.
type LLMResponse struct {
	Action  string      `json:"action"`  // none | tool_call | escalate | close_session
	Message MessageBody `json:"message"`
}

// MessageBody holds the response content. Escalation and Tool are mutually exclusive.
type MessageBody struct {
	Text       string          `json:"text"`
	Escalation *EscalationInfo `json:"escalation,omitempty"`
	Tool       *ToolCall       `json:"tool,omitempty"`
}

// EscalationInfo carries the trigger reason and a free-text note for the human operator.
type EscalationInfo struct {
	Reason       string `json:"reason"`        // confused | angry | ask_for_human
	OperatorNote string `json:"operator_note"` // shown on operator dashboard, never to end-user
}

// ToolCall describes which tool the LLM wants to invoke and with what parameters.
type ToolCall struct {
	ToolName   string          `json:"tool_name"`
	Parameters json.RawMessage `json:"parameters"`
}

// ValidActions is the closed set of allowed action values.
var ValidActions = map[string]bool{
	"none":          true,
	"tool_call":     true,
	"escalate":      true,
	"close_session": true,
}

// ValidEscalationReasons is the closed set of allowed escalation reason values.
var ValidEscalationReasons = map[string]bool{
	"confused":      true,
	"angry":         true,
	"ask_for_human": true,
}

// Validate checks that the action field is consistent with the message sub-fields
// as required by spec section 4.3.
func (r *LLMResponse) Validate() error {
	if !ValidActions[r.Action] {
		return &ValidationError{Field: "action", Message: "unknown action: " + r.Action}
	}
	switch r.Action {
	case "none", "close_session":
		if r.Message.Escalation != nil || r.Message.Tool != nil {
			return &ValidationError{Field: "message", Message: "action " + r.Action + " must have null escalation and tool"}
		}
	case "tool_call":
		if r.Message.Tool == nil {
			return &ValidationError{Field: "message.tool", Message: "action tool_call requires message.tool"}
		}
		if r.Message.Escalation != nil {
			return &ValidationError{Field: "message.escalation", Message: "action tool_call must have null escalation"}
		}
	case "escalate":
		if r.Message.Escalation == nil {
			return &ValidationError{Field: "message.escalation", Message: "action escalate requires message.escalation"}
		}
		if !ValidEscalationReasons[r.Message.Escalation.Reason] {
			return &ValidationError{Field: "message.escalation.reason", Message: "unknown reason: " + r.Message.Escalation.Reason}
		}
		if r.Message.Tool != nil {
			return &ValidationError{Field: "message.tool", Message: "action escalate must have null tool"}
		}
	}
	if r.Message.Text == "" {
		return &ValidationError{Field: "message.text", Message: "message.text is always required"}
	}
	return nil
}

// ValidationError is returned when the LLM response fails consistency checks.
type ValidationError struct {
	Field   string
	Message string
}

func (e *ValidationError) Error() string {
	return "llm response validation: " + e.Field + ": " + e.Message
}
