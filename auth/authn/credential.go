package authn

import (
	"crypto/rand"
	"encoding/base64"
	"errors"

	"github.com/alexedwards/argon2id"
)

// ErrWeakPassword is returned when a local password is shorter than 12 bytes.
var ErrWeakPassword = errors.New("password must be at least 12 characters")

// Explicit costs preserve the established credential format across library updates.
var passwordParams = &argon2id.Params{
	Memory: 64 * 1024, Iterations: 3, Parallelism: 1, SaltLength: 16, KeyLength: 32,
}

// HashPassword returns an encoded Argon2id hash for password.
func HashPassword(password string) (string, error) {
	if len(password) < 12 {
		return "", ErrWeakPassword
	}
	return argon2id.CreateHash(password, passwordParams)
}

// VerifyPassword reports whether password matches an encoded Argon2id hash.
func VerifyPassword(password string, encoded string) (bool, error) {
	return argon2id.ComparePasswordAndHash(password, encoded)
}

func randomToken(byteCount int) (string, error) {
	b := make([]byte, byteCount)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return base64.RawURLEncoding.EncodeToString(b), nil
}
