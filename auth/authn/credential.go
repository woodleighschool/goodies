package authn

import (
	"crypto/rand"
	"encoding/base64"

	"github.com/alexedwards/argon2id"
)

// Password hashes use 64 MiB of memory and three Argon2id iterations.
var passwordParams = &argon2id.Params{
	Memory: 64 * 1024, Iterations: 3, Parallelism: 1, SaltLength: 16, KeyLength: 32,
}

// HashPassword returns an encoded Argon2id hash for password.
func HashPassword(password string) (string, error) {
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
