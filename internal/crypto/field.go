// Package crypto implements the application-level field encryption half of
// the two-layer scheme in ARCHITECTURE.md § Storage security. Layer 1 (the
// deployment's encrypted disk) is outside this package entirely; this is
// Layer 2, run inside the gateway process on the specific columns
// schema/0001_init.sql marks `bytea`.
package crypto

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"errors"
	"fmt"
	"io"
)

// Encryptor performs AES-256-GCM encryption using an already-unwrapped
// data encryption key (DEK). It does not source the DEK from anywhere —
// that's main.go's job in a real build: read the KEK from wherever the
// deployment keeps secrets (env var for self-host, a real KMS for
// enterprise), then UnwrapDEK it before constructing an Encryptor. This
// type never sees the KEK.
type Encryptor struct {
	aead cipher.AEAD
}

// NewEncryptor wraps a 32-byte AES-256 key into an Encryptor.
func NewEncryptor(key []byte) (*Encryptor, error) {
	if len(key) != 32 {
		return nil, fmt.Errorf("crypto: key must be 32 bytes for AES-256, got %d", len(key))
	}
	block, err := aes.NewCipher(key)
	if err != nil {
		return nil, fmt.Errorf("crypto: build cipher: %w", err)
	}
	aead, err := cipher.NewGCM(block)
	if err != nil {
		return nil, fmt.Errorf("crypto: build GCM: %w", err)
	}
	return &Encryptor{aead: aead}, nil
}

// Encrypt returns nonce||ciphertext as a single blob — exactly the bytea
// value schema/0001_init.sql's encrypted columns store, so no separate
// nonce column is needed anywhere in the schema.
func (e *Encryptor) Encrypt(plaintext string) ([]byte, error) {
	nonce := make([]byte, e.aead.NonceSize())
	if _, err := io.ReadFull(rand.Reader, nonce); err != nil {
		return nil, fmt.Errorf("crypto: generate nonce: %w", err)
	}
	return e.aead.Seal(nonce, nonce, []byte(plaintext), nil), nil
}

// Decrypt reverses Encrypt. An empty/nil blob decrypts to "" — a column
// that was never written, not a decryption failure — since several
// episode/summary fields are legitimately optional (e.g. output_text on a
// truncated stream, correction_reason when there's no correction).
func (e *Encryptor) Decrypt(blob []byte) (string, error) {
	if len(blob) == 0 {
		return "", nil
	}
	nonceSize := e.aead.NonceSize()
	if len(blob) < nonceSize {
		return "", errors.New("crypto: ciphertext shorter than nonce, corrupt or wrong key")
	}
	nonce, ciphertext := blob[:nonceSize], blob[nonceSize:]
	plaintext, err := e.aead.Open(nil, nonce, ciphertext, nil)
	if err != nil {
		return "", fmt.Errorf("crypto: decrypt: %w", err)
	}
	return string(plaintext), nil
}

// GenerateDEK produces a fresh random AES-256 key. Run once per deployment
// (or per tenant, for Tier 3) and persist the *wrapped* result
// (WrapDEK) — never regenerated on every startup, or every previously
// encrypted row becomes permanently unreadable.
func GenerateDEK() ([]byte, error) {
	dek := make([]byte, 32)
	if _, err := io.ReadFull(rand.Reader, dek); err != nil {
		return nil, fmt.Errorf("crypto: generate DEK: %w", err)
	}
	return dek, nil
}

// WrapDEK and UnwrapDEK protect the DEK at rest using the deployment's KEK
// — the same AES-GCM primitive applied one envelope level up. Note the DEK
// is raw key bytes, not text; passing it through Encrypt/Decrypt's `string`
// parameter is safe because a Go string is just an immutable byte
// sequence, not required to be valid UTF-8.
func WrapDEK(kek, dek []byte) ([]byte, error) {
	enc, err := NewEncryptor(kek)
	if err != nil {
		return nil, fmt.Errorf("crypto: wrap DEK: %w", err)
	}
	return enc.Encrypt(string(dek))
}

func UnwrapDEK(kek, wrapped []byte) ([]byte, error) {
	enc, err := NewEncryptor(kek)
	if err != nil {
		return nil, fmt.Errorf("crypto: unwrap DEK: %w", err)
	}
	plaintext, err := enc.Decrypt(wrapped)
	if err != nil {
		return nil, fmt.Errorf("crypto: unwrap DEK: %w", err)
	}
	return []byte(plaintext), nil
}
