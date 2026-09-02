package capability

import (
	"encoding/base64"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"
)

// testClaims is a representative caller payload: the shared op and exp fields
// plus its own.
type testClaims struct {
	Op     string `json:"op"`
	Exp    int64  `json:"exp"`
	Key    string `json:"key"`
	SHA256 string `json:"sha256,omitempty"`
}

func TestSignVerifyRoundTrip(t *testing.T) {
	t.Parallel()
	now := time.Unix(1_700_000_000, 0)
	claims := testClaims{
		Op:     OpGet,
		Exp:    now.Add(time.Minute).Unix(),
		Key:    "munki/packages/1/Installer.pkg",
		SHA256: strings.Repeat("a", 64),
	}

	token, err := Sign([]byte("secret"), claims)
	if err != nil {
		t.Fatalf("Sign: %v", err)
	}

	got, err := Verify[testClaims]([]byte("secret"), token, OpGet, now)
	if err != nil {
		t.Fatalf("Verify: %v", err)
	}
	if got != claims {
		t.Fatalf("claims = %+v, want %+v", got, claims)
	}
}

// TestVerifyDecodesIntoCallerType proves the codec is generic over the payload:
// a token signed from one struct verifies into any struct sharing the op and
// exp fields, decoding only the fields that type declares.
func TestVerifyDecodesIntoCallerType(t *testing.T) {
	t.Parallel()
	now := time.Unix(1_700_000_000, 0)
	token, err := Sign([]byte("secret"), testClaims{
		Op:     OpGet,
		Exp:    now.Add(time.Minute).Unix(),
		Key:    "munki/packages/7/Chrome.pkg",
		SHA256: strings.Repeat("b", 64),
	})
	if err != nil {
		t.Fatalf("Sign: %v", err)
	}

	type grantClaims struct {
		Op        string `json:"op"`
		Exp       int64  `json:"exp"`
		PackageID int64  `json:"package_id"`
	}
	got, err := Verify[grantClaims]([]byte("secret"), token, OpGet, now)
	if err != nil {
		t.Fatalf("Verify: %v", err)
	}
	if got.Op != OpGet {
		t.Fatalf("op = %q, want %q", got.Op, OpGet)
	}
}

func TestVerifyRejectsInvalidTokens(t *testing.T) {
	t.Parallel()
	now := time.Unix(1_700_000_000, 0)
	key := []byte("secret")
	valid, err := Sign(key, testClaims{
		Op:  OpGet,
		Exp: now.Add(time.Minute).Unix(),
		Key: "munki/icons/1/icon.png",
	})
	if err != nil {
		t.Fatalf("Sign: %v", err)
	}

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
			want:  ErrInvalid,
		},
		{
			name: "tampered claims",
			token: tamperClaims(t, valid, func(claims testClaims) testClaims {
				claims.Key = "munki/icons/2/icon.png"
				return claims
			}),
			key:  key,
			want: ErrInvalid,
		},
		{
			name:  "tampered mac",
			token: tamperLastByte(valid),
			key:   key,
			want:  ErrInvalid,
		},
		{
			name:  "wrong key",
			token: valid,
			key:   []byte("other-secret"),
			want:  ErrInvalid,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			if _, err := Verify[testClaims](tc.key, tc.token, OpGet, now); !errors.Is(err, tc.want) {
				t.Fatalf("Verify error = %v, want %v", err, tc.want)
			}
		})
	}
}

func TestVerifyRejectsExpiredAndWrongOp(t *testing.T) {
	t.Parallel()
	now := time.Unix(1_700_000_000, 0)
	key := []byte("secret")

	expired, err := Sign(key, testClaims{
		Op:  OpGet,
		Exp: now.Add(-time.Second).Unix(),
		Key: "munki/packages/1/Installer.pkg",
	})
	if err != nil {
		t.Fatalf("Sign expired: %v", err)
	}
	if _, err := Verify[testClaims](key, expired, OpGet, now); !errors.Is(err, ErrExpired) {
		t.Fatalf("expired Verify error = %v, want ErrExpired", err)
	}

	put, err := Sign(key, testClaims{
		Op:  OpPut,
		Exp: now.Add(time.Minute).Unix(),
		Key: "munki/packages/1/Installer.pkg",
	})
	if err != nil {
		t.Fatalf("Sign put: %v", err)
	}
	if _, err := Verify[testClaims](key, put, OpGet, now); !errors.Is(err, ErrWrongOp) {
		t.Fatalf("wrong op Verify error = %v, want ErrWrongOp", err)
	}
}

func tamperLastByte(value string) string {
	replacement := byte('x')
	if value[len(value)-1] == replacement {
		replacement = 'y'
	}
	return value[:len(value)-1] + string(replacement)
}

func tamperClaims(t *testing.T, token string, edit func(testClaims) testClaims) string {
	t.Helper()
	payload, mac, ok := strings.Cut(token, ".")
	if !ok {
		t.Fatalf("token has no mac: %q", token)
	}
	raw, err := base64.RawURLEncoding.DecodeString(payload)
	if err != nil {
		t.Fatalf("decode payload: %v", err)
	}
	var claims testClaims
	if err := json.Unmarshal(raw, &claims); err != nil {
		t.Fatalf("unmarshal claims: %v", err)
	}
	raw, err = json.Marshal(edit(claims))
	if err != nil {
		t.Fatalf("marshal claims: %v", err)
	}
	return base64.RawURLEncoding.EncodeToString(raw) + "." + mac
}
