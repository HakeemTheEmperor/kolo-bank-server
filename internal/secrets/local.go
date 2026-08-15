package secrets

import (
	"context"
	"crypto/aes"
	"crypto/cipher"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"fmt"
	"os"
)

// LocalKeyProvider is a dev/test-only KeyProvider. Keys are 32-byte values
// read from environment variables (base64-encoded), never hardcoded and
// never committed. It is NOT suitable for production — it exists only so
// the KeyProvider interface has a working implementation before a real
// KMS/HSM is integrated.
type LocalKeyProvider struct{}

// NewLocalKeyProvider constructs a LocalKeyProvider.
func NewLocalKeyProvider() *LocalKeyProvider {
	return &LocalKeyProvider{}
}

func (p *LocalKeyProvider) keyBytes(keyName string) ([]byte, error) {
	envVar := "KOLO_KEY_" + keyName
	v, ok := os.LookupEnv(envVar)
	if !ok || v == "" {
		return nil, fmt.Errorf("secrets: no local key configured for %q (expected env var %s)", keyName, envVar)
	}
	key, err := base64.StdEncoding.DecodeString(v)
	if err != nil {
		return nil, fmt.Errorf("secrets: decode key %q: %w", keyName, err)
	}
	if len(key) != 32 {
		return nil, fmt.Errorf("secrets: key %q must be 32 bytes, got %d", keyName, len(key))
	}
	return key, nil
}

func (p *LocalKeyProvider) Sign(_ context.Context, keyName string, data []byte) ([]byte, error) {
	key, err := p.keyBytes(keyName)
	if err != nil {
		return nil, err
	}
	mac := hmac.New(sha256.New, key)
	mac.Write(data)
	return mac.Sum(nil), nil
}

func (p *LocalKeyProvider) Encrypt(_ context.Context, keyName string, plaintext []byte) ([]byte, error) {
	key, err := p.keyBytes(keyName)
	if err != nil {
		return nil, err
	}
	block, err := aes.NewCipher(key)
	if err != nil {
		return nil, fmt.Errorf("secrets: build cipher: %w", err)
	}
	gcm, err := cipher.NewGCM(block)
	if err != nil {
		return nil, fmt.Errorf("secrets: build gcm: %w", err)
	}
	nonce := make([]byte, gcm.NonceSize())
	if _, err := rand.Read(nonce); err != nil {
		return nil, fmt.Errorf("secrets: generate nonce: %w", err)
	}
	return gcm.Seal(nonce, nonce, plaintext, nil), nil
}

func (p *LocalKeyProvider) Decrypt(_ context.Context, keyName string, ciphertext []byte) ([]byte, error) {
	key, err := p.keyBytes(keyName)
	if err != nil {
		return nil, err
	}
	block, err := aes.NewCipher(key)
	if err != nil {
		return nil, fmt.Errorf("secrets: build cipher: %w", err)
	}
	gcm, err := cipher.NewGCM(block)
	if err != nil {
		return nil, fmt.Errorf("secrets: build gcm: %w", err)
	}
	if len(ciphertext) < gcm.NonceSize() {
		return nil, fmt.Errorf("secrets: ciphertext too short")
	}
	nonce, ct := ciphertext[:gcm.NonceSize()], ciphertext[gcm.NonceSize():]
	return gcm.Open(nil, nonce, ct, nil)
}
