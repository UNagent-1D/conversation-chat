package llm

import (
	"context"

	"github.com/UNagent-1D/conversation-chat/internal/domain"
)

// LLMClient is the abstraction over any LLM provider.
// The Chat service calls ONLY this interface — never the OpenAI SDK directly.
// This enables provider swap (Anthropic, Gemini, local model) without changing handler code.
type LLMClient interface {
	Complete(ctx context.Context, req CompletionRequest) (CompletionResponse, error)
}

// CompletionRequest carries everything the LLM needs for a single turn.
type CompletionRequest struct {
	Model        string
	Temperature  float64
	MaxTokens    int
	SystemPrompt string
	Messages     []domain.Turn // full history including the new user turn
	Tools        []ToolDef     // derived from agent_runtime.tool_permissions
}

// ToolDef is the OpenAI function definition forwarded to the LLM.
type ToolDef struct {
	Name        string
	Description string
	Parameters  any // OpenAI-compatible parameters schema
}

// CompletionResponse holds the parsed structured output from the LLM.
type CompletionResponse struct {
	Response    domain.LLMResponse
	RawContent  string // raw JSON string returned by the model (for logging)
	InputTokens int
	OutputTokens int
}
