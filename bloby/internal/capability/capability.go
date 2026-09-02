package capability

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"strings"
	"time"
)

const (
	// OpGet allows a token to read one object.
	OpGet = "get"
	// OpPut allows a token to write one object.
	OpPut = "put"
)

var (
	// ErrInvalid reports a malformed token or a failed MAC.
	ErrInvalid = errors.New("invalid capability")
	// ErrExpired reports a valid token whose expiry has passed.
	ErrExpired = errors.New("expired capability")
	// ErrWrongOp reports a valid token used for a different operation.
	ErrWrongOp = errors.New("wrong capability operation")
)

// envelope is the part of every claims payload the codec reads itself: the
// operation a token authorizes and when it expires. Claims types carry these
// two fields with these json tags alongside their own.
type envelope struct {
	Op  string `json:"op"`
	Exp int64  `json:"exp"`
}

// Sign returns a compact HMAC token over the JSON encoding of claims.
func Sign(key []byte, claims any) (string, error) {
	payload, err := json.Marshal(claims)
	if err != nil {
		return "", err
	}
	encodedPayload := base64.RawURLEncoding.EncodeToString(payload)
	mac := hmacSHA256(key, []byte(encodedPayload))
	return encodedPayload + "." + base64.RawURLEncoding.EncodeToString(mac), nil
}

// Verify checks a token's MAC, expiry, and operation, then decodes its payload
// into C. Claim-specific checks, such as a storage key or mirror integrity, are
// the caller's responsibility.
func Verify[C any](key []byte, token string, op string, now time.Time) (C, error) {
	var claims C
	payload, err := open(key, token)
	if err != nil {
		return claims, err
	}
	var env envelope
	if err := json.Unmarshal(payload, &env); err != nil {
		return claims, ErrInvalid
	}
	if env.Exp <= now.Unix() {
		return claims, ErrExpired
	}
	if env.Op != op {
		return claims, ErrWrongOp
	}
	if err := json.Unmarshal(payload, &claims); err != nil {
		return claims, ErrInvalid
	}
	return claims, nil
}

// open splits a token, verifies its MAC against key, and returns the raw claims
// JSON for the caller to decode.
func open(key []byte, token string) ([]byte, error) {
	encodedPayload, mac, ok := strings.Cut(token, ".")
	if !ok || encodedPayload == "" || mac == "" || strings.Contains(mac, ".") {
		return nil, ErrInvalid
	}
	gotMAC, err := base64.RawURLEncoding.DecodeString(mac)
	if err != nil {
		return nil, ErrInvalid
	}
	if !hmac.Equal(gotMAC, hmacSHA256(key, []byte(encodedPayload))) {
		return nil, ErrInvalid
	}
	payload, err := base64.RawURLEncoding.DecodeString(encodedPayload)
	if err != nil {
		return nil, ErrInvalid
	}
	return payload, nil
}

func hmacSHA256(key []byte, message []byte) []byte {
	mac := hmac.New(sha256.New, key)
	_, _ = mac.Write(message)
	return mac.Sum(nil)
}
