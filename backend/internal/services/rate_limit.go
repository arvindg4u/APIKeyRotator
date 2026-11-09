package services

import (
	"encoding/json"
	"fmt"
	"net/http"
	"regexp"
	"strings"
	"time"
)

// RateLimitClass distinguishes a transient upstream throttle from a
// per-account free-tier allowance that will not recover for hours or days.
type RateLimitClass int

const (
	// RateLimitTransient is a normal 429: rotate the key and cool it briefly.
	RateLimitTransient RateLimitClass = iota
	// RateLimitFreeTierExhausted means the key's free-model allowance is used
	// up until a specific reset time (Token Harbor, and similar gateways).
	// Rotating to other keys is still useful, but this key must not be put
	// back into free-model rotation after a 30-minute cooldown.
	RateLimitFreeTierExhausted
)

// freeTierResetRe matches Token Harbor's free-allowance reset sentences:
//
//	"It comes back on 13 Nov 2026 at 08:57 UTC."
//	"Your next rolling 7-day period starts on 13 Nov 2026 at 07:53 UTC."
var freeTierResetRe = regexp.MustCompile(`(?i)(?:comes back on|starts on)\s+(\d{1,2})\s+([A-Za-z]{3,9})\s+(\d{4})\s+at\s+(\d{1,2}):(\d{2})\s*UTC`)

// ClassifyRateLimit inspects a 429 response and separates:
//
//   - free_tier_limit_reached: account-level free allowance. The returned
//     duration is the parsed reset time when available, otherwise 0 so the
//     caller can use its configured free-tier fallback.
//   - everything else (including Token Harbor's server_busy): a transient
//     throttle that should use Retry-After / the short key cooldown.
func ClassifyRateLimit(header http.Header, body []byte) (RateLimitClass, time.Duration) {
	code, typ, message := rateLimitErrorDetails(body)

	if strings.EqualFold(code, "free_tier_limit_reached") || strings.EqualFold(typ, "free_tier_limit_reached") {
		if wait := parseFreeTierReset(message); wait > 0 {
			return RateLimitFreeTierExhausted, wait
		}
		return RateLimitFreeTierExhausted, 0
	}

	return RateLimitTransient, ParseRetryAfter(header, body)
}

// rateLimitErrorDetails extracts code/type/message from the two common error
// shapes:
//
//	OpenAI:    {"error":{"code":"...","type":"...","message":"..."}}
//	Anthropic: {"type":"error","error":{"type":"...","message":"..."}}
func rateLimitErrorDetails(body []byte) (code, typ, message string) {
	var payload struct {
		Type  string `json:"type"`
		Error struct {
			Type    string `json:"type"`
			Code    string `json:"code"`
			Message string `json:"message"`
		} `json:"error"`
	}
	if err := json.Unmarshal(body, &payload); err == nil {
		code = payload.Error.Code
		typ = payload.Error.Type
		if typ == "" {
			typ = payload.Type
		}
		message = payload.Error.Message
	}

	if code == "" && typ == "" && message == "" && len(body) > 0 {
		// Non-JSON or unexpected shape: still recognize the token so a
		// free-tier exhaustion is not downgraded to a short transient cooldown.
		text := string(body)
		if strings.Contains(strings.ToLower(text), "free_tier_limit_reached") {
			code = "free_tier_limit_reached"
			message = text
		}
	}
	return code, typ, message
}

// parseFreeTierReset turns the reset sentence into a duration from now.
// Returns 0 when the text cannot be parsed or the reset time has passed.
func parseFreeTierReset(message string) time.Duration {
	match := freeTierResetRe.FindStringSubmatch(message)
	if len(match) != 6 {
		return 0
	}

	reset, err := time.ParseInLocation(
		"2 Jan 2006 15:04",
		fmt.Sprintf("%s %s %s %02s:%s", match[1], match[2], match[3], match[4], match[5]),
		time.UTC,
	)
	if err != nil {
		return 0
	}

	wait := time.Until(reset)
	if wait <= 0 {
		return 0
	}
	// A small buffer avoids re-trying exactly on the boundary.
	return wait + time.Minute
}
