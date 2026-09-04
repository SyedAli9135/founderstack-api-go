// Package vault encrypts secrets (BYOK keys, OAuth tokens) for storage at
// rest, using AES-256-GCM against ENCRYPTION_KEY — reused from
// founderstack-api's Fernet key (base64-decoded, exactly the 32 raw bytes
// AES-256 needs). The two backends' encrypted values are NOT
// byte-compatible — Fernet's envelope differs from GCM's — only the key
// material is shared.
package vault

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"encoding/base64"
	"errors"
	"fmt"
	"io"
)

// ErrInvalidKeySize is returned by DecodeKey when ENCRYPTION_KEY doesn't
// decode to exactly 32 bytes — AES-256 requires precisely that, and a
// silently-wrong key size would otherwise surface as a confusing panic
// deep inside aes.NewCipher instead of a clear config error.
var ErrInvalidKeySize = errors.New("vault: key must be exactly 32 bytes (AES-256) after base64 decoding")

// DecodeKey parses ENCRYPTION_KEY's base64 form into the raw key bytes
// Encrypt/Decrypt expect.
func DecodeKey(base64Key string) ([]byte, error) {
	key, err := base64.StdEncoding.DecodeString(base64Key)
	if err != nil {
		return nil, fmt.Errorf("vault: decode key: %w", err)
	}
	if len(key) != 32 {
		return nil, ErrInvalidKeySize
	}
	return key, nil
}

// Encrypt seals plaintext with AES-256-GCM under key (from DecodeKey) and
// returns base64(nonce || ciphertext || auth tag) — everything needed to
// decrypt, in one opaque string safe to store in a text column.
func Encrypt(plaintext string, key []byte) (string, error) {
	gcm, err := newGCM(key)
	if err != nil {
		return "", err
	}

	nonce := make([]byte, gcm.NonceSize())
	if _, err := io.ReadFull(rand.Reader, nonce); err != nil {
		return "", fmt.Errorf("vault: generate nonce: %w", err)
	}

	sealed := gcm.Seal(nonce, nonce, []byte(plaintext), nil)
	return base64.StdEncoding.EncodeToString(sealed), nil
}

// Decrypt reverses Encrypt. Returns an error (never partial/garbage
// plaintext) if ciphertext was tampered with, truncated, or encrypted
// under a different key — GCM's authentication tag makes all three
// detectable rather than silently producing wrong output.
func Decrypt(ciphertext string, key []byte) (string, error) {
	gcm, err := newGCM(key)
	if err != nil {
		return "", err
	}

	raw, err := base64.StdEncoding.DecodeString(ciphertext)
	if err != nil {
		return "", fmt.Errorf("vault: decode ciphertext: %w", err)
	}

	nonceSize := gcm.NonceSize()
	if len(raw) < nonceSize {
		return "", errors.New("vault: ciphertext shorter than nonce — truncated or corrupt")
	}
	nonce, sealed := raw[:nonceSize], raw[nonceSize:]

	plaintext, err := gcm.Open(nil, nonce, sealed, nil)
	if err != nil {
		return "", fmt.Errorf("vault: decrypt: %w", err)
	}
	return string(plaintext), nil
}

func newGCM(key []byte) (cipher.AEAD, error) {
	if len(key) != 32 {
		return nil, ErrInvalidKeySize
	}
	block, err := aes.NewCipher(key)
	if err != nil {
		return nil, fmt.Errorf("vault: new cipher: %w", err)
	}
	gcm, err := cipher.NewGCM(block)
	if err != nil {
		return nil, fmt.Errorf("vault: new gcm: %w", err)
	}
	return gcm, nil
}
