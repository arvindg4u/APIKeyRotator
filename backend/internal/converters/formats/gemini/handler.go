package gemini

import (
	"encoding/json"
	"fmt"
	"strings"

	"api-key-rotator/backend/internal/converters/formats"
)

func init() {
	formats.RegisterFormat("gemini", &Handler{}, &StreamHandler{})
}

// Handler implements FormatHandler for Gemini format
type Handler struct{}

func (h *Handler) Name() string {
	return "gemini"
}

// Request types
type Request struct {
	Contents          []RequestContent  `json:"contents"`
	GenerationConfig  *GenerationConfig `json:"generationConfig,omitempty"`
	SystemInstruction *RequestContent   `json:"systemInstruction,omitempty"`
}

type RequestContent struct {
	Role  string `json:"role,omitempty"`
	Parts []Part `json:"parts"`
}

type Part struct {
	Text       string      `json:"text,omitempty"`
	InlineData *InlineData `json:"inlineData,omitempty"`
}

// InlineData carries base64-encoded media (e.g. audio) in a Gemini part.
type InlineData struct {
	MimeType string `json:"mimeType"`
	Data     string `json:"data"`
}

type GenerationConfig struct {
	MaxOutputTokens int      `json:"maxOutputTokens,omitempty"`
	Temperature     *float64 `json:"temperature,omitempty"`
	TopP            *float64 `json:"topP,omitempty"`
	StopSequences   []string `json:"stopSequences,omitempty"`
}

// Response types
type Response struct {
	Candidates    []Candidate    `json:"candidates"`
	UsageMetadata *UsageMetadata `json:"usageMetadata,omitempty"`
}

type Candidate struct {
	Content      *ResponseContent `json:"content"`
	FinishReason string           `json:"finishReason,omitempty"`
	Index        int              `json:"index"`
}

type ResponseContent struct {
	Parts []Part `json:"parts"`
	Role  string `json:"role"`
}

type UsageMetadata struct {
	PromptTokenCount     int `json:"promptTokenCount"`
	CandidatesTokenCount int `json:"candidatesTokenCount"`
	TotalTokenCount      int `json:"totalTokenCount"`
}

// ParseRequest implements FormatHandler
func (h *Handler) ParseRequest(body []byte) (*formats.UniversalRequest, error) {
	var req Request
	if err := json.Unmarshal(body, &req); err != nil {
		return nil, fmt.Errorf("failed to parse Gemini request: %w", err)
	}

	universal := &formats.UniversalRequest{
		Messages: make([]formats.UniversalMessage, 0),
	}

	if req.SystemInstruction != nil && len(req.SystemInstruction.Parts) > 0 {
		universal.System = req.SystemInstruction.Parts[0].Text
	}

	for _, content := range req.Contents {
		role := content.Role
		if role == "model" {
			role = "assistant"
		}
		var text string
		var audio *formats.UniversalAudio
		for _, part := range content.Parts {
			text += part.Text
			if part.InlineData != nil && audio == nil {
				audio = &formats.UniversalAudio{
					Data:   part.InlineData.Data,
					Format: mimeToAudioFormat(part.InlineData.MimeType),
				}
			}
		}
		universal.Messages = append(universal.Messages, formats.UniversalMessage{
			Role:    role,
			Content: text,
			Audio:   audio,
		})
	}

	if req.GenerationConfig != nil {
		universal.MaxTokens = req.GenerationConfig.MaxOutputTokens
		universal.Temperature = req.GenerationConfig.Temperature
		universal.TopP = req.GenerationConfig.TopP
		universal.Stop = req.GenerationConfig.StopSequences
	}

	return universal, nil
}

