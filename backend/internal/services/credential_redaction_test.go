package services

import (
	"strings"
	"testing"
)

func TestRedactTargetCredentials(t *testing.T) {
	target := &TargetRequest{
		Headers: map[string]string{
			"Authorization":       "Bearer proxy-secret",
			"Proxy-Authorization": "Basic proxy-secret",
			"X-Proxy-Key":         "proxy-secret",
			"X-Admin-Token":       "admin-token",
			"X-Api-Key":           "proxy-secret",
			"X-Goog-Api-Key":      "proxy-secret",
			"X-Anthropic-Api-Key": "proxy-secret",
			"Referer":             "https://client.example/?key=proxy-secret",
			"X-Custom-Credential": "Bearer proxy-secret",
			"Cookie":              "session=legit; leaked=proxy-secret",
			"Content-Type":        "application/json",
			"User-Agent":          "credential-test/1.0",
			"X-Legitimate-Header": "ordinary-value",
		},
		Params: map[string]string{
			"key":        "proxy-secret",
			"foo":        "prefix-proxy-secret-suffix",
			"api_key":    "proxy-secret",
			"legitimate": "ordinary-value",
			"empty":      "",
		},
	}

	RedactTargetCredentials(target, []string{"proxy-secret", "another-secret"})

	for _, header := range []string{
		"Authorization",
		"Proxy-Authorization",
		"X-Proxy-Key",
		"X-Admin-Token",
		"X-Api-Key",
		"X-Goog-Api-Key",
		"X-Anthropic-Api-Key",
		"Referer",
		"X-Custom-Credential",
		"Cookie",
	} {
		if value, ok := target.Headers[header]; ok {
			t.Errorf("header %q leaked: %q", header, value)
		}
	}

	for _, header := range []string{"Content-Type", "User-Agent", "X-Legitimate-Header"} {
		if _, ok := target.Headers[header]; !ok {
			t.Errorf("legitimate header %q was removed", header)
		}
	}

	for _, param := range []string{"key", "foo", "api_key"} {
		if value, ok := target.Params[param]; ok {
			t.Errorf("query parameter %q leaked: %q", param, value)
		}
	}
	if target.Params["legitimate"] != "ordinary-value" {
		t.Errorf("legitimate query parameter was lost: %v", target.Params)
	}
	if _, ok := target.Params["empty"]; !ok {
		t.Error("empty query parameter should not be treated as a credential")
	}

	// A custom header/parameter named after a proxy key but with an unrelated
	// value is not a credential and must be preserved.
	target.Headers["X-Other"] = "not-a-secret"
	target.Params["other"] = "not-a-secret"
	RedactTargetCredentials(target, []string{"proxy-secret"})
	if target.Headers["X-Other"] != "not-a-secret" {
		t.Error("unrelated custom header was removed")
	}
	if target.Params["other"] != "not-a-secret" {
		t.Error("unrelated custom query parameter was removed")
	}

	for name, value := range target.Headers {
		if strings.Contains(value, "proxy-secret") {
			t.Errorf("header %q still contains a proxy key: %q", name, value)
		}
	}
}

func TestRedactTargetCredentialsHandlesNilTarget(t *testing.T) {
	RedactTargetCredentials(nil, []string{"proxy-secret"})
}
