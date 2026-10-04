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
	"strings"
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

const keySize = 32

// DecodeKeyring parses the current ENCRYPTION_KEY plus any retired keys
// (comma-separated, newest first) into one value Encrypt/Decrypt accept: keys
// laid end to end, current first. Encrypt only ever uses the current key;
// Decrypt tries each in turn, so values written before a rotation stay
// readable until cmd/rotatekeys has re-encrypted them. Keeping the ciphertext
// format unchanged is what makes rotation a config change plus one command
// instead of a data migration.
func DecodeKeyring(current, previousCSV string) ([]byte, error) {
	ring, err := DecodeKey(current)
	if err != nil {
		return nil, err
	}
	for _, p := range strings.Split(previousCSV, ",") {
		if p = strings.TrimSpace(p); p == "" {
			continue
		}
		old, err := DecodeKey(p)
		if err != nil {
			return nil, fmt.Errorf("vault: ENCRYPTION_KEY_PREVIOUS: %w", err)
		}
		ring = append(ring, old...)
	}
	return ring, nil
}

// UsesCurrentKey reports whether ciphertext opens under the current key alone
// (the first key of a keyring), i.e. whether it still needs rotating.
func UsesCurrentKey(ciphertext string, keyring []byte) bool {
	if len(keyring) < keySize {
		return false
	}
	_, err := Decrypt(ciphertext, keyring[:keySize])
	return err == nil
}

// Encrypt seals plaintext with AES-256-GCM under key (from DecodeKey, or the
// current key of a DecodeKeyring) and returns
// base64(nonce || ciphertext || auth tag) — everything needed to decrypt, in
// one opaque string safe to store in a text column.
func Encrypt(plaintext string, key []byte) (string, error) {
	if len(key) > keySize && len(key)%keySize == 0 {
		key = key[:keySize]
	}
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
	if len(key) > keySize && len(key)%keySize == 0 {
		var lastErr error
		for i := 0; i < len(key); i += keySize {
			plaintext, err := Decrypt(ciphertext, key[i:i+keySize])
			if err == nil {
				return plaintext, nil
			}
			lastErr = err
		}
		return "", lastErr
	}
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