// BuildRequest implements FormatHandler
func (h *Handler) BuildRequest(req *formats.UniversalRequest) ([]byte, error) {
	geminiReq := Request{
		Contents: make([]RequestContent, 0),
	}

	if req.System != "" {
		geminiReq.SystemInstruction = &RequestContent{
			Parts: []Part{{Text: req.System}},
		}
	}

	for _, msg := range req.Messages {
		role := msg.Role
		if role == "assistant" {
			role = "model"
		}
		parts := make([]Part, 0, 2)
		if msg.Audio == nil {
			// Text-only: preserve the historical single-text-part shape.
			parts = append(parts, Part{Text: msg.Content})
		} else {
			if msg.Content != "" {
				parts = append(parts, Part{Text: msg.Content})
			}
			parts = append(parts, Part{InlineData: &InlineData{
				MimeType: "audio/" + strings.ToLower(msg.Audio.Format),
				Data:     msg.Audio.Data,
			}})
		}
		geminiReq.Contents = append(geminiReq.Contents, RequestContent{
			Role:  role,
			Parts: parts,
		})
	}

	geminiReq.GenerationConfig = &GenerationConfig{
		MaxOutputTokens: req.MaxTokens,
		Temperature:     req.Temperature,
		TopP:            req.TopP,
		StopSequences:   req.Stop,
	}

	return json.Marshal(geminiReq)
}

// ParseResponse implements FormatHandler
func (h *Handler) ParseResponse(body []byte) (*formats.UniversalResponse, error) {
	var resp Response
	if err := json.Unmarshal(body, &resp); err != nil {
		return nil, fmt.Errorf("failed to parse Gemini response: %w", err)
	}

	universal := &formats.UniversalResponse{Role: "assistant"}

	if len(resp.Candidates) > 0 {
		candidate := resp.Candidates[0]
		if candidate.Content != nil {
			for _, part := range candidate.Content.Parts {
				universal.Content += part.Text
			}
			universal.Role = candidate.Content.Role
			if universal.Role == "model" {
				universal.Role = "assistant"
			}
		}
		universal.StopReason = mapGeminiFinishReason(candidate.FinishReason)
	}

	if resp.UsageMetadata != nil {
		universal.Usage = &formats.UniversalUsage{
			InputTokens:  resp.UsageMetadata.PromptTokenCount,
			OutputTokens: resp.UsageMetadata.CandidatesTokenCount,
			TotalTokens:  resp.UsageMetadata.TotalTokenCount,
		}
	}

	return universal, nil
}

// BuildResponse implements FormatHandler
func (h *Handler) BuildResponse(resp *formats.UniversalResponse) ([]byte, error) {
	role := resp.Role
	if role == "assistant" {
		role = "model"
	}

	geminiResp := Response{
		Candidates: []Candidate{
			{
				Index:        0,
				FinishReason: mapToGeminiFinishReason(resp.StopReason),
				Content: &ResponseContent{
					Role:  role,
					Parts: []Part{{Text: resp.Content}},
				},
			},
		},
	}

	if resp.Usage != nil {
		geminiResp.UsageMetadata = &UsageMetadata{
			PromptTokenCount:     resp.Usage.InputTokens,
			CandidatesTokenCount: resp.Usage.OutputTokens,
			TotalTokenCount:      resp.Usage.TotalTokens,
		}
	}

	return json.Marshal(geminiResp)
}

// GetAPIPath implements FormatHandler
func (h *Handler) GetAPIPath(action string) string {
	switch action {
	case "chat/completions", "v1/chat/completions":
		return "v1beta/models/{model}:generateContent"
	default:
		return action
	}
}

// GetClientAction implements FormatHandler
func (h *Handler) GetClientAction(apiPath string) string {
	return "chat/completions"
}

// mimeToAudioFormat maps a mime type like "audio/wav" back to the short
// audio format ("wav"). Best-effort: unknown shapes pass through lowercased.
func mimeToAudioFormat(mime string) string {
	mime = strings.ToLower(strings.TrimSpace(mime))
	if rest, ok := strings.CutPrefix(mime, "audio/"); ok {
		if i := strings.IndexByte(rest, ';'); i >= 0 {
			rest = rest[:i]
		}
		if rest != "" {
			return rest
		}
	}
	return mime
}

func mapGeminiFinishReason(reason string) string {
	switch reason {
	case "STOP":
		return "stop"
	case "MAX_TOKENS":
		return "length"
	case "SAFETY":
		return "content_filter"
	default:
		return "stop"
	}
}

func mapToGeminiFinishReason(reason string) string {
	switch reason {
	case "stop", "end_turn":
		return "STOP"
	case "length", "max_tokens":
		return "MAX_TOKENS"
	case "content_filter":
		return "SAFETY"
	default:
		return "STOP"
	}
}
