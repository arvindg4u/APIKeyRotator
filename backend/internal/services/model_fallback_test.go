package services

import (
	"encoding/json"
	"strings"
	"testing"
)

func TestFreeModelFallbackBody(t *testing.T) {
	body := []byte(`{"model":"deepseek-v4.1-flash:free","messages":[{"role":"user","content":"hi"}],"max_tokens":5}`)
	newBody, newModel, ok := FreeModelFallbackBody(body)
	if !ok {
		t.Fatal("FreeModelFallbackBody() = false, want true")
	}
	if newModel != "deepseek-v4.1-flash" {
		t.Fatalf("newModel = %q, want deepseek-v4.1-flash", newModel)
	}
	if strings.Contains(string(newBody), ":free") {
		t.Fatalf("rewritten body still contains :free: %s", newBody)
	}
	var parsed map[string]interface{}
	if err := json.Unmarshal(newBody, &parsed); err != nil {
		t.Fatalf("rewritten body is not JSON: %v", err)
	}
	if parsed["model"] != "deepseek-v4.1-flash" {
		t.Fatalf("model = %v, want deepseek-v4.1-flash", parsed["model"])
	}
	if parsed["max_tokens"].(float64) != 5 {
		t.Fatalf("other fields were not preserved: %v", parsed)
	}
}

func TestFreeModelFallbackBodyNoFreeSuffix(t *testing.T) {
	body := []byte(`{"model":"deepseek-v4.1-flash","messages":[]}`)
	if _, _, ok := FreeModelFallbackBody(body); ok {
		t.Fatal("FreeModelFallbackBody() = true for a non-free model")
	}
}

func TestFreeModelFallbackBodyInvalid(t *testing.T) {
	for _, body := range [][]byte{
		[]byte(`not json`),
		[]byte(`{"messages":[]}`),
		[]byte(`{"model":123}`),
		[]byte(`{"model":":free"}`),
	} {
		if _, _, ok := FreeModelFallbackBody(body); ok {
			t.Fatalf("FreeModelFallbackBody(%s) = true, want false", body)
		}
	}
}
