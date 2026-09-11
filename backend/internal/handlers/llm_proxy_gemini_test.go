package handlers

import (
	"strings"
	"testing"

	"api-key-rotator/backend/internal/converters"
)

// TestOpenAIClientAgainstGeminiUpstream covers the client-side flow used when a service is
// configured with api_format "gemini_native" and client format "openai" (OpenAI SDK):
//
//	client -> OpenAI chat/completions -> convert -> Gemini generateContent -> upstream
//	upstream -> Gemini response -> convert -> OpenAI chat.completion -> client
func TestOpenAIClientAgainstGeminiUpstream(t *testing.T) {
	const (
		clientFormat = "openai"
		apiFormat    = "gemini_native"
	)

	if !converters.NeedsConversion(clientFormat, apiFormat) {
		t.Fatalf("NeedsConversion(%q, %q) = false, want true", clientFormat, apiFormat)
	}

	reqConverter, err := converters.NewConverter(clientFormat, apiFormat)
	if err != nil {
		t.Fatalf("NewConverter(%q, %q): %v", clientFormat, apiFormat, err)
	}

	// The OpenAI action path must map to the Gemini generateContent action with a {model} placeholder.
	action := reqConverter.GetTargetPath("v1/chat/completions")
	if !strings.Contains(action, "{model}") || !strings.Contains(action, ":generateContent") {
		t.Fatalf("GetTargetPath(\"v1/chat/completions\") = %q, want a Gemini generateContent path with {model}", action)
	}

	// prepareLLMRequest extracts model and stream from the *original* client body, because the
	// converted Gemini body no longer carries a "model" field.
	openaiBody := []byte(`{"model":"gemini-2.5-flash","stream":true,"messages":[{"role":"user","content":"hi"}]}`)
	model := extractModelFromBody(openaiBody)
	stream := extractStreamFromBody(openaiBody)
	if model != "gemini-2.5-flash" {
		t.Fatalf("extractModelFromBody() = %q, want %q", model, "gemini-2.5-flash")
	}
	if !stream {
		t.Fatalf("extractStreamFromBody() = false, want true")
	}

	convertedAction := strings.ReplaceAll(action, "{model}", model)
	if stream && strings.Contains(convertedAction, ":generateContent") {
		convertedAction = strings.ReplaceAll(convertedAction, ":generateContent", ":streamGenerateContent")
	}
	if want := "v1beta/models/gemini-2.5-flash:streamGenerateContent"; convertedAction != want {
		t.Fatalf("converted action = %q, want %q", convertedAction, want)
	}

	convertedBody, err := reqConverter.ConvertRequest(openaiBody)
	if err != nil {
		t.Fatalf("ConvertRequest: %v", err)
	}
	if !strings.Contains(string(convertedBody), `"contents"`) {
		t.Fatalf("converted request body = %s, want Gemini contents", convertedBody)
	}

	// Responses travel the other direction: upstream Gemini -> OpenAI client.
	respConverter, err := converters.NewConverter(apiFormat, clientFormat)
	if err != nil {
		t.Fatalf("NewConverter(%q, %q): %v", apiFormat, clientFormat, err)
	}

	geminiResponse := []byte(`{"candidates":[{"content":{"parts":[{"text":"Hello there"}],"role":"model"},"finishReason":"STOP"}],"usageMetadata":{"promptTokenCount":3,"candidatesTokenCount":2,"totalTokenCount":5}}`)
	openaiResponse, err := respConverter.ConvertResponse(geminiResponse)
	if err != nil {
		t.Fatalf("ConvertResponse: %v", err)
	}
	if !strings.Contains(string(openaiResponse), `"choices"`) || !strings.Contains(string(openaiResponse), "Hello there") {
		t.Fatalf("converted response = %s, want an OpenAI chat.completion with content", openaiResponse)
	}

	// Stream end events must be produced in the client format, so an OpenAI SDK sees "data: [DONE]".
	endEvents := respConverter.GetStreamEndEvents()
	if len(endEvents) == 0 || !strings.Contains(string(endEvents[len(endEvents)-1]), "[DONE]") {
		t.Fatalf("GetStreamEndEvents() = %q, want an OpenAI [DONE] event", endEvents)
	}

	geminiChunk := []byte(`{"candidates":[{"content":{"parts":[{"text":"Hel"}],"role":"model"}}]}`)
	convertedChunk, err := respConverter.ConvertStreamChunk(geminiChunk)
	if err != nil {
		t.Fatalf("ConvertStreamChunk: %v", err)
	}
	if !strings.Contains(string(convertedChunk), "Hel") || !strings.Contains(string(convertedChunk), "chat.completion.chunk") {
		t.Fatalf("converted stream chunk = %s, want an OpenAI chat.completion.chunk carrying the delta", convertedChunk)
	}
}
