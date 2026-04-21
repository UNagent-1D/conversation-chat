package llm

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/UNagent-1D/conversation-chat/internal/domain"
	openai "github.com/sashabaranov/go-openai"
)

const structuredOutputInstructions = `
You MUST respond with a valid JSON object matching EXACTLY this schema. No extra fields, no markdown, no explanation outside the JSON.

{
  "action": "<none | tool_call | escalate | close_session>",
  "message": {
    "text": "<always required — the message shown to the user>",
    "escalation": {
      "reason": "<confused | angry | ask_for_human>",
      "operator_note": "<free text for the human operator — NOT shown to user>"
    },
    "tool": {
      "tool_name": "<tool name from the allowed list>",
      "parameters": {}
    }
  }
}

Rules:
- action = "none"          → message.escalation = null, message.tool = null
- action = "tool_call"     → message.tool = required, message.escalation = null
- action = "escalate"      → message.escalation = required, message.tool = null
- action = "close_session" → message.escalation = null, message.tool = null
- message.text is ALWAYS required regardless of action.
- message.escalation and message.tool are mutually exclusive.
`

// OpenAIClient implements LLMClient using the OpenAI Go SDK.
type OpenAIClient struct {
	client *openai.Client
}

// NewOpenAIClient creates a new OpenAIClient with the given API key and optional base URL.
// If baseURL is non-empty it overrides the default api.openai.com endpoint (e.g. for OpenRouter).
func NewOpenAIClient(apiKey, baseURL string) *OpenAIClient {
	cfg := openai.DefaultConfig(apiKey)
	if baseURL != "" {
		cfg.BaseURL = baseURL
	}
	return &OpenAIClient{client: openai.NewClientWithConfig(cfg)}
}

// Complete sends a completion request and parses the structured JSON response.
func (c *OpenAIClient) Complete(ctx context.Context, req CompletionRequest) (CompletionResponse, error) {
	messages := buildMessages(req)

	chatReq := openai.ChatCompletionRequest{
		Model:       req.Model,
		Temperature: float32(req.Temperature),
		MaxTokens:   req.MaxTokens,
		Messages:    messages,
		ResponseFormat: &openai.ChatCompletionResponseFormat{
			Type: openai.ChatCompletionResponseFormatTypeJSONObject,
		},
	}

	resp, err := c.client.CreateChatCompletion(ctx, chatReq)
	if err != nil {
		return CompletionResponse{}, fmt.Errorf("openai completion: %w", err)
	}

	if len(resp.Choices) == 0 {
		return CompletionResponse{}, fmt.Errorf("openai returned no choices")
	}

	rawContent := resp.Choices[0].Message.Content

	var llmResp domain.LLMResponse
	if err := json.Unmarshal([]byte(rawContent), &llmResp); err != nil {
		return CompletionResponse{RawContent: rawContent}, fmt.Errorf("parse llm json: %w", err)
	}

	return CompletionResponse{
		Response:     llmResp,
		RawContent:   rawContent,
		InputTokens:  resp.Usage.PromptTokens,
		OutputTokens: resp.Usage.CompletionTokens,
	}, nil
}

// buildMessages converts CompletionRequest into OpenAI chat messages.
// The system prompt includes the structured output schema instructions appended.
func buildMessages(req CompletionRequest) []openai.ChatCompletionMessage {
	systemContent := req.SystemPrompt + "\n\n" + structuredOutputInstructions

	msgs := []openai.ChatCompletionMessage{
		{Role: openai.ChatMessageRoleSystem, Content: systemContent},
	}

	for _, turn := range req.Messages {
		switch turn.Role {
		case domain.RoleUser:
			msgs = append(msgs, openai.ChatCompletionMessage{
				Role:    openai.ChatMessageRoleUser,
				Content: turn.Content,
			})
		case domain.RoleAssistant:
			msgs = append(msgs, openai.ChatCompletionMessage{
				Role:    openai.ChatMessageRoleAssistant,
				Content: turn.Content,
			})
		case domain.RoleTool:
			// Tool results are injected as assistant messages with a structured prefix
			resultStr := string(turn.Result)
			msgs = append(msgs, openai.ChatCompletionMessage{
				Role:    openai.ChatMessageRoleAssistant,
				Content: fmt.Sprintf("[Tool result for %s]: %s", turn.ToolName, resultStr),
			})
		}
	}

	return msgs
}
