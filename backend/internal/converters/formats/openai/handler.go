package openai

import (
	"encoding/json"
	"fmt"
	"time"

	"api-key-rotator/backend/internal/converters/formats"
)

func init() {
	formats.RegisterFormat("openai", &Handler{}, &StreamHandler{})
}

// Handler implements FormatHandler for OpenAI format
type Handler struct{}

func (h *Handler) Name() string {
	return "openai"
}

// Request types
type Request struct {
	Model       string           `json:"model"`
	Messages    []RequestMessage `json:"messages"`
	MaxTokens   *int             `json:"max_tokens,omitempty"`
	Temperature *float64         `json:"temperature,omitempty"`
	TopP        *float64         `json:"top_p,omitempty"`
	Stream      bool             `json:"stream,omitempty"`
	Stop        []string         `json:"stop,omitempty"`
}

type RequestMessage struct {
	Role    string          `json:"role"`
	Content json.RawMessage `json:"content"`
}

// contentPart is one element of an array-shaped message content.
type contentPart struct {
	Type       string `json:"type"`
	Text       string `json:"text,omitempty"`
	InputAudio *struct {
		Data   string `json:"data"`
		Format string `json:"format"`
	} `json:"input_audio,omitempty"`
}

// parseContent decodes a message content that may be either a plain string
// or an array of text/input_audio parts (OpenAI chat completion shape).
// It returns the concatenated text and the first input_audio payload, if any.
func parseContent(raw json.RawMessage) (string, *formats.UniversalAudio, error) {
	if len(raw) == 0 || string(raw) == "null" {
		return "", nil, nil
	}
	var s string
	if err := json.Unmarshal(raw, &s); err == nil {
		return s, nil, nil
	}
	var parts []contentPart
	if err := json.Unmarshal(raw, &parts); err != nil {
		return "", nil, fmt.Errorf("message content must be a string or content-part array: %w", err)
	}
	var text string
	var audio *formats.UniversalAudio
	for _, p := range parts {
		switch p.Type {
		case "text":
			text += p.Text
		case "input_audio":
			if p.InputAudio != nil && audio == nil {
				audio = &formats.UniversalAudio{
					Data:   p.InputAudio.Data,
					Format: p.InputAudio.Format,
				}
			}
		}
	}
	return text, audio, nil
}

// buildContent encodes text (+ optional audio) back into OpenAI content
// shape: plain string when there is no audio (backward compat), otherwise
// an array of [text?, input_audio] parts.
func buildContent(text string, audio *formats.UniversalAudio) (json.RawMessage, error) {
	if audio == nil {
		return json.Marshal(text)
	}
	parts := make([]contentPart, 0, 2)
	if text != "" {
		parts = append(parts, contentPart{Type: "text", Text: text})
	}
	parts = append(parts, contentPart{
		Type: "input_audio",
		InputAudio: &struct {
			Data   string `json:"data"`
			Format string `json:"format"`
		}{Data: audio.Data, Format: audio.Format},
	})
	return json.Marshal(parts)
}

// Response types
type Response struct {
	ID      string   `json:"id"`
	Object  string   `json:"object"`
	Created int64    `json:"created"`
	Model   string   `json:"model"`
	Choices []Choice `json:"choices"`
	Usage   *Usage   `json:"usage,omitempty"`
}

type Choice struct {
	Index        int      `json:"index"`
	Message      *Message `json:"message,omitempty"`
	FinishReason *string  `json:"finish_reason,omitempty"`
}

type Message struct {
	Role    string `json:"role"`
	Content string `json:"content"`
}

type Usage struct {
	PromptTokens     int `json:"prompt_tokens"`
	CompletionTokens int `json:"completion_tokens"`
	TotalTokens      int `json:"total_tokens"`
}

// ParseRequest implements FormatHandler
func (h *Handler) ParseRequest(body []byte) (*formats.UniversalRequest, error) {
	var req Request
	if err := json.Unmarshal(body, &req); err != nil {
		return nil, fmt.Errorf("failed to parse OpenAI request: %w", err)
	}

	universal := &formats.UniversalRequest{
		Model:       req.Model,
		Messages:    make([]formats.UniversalMessage, 0),
		Stream:      req.Stream,
		Stop:        req.Stop,
		Temperature: req.Temperature,
		TopP:        req.TopP,
	}

	if req.MaxTokens != nil {
		universal.MaxTokens = *req.MaxTokens
	}

	for _, msg := range req.Messages {
		text, audio, err := parseContent(msg.Content)
		if err != nil {
			return nil, fmt.Errorf("failed to parse OpenAI message content: %w", err)
		}
		if msg.Role == "system" {
			universal.System = text
		} else {
			universal.Messages = append(universal.Messages, formats.UniversalMessage{
				Role:    msg.Role,
				Content: text,
				Audio:   audio,
			})
		}
	}

	return universal, nil
}

