package openai

import (
	"encoding/json"
	"strings"
	"testing"
)

func TestParseRequestStringContent(t *testing.T) {
	body := []byte(`{"model":"gpt-4","messages":[{"role":"user","content":"hello"}]}`)
	h := &Handler{}
	univ, err := h.ParseRequest(body)
	if err != nil {
		t.Fatalf("ParseRequest failed: %v", err)
	}
	if len(univ.Messages) != 1 {
		t.Fatalf("expected 1 message, got %d", len(univ.Messages))
	}
	if univ.Messages[0].Content != "hello" {
		t.Fatalf("expected content %q, got %q", "hello", univ.Messages[0].Content)
	}
	if univ.Messages[0].Audio != nil {
		t.Fatalf("expected no audio, got %+v", univ.Messages[0].Audio)
	}
}

func TestParseRequestArrayContentWithAudio(t *testing.T) {
	body := []byte(`{"model":"gpt-audio-1.5","messages":[{"role":"user","content":[{"type":"text","text":"Transcribe the speech exactly."},{"type":"input_audio","input_audio":{"data":"QUJDRA==","format":"wav"}}]}]}`)
	h := &Handler{}
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
		t.Fatalf("expected audio to be parsed, got nil")
	}
	if msg.Audio.Data != "QUJDRA==" {
		t.Fatalf("expected audio data %q, got %q", "QUJDRA==", msg.Audio.Data)
	}
	if msg.Audio.Format != "wav" {
		t.Fatalf("expected audio format %q, got %q", "wav", msg.Audio.Format)
	}
}

func TestParseRequestArrayContentTextOnly(t *testing.T) {
	body := []byte(`{"model":"gpt-4","messages":[{"role":"user","content":[{"type":"text","text":"hi "},{"type":"text","text":"there"}]}]}`)
	h := &Handler{}
	univ, err := h.ParseRequest(body)
	if err != nil {
		t.Fatalf("ParseRequest failed: %v", err)
	}
	if univ.Messages[0].Content != "hi there" {
		t.Fatalf("expected concatenated text, got %q", univ.Messages[0].Content)
	}
	if univ.Messages[0].Audio != nil {
		t.Fatalf("expected no audio, got %+v", univ.Messages[0].Audio)
	}
}

func TestParseRequestInvalidContentShape(t *testing.T) {
	body := []byte(`{"model":"gpt-4","messages":[{"role":"user","content":42}]}`)
	h := &Handler{}
	if _, err := h.ParseRequest(body); err == nil {
		t.Fatalf("expected error for numeric content, got nil")
	}
}

func TestParseRequestSystemArrayContent(t *testing.T) {
	body := []byte(`{"model":"gpt-4","messages":[{"role":"system","content":[{"type":"text","text":"sys prompt"}]},{"role":"user","content":"hi"}]}`)
	h := &Handler{}
	univ, err := h.ParseRequest(body)
	if err != nil {
		t.Fatalf("ParseRequest failed: %v", err)
	}
	if univ.System != "sys prompt" {
		t.Fatalf("expected system %q, got %q", "sys prompt", univ.System)
	}
}

func TestBuildRequestRoundTripsAudio(t *testing.T) {
	h := &Handler{}
	body := []byte(`{"model":"gpt-audio-1.5","messages":[{"role":"user","content":[{"type":"text","text":"Transcribe the speech exactly."},{"type":"input_audio","input_audio":{"data":"QUJDRA==","format":"wav"}}]}]}`)
	univ, err := h.ParseRequest(body)
	if err != nil {
		t.Fatalf("ParseRequest failed: %v", err)
	}
	out, err := h.BuildRequest(univ)
	if err != nil {
		t.Fatalf("BuildRequest failed: %v", err)
	}
	var req Request
	if err := json.Unmarshal(out, &req); err != nil {
		t.Fatalf("re-unmarshal failed: %v", err)
	}
	var parts []map[string]interface{}
	if err := json.Unmarshal(req.Messages[0].Content, &parts); err != nil {
		t.Fatalf("expected array content shape, got %s: %v", string(req.Messages[0].Content), err)
	}
	if len(parts) != 2 {
		t.Fatalf("expected 2 parts, got %d", len(parts))
	}
	audioPart := parts[1]
	if audioPart["type"] != "input_audio" {
		t.Fatalf("expected input_audio part, got %v", audioPart["type"])
	}
	ia, ok := audioPart["input_audio"].(map[string]interface{})
	if !ok {
		t.Fatalf("missing input_audio object: %v", audioPart)
	}
	if ia["data"] != "QUJDRA==" || ia["format"] != "wav" {
		t.Fatalf("audio round-trip mismatch: %v", ia)
	}
}

func TestBuildRequestNoAudioStringShape(t *testing.T) {
	h := &Handler{}
	body := []byte(`{"model":"gpt-4","messages":[{"role":"user","content":"hello"}]}`)
	univ, err := h.ParseRequest(body)
	if err != nil {
		t.Fatalf("ParseRequest failed: %v", err)
	}
	out, err := h.BuildRequest(univ)
	if err != nil {
		t.Fatalf("BuildRequest failed: %v", err)
	}
	if !strings.Contains(string(out), `"content":"hello"`) {
		t.Fatalf("expected string content shape (backward compat), got %s", string(out))
	}
}
