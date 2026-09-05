package bloby

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
	capabilityGet = "get"
	capabilityPut = "put"
)

var (
	errInvalidCapability = errors.New("invalid storage capability")
	errExpiredCapability = errors.New("expired storage capability")
)

type capabilityClaims struct {
	Op          string `json:"op"`
	Key         string `json:"key"`
	Exp         int64  `json:"exp"`
	ContentType string `json:"content_type,omitempty"`
}

func signCapability(key []byte, claims capabilityClaims) string {
	payload, _ := json.Marshal(claims)
	encodedPayload := base64.RawURLEncoding.EncodeToString(payload)
	mac := capabilityMAC(key, encodedPayload)
	return encodedPayload + "." + base64.RawURLEncoding.EncodeToString(mac)
}

func verifyCapability(key []byte, token, op string, now time.Time) (capabilityClaims, error) {
	var claims capabilityClaims
	encodedPayload, encodedMAC, ok := strings.Cut(token, ".")
	if !ok || encodedPayload == "" || encodedMAC == "" {
		return claims, errInvalidCapability
	}
	mac, err := base64.RawURLEncoding.DecodeString(encodedMAC)
	if err != nil || !hmac.Equal(mac, capabilityMAC(key, encodedPayload)) {
		return claims, errInvalidCapability
	}
	payload, err := base64.RawURLEncoding.DecodeString(encodedPayload)
	if err != nil || json.Unmarshal(payload, &claims) != nil {
		return capabilityClaims{}, errInvalidCapability
	}
	if claims.Exp <= now.Unix() {
		return capabilityClaims{}, errExpiredCapability
	}
	if claims.Op != op {
		return capabilityClaims{}, errInvalidCapability
	}
	return claims, nil
}

func capabilityMAC(key []byte, message string) []byte {
	mac := hmac.New(sha256.New, key)
	_, _ = mac.Write([]byte(message))
	return mac.Sum(nil)
}
