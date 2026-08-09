package vault

import (
	"crypto/rand"
	"encoding/base64"
	"errors"
	"testing"
)

func testKey(t *testing.T) []byte {
	t.Helper()
	key := make([]byte, 32)
	if _, err := rand.Read(key); err != nil {
		t.Fatal(err)
	}
	return key
}

func TestDecodeKey_ValidKey(t *testing.T) {
	raw := make([]byte, 32)
	_, _ = rand.Read(raw)
	encoded := base64.StdEncoding.EncodeToString(raw)

	key, err := DecodeKey(encoded)
	if err != nil {
		t.Fatalf("DecodeKey() error = %v, want nil", err)
	}
	if len(key) != 32 {
		t.Fatalf("len(key) = %d, want 32", len(key))
	}
}

func TestDecodeKey_WrongSize(t *testing.T) {
	tooShort := base64.StdEncoding.EncodeToString(make([]byte, 16))
	if _, err := DecodeKey(tooShort); !errors.Is(err, ErrInvalidKeySize) {
		t.Errorf("DecodeKey(16 bytes) error = %v, want ErrInvalidKeySize", err)
	}

	tooLong := base64.StdEncoding.EncodeToString(make([]byte, 64))
	if _, err := DecodeKey(tooLong); !errors.Is(err, ErrInvalidKeySize) {
		t.Errorf("DecodeKey(64 bytes) error = %v, want ErrInvalidKeySize", err)
	}
}

func TestDecodeKey_NotBase64(t *testing.T) {
	if _, err := DecodeKey("not valid base64!!!"); err == nil {
		t.Error("DecodeKey(invalid base64) error = nil, want an error")
	}
}

func TestEncryptDecrypt_RoundTrip(t *testing.T) {
	key := testKey(t)
	const plaintext = "sk-ant-api03-super-secret-anthropic-key"

	ciphertext, err := Encrypt(plaintext, key)
	if err != nil {
		t.Fatalf("Encrypt() error = %v, want nil", err)
	}
	if ciphertext == plaintext {
		t.Fatal("ciphertext equals plaintext — nothing was encrypted")
	}

	decrypted, err := Decrypt(ciphertext, key)
	if err != nil {
		t.Fatalf("Decrypt() error = %v, want nil", err)
	}
	if decrypted != plaintext {
		t.Fatalf("decrypted = %q, want %q", decrypted, plaintext)
	}
}

func TestEncrypt_NonceIsRandomPerCall(t *testing.T) {
	key := testKey(t)
	const plaintext = "same plaintext both times"

	a, err := Encrypt(plaintext, key)
	if err != nil {
		t.Fatal(err)
	}
	b, err := Encrypt(plaintext, key)
	if err != nil {
		t.Fatal(err)
	}
	// Same plaintext, same key, must still produce different ciphertext —
	// a fixed/reused nonce would be a serious AES-GCM confidentiality bug
	// (nonce reuse breaks GCM's security guarantees entirely).
	if a == b {
		t.Fatal("encrypting the same plaintext twice produced identical ciphertext — nonce is not being randomized")
	}
}

func TestDecrypt_WrongKeyFails(t *testing.T) {
	ciphertext, err := Encrypt("secret value", testKey(t))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := Decrypt(ciphertext, testKey(t)); err == nil {
		t.Fatal("Decrypt() with the wrong key succeeded, want an error")
	}
}

func TestDecrypt_TamperedCiphertextFails(t *testing.T) {
	key := testKey(t)
	ciphertext, err := Encrypt("secret value", key)
	if err != nil {
		t.Fatal(err)
	}

	raw, err := base64.StdEncoding.DecodeString(ciphertext)
	if err != nil {
		t.Fatal(err)
	}
	raw[len(raw)-1] ^= 0xFF // flip bits in the auth tag
	tampered := base64.StdEncoding.EncodeToString(raw)

	if _, err := Decrypt(tampered, key); err == nil {
		t.Fatal("Decrypt() of tampered ciphertext succeeded, want GCM's auth tag to catch it")
	}
}

func TestDecrypt_TruncatedCiphertextFails(t *testing.T) {
	if _, err := Decrypt(base64.StdEncoding.EncodeToString([]byte("short")), testKey(t)); err == nil {
		t.Fatal("Decrypt() of a too-short ciphertext succeeded, want an error")
	}
}

func TestDecrypt_NotBase64Fails(t *testing.T) {
	if _, err := Decrypt("not valid base64!!!", testKey(t)); err == nil {
		t.Fatal("Decrypt() of non-base64 input succeeded, want an error")
	}
}

func TestEncrypt_RejectsWrongKeySize(t *testing.T) {
	if _, err := Encrypt("x", make([]byte, 16)); !errors.Is(err, ErrInvalidKeySize) {
		t.Errorf("Encrypt() with a 16-byte key error = %v, want ErrInvalidKeySize", err)
	}
}
