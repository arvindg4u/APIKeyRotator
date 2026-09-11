package gemini

import (
	"encoding/json"
	"strings"
	"testing"

	"api-key-rotator/backend/internal/converters/formats"
)

func TestBuildRequestWithAudio(t *testing.T) {
	h := &Handler{}
	univ := &formats.UniversalRequest{
		Model: "gemini-3.5-flash-lite",
		Messages: []formats.UniversalMessage{
			{
				Role:    "user",
				Content: "Transcribe the speech exactly.",
				Audio:   &formats.UniversalAudio{Data: "QUJDRA==", Format: "wav"},
			},
		},
	}
	out, err := h.BuildRequest(univ)
	if err != nil {
		t.Fatalf("BuildRequest failed: %v", err)
	}
	var req Request
	if err := json.Unmarshal(out, &req); err != nil {
		t.Fatalf("re-unmarshal failed: %v", err)
	}
	if len(req.Contents) != 1 {
		t.Fatalf("expected 1 content, got %d", len(req.Contents))
	}
	parts := req.Contents[0].Parts
	if len(parts) != 2 {
		t.Fatalf("expected 2 parts (text + inlineData), got %d", len(parts))
	}
	if parts[0].Text != "Transcribe the speech exactly." {
		t.Fatalf("expected text part first, got %+v", parts[0])
	}
	if parts[1].InlineData == nil {
		t.Fatalf("expected inlineData part, got %+v", parts[1])
	}
	if parts[1].InlineData.MimeType != "audio/wav" {
		t.Fatalf("expected mime audio/wav, got %q", parts[1].InlineData.MimeType)
	}
	if parts[1].InlineData.Data != "QUJDRA==" {
		t.Fatalf("expected data passthrough, got %q", parts[1].InlineData.Data)
	}
}

func TestBuildRequestMimeLowercase(t *testing.T) {
	h := &Handler{}
	univ := &formats.UniversalRequest{
		Model: "gemini-3.5-flash-lite",
		Messages: []formats.UniversalMessage{
			{Role: "user", Audio: &formats.UniversalAudio{Data: "eA==", Format: "WAV"}},
		},
	}
	out, err := h.BuildRequest(univ)
	if err != nil {
		t.Fatalf("BuildRequest failed: %v", err)
	}
	var req Request
	if err := json.Unmarshal(out, &req); err != nil {
		t.Fatalf("re-unmarshal failed: %v", err)
	}
	got := req.Contents[0].Parts[0].InlineData
	if got == nil || got.MimeType != "audio/wav" {
		t.Fatalf("expected lowercase mime audio/wav, got %+v", got)
	}
}

func TestBuildRequestNoAudioTextOnly(t *testing.T) {
	h := &Handler{}
	univ := &formats.UniversalRequest{
		Model:    "gemini-3.5-flash-lite",
		Messages: []formats.UniversalMessage{{Role: "user", Content: "hello"}},
	}
	out, err := h.BuildRequest(univ)
	if err != nil {
		t.Fatalf("BuildRequest failed: %v", err)
	}
	if strings.Contains(string(out), "inlineData") {
		t.Fatalf("expected no inlineData for text-only message, got %s", string(out))
	}
}

func TestParseRequestExtractsInlineData(t *testing.T) {
	h := &Handler{}
	body := []byte(`{"contents":[{"role":"user","parts":[{"text":"Transcribe the speech exactly."},{"inlineData":{"mimeType":"audio/wav","data":"QUJDRA=="}}]}]}`)
	univ, err := h.ParseRequest(body)
	if err != nil {
		t.Fatalf("ParseRequest failed: %v", err)
	}
	if len(univ.Messages) != 1 {
		t.Fatalf("expected 1 message, got %d", len(univ.Messages))
	}
	msg := univ.Messages[0]
	if msg.Content != "Transcribe the speech exactly." {
		t.Fatalf("expected text content, got %q", msg.Content)
	}
	if msg.Audio == nil {
		t.Fatalf("expected audio extracted from inlineData, got nil")
	}
	if msg.Audio.Data != "QUJDRA==" {
		t.Fatalf("expected audio data %q, got %q", "QUJDRA==", msg.Audio.Data)
	}
	if msg.Audio.Format != "wav" {
		t.Fatalf("expected audio format wav (from mime audio/wav), got %q", msg.Audio.Format)
	}
}

func TestParseRequestTextOnlyNoAudio(t *testing.T) {
	h := &Handler{}
	body := []byte(`{"contents":[{"role":"user","parts":[{"text":"hello"}]}]}`)
	univ, err := h.ParseRequest(body)
	if err != nil {
		t.Fatalf("ParseRequest failed: %v", err)
	}
	if univ.Messages[0].Content != "hello" {
		t.Fatalf("expected content hello, got %q", univ.Messages[0].Content)
	}
	if univ.Messages[0].Audio != nil {
		t.Fatalf("expected no audio, got %+v", univ.Messages[0].Audio)
	}
}
