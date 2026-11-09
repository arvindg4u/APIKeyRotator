package services

import (
	"fmt"
	"net/http"
	"testing"
	"time"
)

func TestClassifyRateLimitFreeTier(t *testing.T) {
	reset := time.Now().UTC().Add(48 * time.Hour).Truncate(time.Minute)
	resetText := reset.Format("2 Jan 2006") + " at " + reset.Format("15:04")
	body := []byte(fmt.Sprintf(
		`{"error":{"message":"Your free allowance for ':free' models is used up. It comes back on %s UTC. Your account is not limited.","type":"free_tier_limit_reached","code":"free_tier_limit_reached"}}`,
		resetText,
	))

	class, wait := ClassifyRateLimit(http.Header{"Retry-After": []string{"3600"}}, body)
	if class != RateLimitFreeTierExhausted {
		t.Fatalf("class = %v, want free-tier exhaustion", class)
	}
	if wait < 47*time.Hour || wait > 49*time.Hour {
		t.Fatalf("wait = %s, want about 48h", wait)
	}
}

func TestClassifyRateLimitFreeTierStartsOn(t *testing.T) {
	reset := time.Now().UTC().Add(72 * time.Hour).Truncate(time.Minute)
	resetText := reset.Format("2 Jan 2006") + " at " + reset.Format("15:04")
	body := []byte(fmt.Sprintf(
		`{"error":{"message":"You've used this period's free allowance. Your next rolling 7-day period starts on %s UTC.","type":"free_tier_limit_reached","code":"free_tier_limit_reached"}}`,
		resetText,
	))

	class, wait := ClassifyRateLimit(nil, body)
	if class != RateLimitFreeTierExhausted {
		t.Fatalf("class = %v, want free-tier exhaustion", class)
	}
	if wait < 71*time.Hour || wait > 73*time.Hour {
		t.Fatalf("wait = %s, want about 72h", wait)
	}
}

func TestClassifyRateLimitServerBusyIsTransient(t *testing.T) {
	body := []byte(`{"error":{"message":"Token Harbor is under heavy load.","type":"rate_limit_error","code":"server_busy"}}`)
	class, wait := ClassifyRateLimit(http.Header{"Retry-After": []string{"60"}}, body)
	if class != RateLimitTransient {
		t.Fatalf("class = %v, want transient", class)
	}
	if wait != 60*time.Second {
		t.Fatalf("wait = %s, want 60s", wait)
	}
}

func TestClassifyRateLimitFreeTierWithoutResetTime(t *testing.T) {
	body := []byte(`{"error":{"message":"free_tier_limit_reached","type":"free_tier_limit_reached","code":"free_tier_limit_reached"}}`)
	class, wait := ClassifyRateLimit(http.Header{"Retry-After": []string{"3600"}}, body)
	if class != RateLimitFreeTierExhausted {
		t.Fatalf("class = %v, want free-tier exhaustion", class)
	}
	if wait != 0 {
		t.Fatalf("wait = %s, want 0 so the caller uses its configured fallback", wait)
	}
}

func TestClassifyRateLimitNonJSONFallback(t *testing.T) {
	class, wait := ClassifyRateLimit(nil, []byte("upstream said free_tier_limit_reached"))
	if class != RateLimitFreeTierExhausted {
		t.Fatalf("class = %v, want free-tier exhaustion", class)
	}
	if wait != 0 {
		t.Fatalf("wait = %s, want 0", wait)
	}
}
