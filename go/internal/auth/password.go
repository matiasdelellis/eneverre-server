package auth

import (
	"crypto/rand"
	"crypto/sha1"
	"crypto/sha256"
	"crypto/sha512"
	"crypto/subtle"
	"encoding/hex"
	"hash"
	"math/big"
	"strconv"
	"strings"

	"golang.org/x/crypto/pbkdf2"
	"golang.org/x/crypto/scrypt"
)

// CheckPasswordHash verifies a password against a Werkzeug-format hash. It
// supports both formats present in existing databases: pbkdf2 and scrypt.
//
// The stored form is "method$salt$hexhash" where method is itself
// colon-delimited: "pbkdf2:sha256:600000" or "scrypt:32768:8:1".
func CheckPasswordHash(stored, password string) bool {
	parts := strings.SplitN(stored, "$", 3)
	if len(parts) != 3 {
		return false
	}
	method, salt, want := parts[0], parts[1], parts[2]

	var got string
	switch {
	case strings.HasPrefix(method, "pbkdf2:"):
		got = pbkdf2Hash(method, salt, password)
	case strings.HasPrefix(method, "scrypt:"):
		got = scryptHash(method, salt, password)
	default:
		return false
	}
	if got == "" {
		return false
	}
	return subtle.ConstantTimeCompare([]byte(got), []byte(want)) == 1
}

func pbkdf2Hash(method, salt, password string) string {
	// method = pbkdf2:<digest>:<iterations>
	fields := strings.Split(method, ":")
	if len(fields) != 3 {
		return ""
	}
	var h func() hash.Hash
	var size int
	switch fields[1] {
	case "sha256":
		h, size = sha256.New, sha256.Size
	case "sha1":
		h, size = sha1.New, sha1.Size
	case "sha512":
		h, size = sha512.New, sha512.Size
	default:
		return ""
	}
	iter, err := strconv.Atoi(fields[2])
	if err != nil {
		return ""
	}
	dk := pbkdf2.Key([]byte(password), []byte(salt), iter, size, h)
	return hex.EncodeToString(dk)
}

func scryptHash(method, salt, password string) string {
	// method = scrypt:<N>:<r>:<p>; Werkzeug uses a 64-byte derived key.
	fields := strings.Split(method, ":")
	if len(fields) != 4 {
		return ""
	}
	n, err1 := strconv.Atoi(fields[1])
	r, err2 := strconv.Atoi(fields[2])
	p, err3 := strconv.Atoi(fields[3])
	if err1 != nil || err2 != nil || err3 != nil {
		return ""
	}
	dk, err := scrypt.Key([]byte(password), []byte(salt), n, r, p, 64)
	if err != nil {
		return ""
	}
	return hex.EncodeToString(dk)
}

// GeneratePasswordHash produces a Werkzeug-format pbkdf2:sha256 hash, so hashes
// interoperate with any existing data/eneverre.db.
func GeneratePasswordHash(password string) string {
	const iterations = 600000
	salt := randSalt(16)
	dk := pbkdf2.Key([]byte(password), []byte(salt), iterations, sha256.Size, sha256.New)
	return "pbkdf2:sha256:" + strconv.Itoa(iterations) + "$" + salt + "$" + hex.EncodeToString(dk)
}

const saltAlphabet = "abcdefghijklmnopqrstuvwxyzABCDEFGHIJKLMNOPQRSTUVWXYZ0123456789"

func randSalt(n int) string {
	b := make([]byte, n)
	max := big.NewInt(int64(len(saltAlphabet)))
	for i := range b {
		idx, _ := rand.Int(rand.Reader, max)
		b[i] = saltAlphabet[idx.Int64()]
	}
	return string(b)
}
