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

CRITICAL — tool-result handling:
- If the most recent assistant message in the history starts with "[Tool result for X]:",
  it contains the data the user is waiting for. Your next response MUST have
  action = "none" and message.text MUST answer the user using that data.
  DO NOT call the same tool again. DO NOT call any tool unless the user asks
  for something the previous result does not contain.
- When action = "tool_call", keep message.text very brief (e.g. "Un momento,
  estoy consultando…") — DO NOT pretend you already have the answer.

CRITICAL — schema shape:
- "tool" and "escalation" are NESTED INSIDE "message". Never put them at the
  root level. The correct shape is:
      {"action":"tool_call","message":{"text":"…","tool":{"tool_name":"…","parameters":{}}}}
  NOT:
      {"action":"tool_call","message":{"text":"…"},"tool":{...}}

Booking flow rules (hospital domain):
- NEVER answer with "Lo siento, no puedo ayudar" for any health-related
  request. If you're unsure what the user wants, call list_doctors first to
  see what specialties / locations the catalog has, then present that list
  and ask the user to choose.
- If the user names a specialty:
  1. Call list_doctors with area set to that specialty.
  2. If the result has 0 doctors, call list_doctors with no filter next, and
     tell the user: "No tenemos especialistas en <X>. Las especialidades
     disponibles son: <list the distinct areas from the no-filter result>.
     ¿Cuál te interesa?"
  3. If the result has ≥1 doctors, present them by name + area + location.
- Slot selection — ALWAYS use only what get_doctor_schedule returned:
  1. Call get_doctor_schedule(doctor_id) after the user picks a doctor.
  2. The response only contains AVAILABLE slots — booked ones are filtered
     out server-side. Present 3-5 of the nearest slots, grouped by day, in
     user-friendly Spanish ("lunes 1 de junio a las 9:00").
  3. If the user picks a time NOT in the returned list, DO NOT call
     book_appointment. Politely note the slot is not available and
     re-present the closest 3-5 available slots.
- Before book_appointment, verify in your conversation history that you
  have: doctor_id, patient_ref, patient_name, slot_start (ISO 8601). If any
  are missing, ask the user for just the missing one(s) — DO NOT re-ask for
  fields the user already provided earlier in the conversation.
- If book_appointment returns error: true with status 409 (conflict), the
  slot was taken between schedule lookup and booking. Apologize briefly,
  re-call get_doctor_schedule, present the new top 3-5 slots, and ask the
  user to pick again. DO NOT escalate.
- Only escalate (action="escalate") when the user EXPLICITLY asks for a
  human ("quiero hablar con alguien", "operador", "agente humano",
  "persona real"). Asking the user a clarifying question is NEVER an
  escalation — use action="none" with the question in message.text.
  DO NOT pick reason="ask_for_human" just because the user was vague or
  you don't know what to do — that label is reserved for explicit human
  requests. Catalog gaps, conflict 409s, missing-info questions, and
  off-topic-but-still-health questions are NOT escalation triggers.

Cancel flow rules:
- When the user wants to cancel an appointment, you usually only know they
  want to cancel — you need to find WHICH appointment. The canonical
  sequence is:
  1. If you don't already have patient_ref in this conversation, ask the
     user for their número de identificación (cédula).
  2. Call get_patient_appointments(patient_ref) with status="confirmed"
     (or omit status if user wants to see all). The result is a list of
     appointments with id, doctor_id, slot_start, area.
  3. If the list is empty, tell the user: "No encontré citas activas para
     ese número de identificación." Do NOT escalate — offer to look up a
     different number.
  4. If the list has 1 entry, confirm briefly: "Encontré tu cita con
     <doctor> el <date>. ¿Confirmas que quieres cancelarla?" and wait for
     yes/no before calling cancel_appointment.
  5. If the list has ≥2 entries, present them numbered ("1. Dr. X el día Y,
     2. Dr. Z el día W") and ask "¿Cuál quieres cancelar?".
- ALWAYS confirm with the user before calling cancel_appointment — cancels
  are destructive. action="none" + a yes/no question is the right shape.
- After cancel_appointment succeeds, briefly summarize what was cancelled.

Reschedule flow rules:
- Reschedule is two-step in the user's head (find the existing appointment,
  pick a new slot) but ONE atomic call (reschedule_appointment handles the
  cancel + book transactionally and reverts if the new slot is taken).
- Canonical sequence:
  1. If patient_ref unknown, ask for it.
  2. Call get_patient_appointments(patient_ref) and let the user pick which
     appointment to reschedule (same 0 / 1 / ≥2 handling as Cancel flow).
  3. Once you know the appointment_id, call get_doctor_schedule for that
     appointment's doctor to learn what slots are available.
  4. Present 3–5 nearest available slots; if the user named a time, only
     accept it if it's in the returned list (same rule as booking).
  5. Confirm before calling reschedule_appointment ("¿Confirmas mover tu
     cita del <old> al <new>?").
  6. Call reschedule_appointment with appointment_id + new_slot_start. If
     the response is error: true with status 409, the new slot got taken
     in-between — re-fetch the schedule and re-present.
- The user can also reschedule to a DIFFERENT doctor: pass new_doctor_id
  alongside new_slot_start. Same available-slots rule applies to that
  doctor's schedule.
- Never escalate on a conflict. Re-fetch and re-offer.
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

	// Weaker models (deepseek-v4-flash in particular) sometimes flatten the
	// schema and put `tool` / `escalation` at the root instead of inside
	// `message`. Hoist them in before strict unmarshal so a recoverable
	// formatting slip doesn't cost a retry round-trip.
	normalized := normalizeLLMJSON(rawContent)

	var llmResp domain.LLMResponse
	if err := json.Unmarshal([]byte(normalized), &llmResp); err != nil {
		return CompletionResponse{RawContent: rawContent}, fmt.Errorf("parse llm json: %w", err)
	}

	return CompletionResponse{
		Response:     llmResp,
		RawContent:   rawContent,
		InputTokens:  resp.Usage.PromptTokens,
		OutputTokens: resp.Usage.CompletionTokens,
	}, nil
}

// normalizeLLMJSON moves root-level `tool` and `escalation` keys into
// `message` when the LLM hoists them out (a common deepseek mistake).
// Returns the original string if anything in this normalization fails — the
// strict unmarshal then surfaces the original error and the retry loop
// kicks in.
func normalizeLLMJSON(raw string) string {
	var generic map[string]any
	if err := json.Unmarshal([]byte(raw), &generic); err != nil {
		return raw
	}
	msg, ok := generic["message"].(map[string]any)
	if !ok {
		return raw
	}
	moved := false
	if t, ok := generic["tool"]; ok {
		if _, present := msg["tool"]; !present {
			msg["tool"] = t
			moved = true
		}
		delete(generic, "tool")
	}
	if e, ok := generic["escalation"]; ok {
		if _, present := msg["escalation"]; !present {
			msg["escalation"] = e
			moved = true
		}
		delete(generic, "escalation")
	}
	if !moved {
		return raw
	}
	generic["message"] = msg
	out, err := json.Marshal(generic)
	if err != nil {
		return raw
	}
	return string(out)
}

// buildMessages converts CompletionRequest into OpenAI chat messages.
// The system prompt includes the structured output schema instructions appended.
func buildMessages(req CompletionRequest) []openai.ChatCompletionMessage {
	systemContent := req.SystemPrompt + "\n\n" + structuredOutputInstructions
	if len(req.Tools) > 0 {
		// Enumerate the actual tool names so the LLM stops hallucinating
		// names (e.g. inventing `schedule_appointment` when the real name
		// is `book_appointment`).
		systemContent += "\n\nAllowed tool names (use EXACTLY one of these as tool.tool_name):\n"
		for _, t := range req.Tools {
			systemContent += "  - " + t.Name + "\n"
		}
		// Hospital tool parameter contract. Hardcoded here because the
		// per-tenant config carries only names + permissions today; without
		// these the LLM guesses param names (e.g. `date` instead of
		// `slot_start`) and the tool call 400s. Keep this in sync with
		// chat-orch/src/hospital.rs::tool_definitions().
		systemContent += `
Tool parameter contract (use EXACT field names):
  list_doctors:             {area?: string, place?: string}
  get_doctor_schedule:      {doctor_id: string (required), days_ahead?: integer}
  book_appointment:         {doctor_id: string (required),
                             patient_ref: string (required — the patient's
                                          identification number / cédula,
                                          e.g. "1055358258"),
                             patient_name: string (required, full name),
                             slot_start: string (required, ISO 8601 like "2026-06-01T09:00:00"),
                             specialty?: string}
  cancel_appointment:       {appointment_id: string (required), reason?: string}
  reschedule_appointment:   {appointment_id: string (required),
                             new_slot_start: string (required, ISO 8601),
                             new_doctor_id?: string, reason?: string}
  get_patient_appointments: {patient_ref: string (required), status?: string}

Required-parameter rule:
- BEFORE calling a tool, check you have ALL required params. If any are missing
  (especially patient_ref + patient_name for book_appointment), DO NOT call the
  tool. Instead, set action="none" and ask the user for the missing values in
  message.text. Only call the tool once every required field is known.

User-facing wording for patient_ref:
- When asking the user for patient_ref, refer to it as "número de
  identificación" (or "número de documento" / "cédula") in Spanish, or
  "identification number" in English. NEVER use the literal phrase
  "referencia del paciente" — that's the internal field name and confuses
  users. The value the user types (e.g. "1055358258" or "CC 1055358258")
  goes verbatim into the patient_ref parameter.
`
	}

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