// BuildRequest implements FormatHandler
func (h *Handler) BuildRequest(req *formats.UniversalRequest) ([]byte, error) {
	openaiReq := Request{
		Model:       req.Model,
		Messages:    make([]RequestMessage, 0),
		Stream:      req.Stream,
		Stop:        req.Stop,
		Temperature: req.Temperature,
		TopP:        req.TopP,
	}

	if req.MaxTokens > 0 {
		openaiReq.MaxTokens = &req.MaxTokens
	}

	if req.System != "" {
		sysContent, err := buildContent(req.System, nil)
		if err != nil {
			return nil, fmt.Errorf("failed to build OpenAI system content: %w", err)
		}
		openaiReq.Messages = append(openaiReq.Messages, RequestMessage{
			Role:    "system",
			Content: sysContent,
		})
	}

	for _, msg := range req.Messages {
		content, err := buildContent(msg.Content, msg.Audio)
		if err != nil {
			return nil, fmt.Errorf("failed to build OpenAI message content: %w", err)
		}
		openaiReq.Messages = append(openaiReq.Messages, RequestMessage{
			Role:    msg.Role,
			Content: content,
		})
	}

	return json.Marshal(openaiReq)
}

// ParseResponse implements FormatHandler
func (h *Handler) ParseResponse(body []byte) (*formats.UniversalResponse, error) {
	var resp Response
	if err := json.Unmarshal(body, &resp); err != nil {
		return nil, fmt.Errorf("failed to parse OpenAI response: %w", err)
	}

	universal := &formats.UniversalResponse{
		ID:    resp.ID,
		Model: resp.Model,
		Role:  "assistant",
	}

	if len(resp.Choices) > 0 {
		choice := resp.Choices[0]
		if choice.Message != nil {
			universal.Content = choice.Message.Content
			universal.Role = choice.Message.Role
		}
		if choice.FinishReason != nil {
			universal.StopReason = *choice.FinishReason
		}
	}

	if resp.Usage != nil {
		universal.Usage = &formats.UniversalUsage{
			InputTokens:  resp.Usage.PromptTokens,
			OutputTokens: resp.Usage.CompletionTokens,
			TotalTokens:  resp.Usage.TotalTokens,
		}
	}

	return universal, nil
}

// BuildResponse implements FormatHandler
func (h *Handler) BuildResponse(resp *formats.UniversalResponse) ([]byte, error) {
	finishReason := resp.StopReason
	if finishReason == "" {
		finishReason = "stop"
	}

	openaiResp := Response{
		ID:      resp.ID,
		Object:  "chat.completion",
		Created: time.Now().Unix(),
		Model:   resp.Model,
		Choices: []Choice{
			{
				Index: 0,
				Message: &Message{
					Role:    resp.Role,
					Content: resp.Content,
				},
				FinishReason: &finishReason,
			},
		},
	}

	if resp.Usage != nil {
		openaiResp.Usage = &Usage{
			PromptTokens:     resp.Usage.InputTokens,
			CompletionTokens: resp.Usage.OutputTokens,
			TotalTokens:      resp.Usage.TotalTokens,
		}
	}

	return json.Marshal(openaiResp)
}

// GetAPIPath implements FormatHandler - converts client action to OpenAI API path
func (h *Handler) GetAPIPath(action string) string {
	switch action {
	case "v1/messages", "messages":
		// Anthropic endpoint -> OpenAI endpoint
		return "chat/completions"
	case "v1/responses", "responses":
		// Copilot/Codex endpoint -> OpenAI endpoint
		return "chat/completions"
	case "v1/chat/completions":
		return "chat/completions"
	default:
		return action
	}
}

// GetClientAction implements FormatHandler - converts API path to client action
func (h *Handler) GetClientAction(apiPath string) string {
	switch apiPath {
	case "chat/completions", "v1/chat/completions":
		return "v1/chat/completions"
	default:
		return apiPath
	}
}
