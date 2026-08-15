// Package secrets defines the seam between application code and key
// management. The interface is stable across environments; only the
// implementation changes (local dev file/env-backed keys now, a real
// KMS/HSM adapter such as AWS KMS or Vault later) without touching callers.
package secrets

import "context"

// KeyProvider signs and encrypts data using keys it manages. Implementations
// never expose raw key material to callers.
type KeyProvider interface {
	// Sign returns a signature over data using the named key.
	Sign(ctx context.Context, keyName string, data []byte) ([]byte, error)
	// Encrypt encrypts plaintext using the named key.
	Encrypt(ctx context.Context, keyName string, plaintext []byte) ([]byte, error)
	// Decrypt reverses Encrypt.
	Decrypt(ctx context.Context, keyName string, ciphertext []byte) ([]byte, error)
}
