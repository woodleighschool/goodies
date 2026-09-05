package bloby

import (
	"encoding/base64"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"
)

func TestSignVerifyRoundTrip(t *testing.T) {
	t.Parallel()
	now := time.Unix(1_700_000_000, 0)
	claims := capabilityClaims{
		Op:          capabilityGet,
		Exp:         now.Add(time.Minute).Unix(),
		Key:         "munki/packages/1/Installer.pkg",
		ContentType: "application/octet-stream",
	}

	token := signCapability([]byte("secret"), claims)

	got, err := verifyCapability([]byte("secret"), token, capabilityGet, now)
	if err != nil {
		t.Fatalf("Verify: %v", err)
	}
	if got != claims {
		t.Fatalf("claims = %+v, want %+v", got, claims)
	}
}

func TestVerifyRejectsInvalidTokens(t *testing.T) {
	t.Parallel()
	now := time.Unix(1_700_000_000, 0)
	key := []byte("secret")
	valid := signCapability(key, capabilityClaims{
		Op:  capabilityGet,
		Exp: now.Add(time.Minute).Unix(),
		Key: "munki/icons/1/icon.png",
	})

	cases := []struct {
		name  string
		token string
		key   []byte
		want  error
	}{
		{
			name:  "malformed",
			token: "not-a-token",
			key:   key,
			want:  errInvalidCapability,
		},
		{
			name: "tampered claims",
			token: tamperClaims(t, valid, func(claims capabilityClaims) capabilityClaims {
				claims.Key = "munki/icons/2/icon.png"
				return claims
			}),
			key:  key,
			want: errInvalidCapability,
		},
		{
			name:  "tampered mac",
			token: tamperLastByte(valid),
			key:   key,
			want:  errInvalidCapability,
		},
		{
			name:  "wrong key",
			token: valid,
			key:   []byte("other-secret"),
			want:  errInvalidCapability,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			if _, err := verifyCapability(tc.key, tc.token, capabilityGet, now); !errors.Is(err, tc.want) {
				t.Fatalf("Verify error = %v, want %v", err, tc.want)
			}
		})
	}
}

func TestVerifyRejectsExpiredAndWrongOp(t *testing.T) {
	t.Parallel()
	now := time.Unix(1_700_000_000, 0)
	key := []byte("secret")

	expired := signCapability(key, capabilityClaims{
		Op:  capabilityGet,
		Exp: now.Add(-time.Second).Unix(),
		Key: "munki/packages/1/Installer.pkg",
	})
	if _, err := verifyCapability(key, expired, capabilityGet, now); !errors.Is(err, errExpiredCapability) {
		t.Fatalf("expired Verify error = %v, want errExpiredCapability", err)
	}

	put := signCapability(key, capabilityClaims{
		Op:  capabilityPut,
		Exp: now.Add(time.Minute).Unix(),
		Key: "munki/packages/1/Installer.pkg",
	})
	if _, err := verifyCapability(key, put, capabilityGet, now); !errors.Is(err, errInvalidCapability) {
		t.Fatalf("wrong op Verify error = %v, want errInvalidCapability", err)
	}
}

func tamperLastByte(value string) string {
	replacement := byte('x')
	if value[len(value)-1] == replacement {
		replacement = 'y'
	}
	return value[:len(value)-1] + string(replacement)
}

func tamperClaims(t *testing.T, token string, edit func(capabilityClaims) capabilityClaims) string {
	t.Helper()
	payload, mac, ok := strings.Cut(token, ".")
	if !ok {
		t.Fatalf("token has no mac: %q", token)
	}
	raw, err := base64.RawURLEncoding.DecodeString(payload)
	if err != nil {
		t.Fatalf("decode payload: %v", err)
	}
	var claims capabilityClaims
	if err := json.Unmarshal(raw, &claims); err != nil {
		t.Fatalf("unmarshal claims: %v", err)
	}
	raw, err = json.Marshal(edit(claims))
	if err != nil {
		t.Fatalf("marshal claims: %v", err)
	}
	return base64.RawURLEncoding.EncodeToString(raw) + "." + mac
}
