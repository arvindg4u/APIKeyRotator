package auth

import (
	"strings"
	"testing"
	"time"
)

func TestGenerateAndValidateToken(t *testing.T) {
	token, err := GenerateToken("s3cr3t", "admin", time.Hour)
	if err != nil {
		t.Fatalf("GenerateToken: %v", err)
	}
	if strings.Count(token, ".") != 2 {
		t.Fatalf("token %q is not a three-part JWT", token)
	}

	username, err := ValidateToken("s3cr3t", token)
	if err != nil {
		t.Fatalf("ValidateToken: %v", err)
	}
	if username != "admin" {
		t.Fatalf("username = %q, want %q", username, "admin")
	}
}

func TestValidateTokenRejectsWrongSecret(t *testing.T) {
	token, err := GenerateToken("s3cr3t", "admin", time.Hour)
	if err != nil {
		t.Fatalf("GenerateToken: %v", err)
	}
	if _, err := ValidateToken("other-secret", token); err == nil {
		t.Fatal("ValidateToken with a wrong secret = nil error, want an error")
	}
}

func TestValidateTokenRejectsTamperedPayload(t *testing.T) {
	token, err := GenerateToken("s3cr3t", "admin", time.Hour)
	if err != nil {
		t.Fatalf("GenerateToken: %v", err)
	}
	parts := strings.Split(token, ".")
	tampered := parts[0] + "." + parts[1] + "x." + parts[2]
	if _, err := ValidateToken("s3cr3t", tampered); err == nil {
		t.Fatal("ValidateToken with a tampered signature = nil error, want an error")
	}
}

func TestValidateTokenRejectsExpired(t *testing.T) {
	token, err := GenerateToken("s3cr3t", "admin", -time.Minute)
	if err != nil {
		t.Fatalf("GenerateToken: %v", err)
	}
	if _, err := ValidateToken("s3cr3t", token); err != ErrExpiredToken {
		t.Fatalf("ValidateToken error = %v, want %v", err, ErrExpiredToken)
	}
}

func TestValidateTokenRejectsGarbage(t *testing.T) {
	for _, token := range []string{"", "not-a-token", "a.b", "a.b.c.d"} {
		if _, err := ValidateToken("s3cr3t", token); err == nil {
			t.Fatalf("ValidateToken(%q) = nil error, want an error", token)
		}
	}
}

func TestConstantTimeEquals(t *testing.T) {
	if !ConstantTimeEquals("admin", "admin") {
		t.Fatal("ConstantTimeEquals(admin, admin) = false, want true")
	}
	if ConstantTimeEquals("admin", "admin1") {
		t.Fatal("ConstantTimeEquals(admin, admin1) = true, want false")
	}
}
