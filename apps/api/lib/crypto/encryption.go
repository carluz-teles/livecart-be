package crypto

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
)

// Encryptor handles AES-GCM encryption for sensitive data like integration credentials.
type Encryptor struct {
	aead cipher.AEAD
}

// NewEncryptor creates a new Encryptor with the given base64-encoded 32-byte key.
// The key should be generated securely and stored as an environment variable.
func NewEncryptor(base64Key string) (*Encryptor, error) {
	keyBytes, err := base64.StdEncoding.DecodeString(base64Key)
	if err != nil {
		return nil, fmt.Errorf("decoding encryption key: %w", err)
	}

	if len(keyBytes) != 32 {
		return nil, fmt.Errorf("encryption key must be 32 bytes, got %d", len(keyBytes))
	}

	block, err := aes.NewCipher(keyBytes)
	if err != nil {
		return nil, fmt.Errorf("creating AES cipher: %w", err)
	}

	aead, err := cipher.NewGCM(block)
	if err != nil {
		return nil, fmt.Errorf("creating GCM: %w", err)
	}

	return &Encryptor{aead: aead}, nil
}

// Encrypt encrypts plaintext bytes using AES-GCM.
// The nonce is prepended to the ciphertext for storage.
func (e *Encryptor) Encrypt(plaintext []byte) ([]byte, error) {
	nonce := make([]byte, e.aead.NonceSize())
	if _, err := io.ReadFull(rand.Reader, nonce); err != nil {
		return nil, fmt.Errorf("generating nonce: %w", err)
	}

	// Seal appends the encrypted data to nonce, so result is: nonce || ciphertext || tag
	ciphertext := e.aead.Seal(nonce, nonce, plaintext, nil)
	return ciphertext, nil
}

// Decrypt decrypts ciphertext that was encrypted with Encrypt.
// Expects the nonce to be prepended to the ciphertext.
func (e *Encryptor) Decrypt(ciphertext []byte) ([]byte, error) {
	nonceSize := e.aead.NonceSize()
	if len(ciphertext) < nonceSize {
		return nil, fmt.Errorf("ciphertext too short: got %d bytes, need at least %d", len(ciphertext), nonceSize)
	}

	nonce, encryptedData := ciphertext[:nonceSize], ciphertext[nonceSize:]
	plaintext, err := e.aead.Open(nil, nonce, encryptedData, nil)
	if err != nil {
		return nil, fmt.Errorf("decrypting data: %w", err)
	}

	return plaintext, nil
}

// EncryptJSON marshals the value to JSON and encrypts it.
func (e *Encryptor) EncryptJSON(v any) ([]byte, error) {
	plaintext, err := json.Marshal(v)
	if err != nil {
		return nil, fmt.Errorf("marshaling to JSON: %w", err)
	}
	return e.Encrypt(plaintext)
}

// DecryptJSON decrypts the ciphertext and unmarshals the JSON into v.
func (e *Encryptor) DecryptJSON(ciphertext []byte, v any) error {
	plaintext, err := e.Decrypt(ciphertext)
	if err != nil {
		return err
	}
	if err := json.Unmarshal(plaintext, v); err != nil {
		return fmt.Errorf("unmarshaling JSON: %w", err)
	}
	return nil
}

// GenerateKey generates a new random 32-byte key and returns it as base64.
// Use this to generate ENCRYPTION_KEY for your environment.
func GenerateKey() (string, error) {
	key := make([]byte, 32)
	if _, err := io.ReadFull(rand.Reader, key); err != nil {
		return "", fmt.Errorf("generating random key: %w", err)
	}
	return base64.StdEncoding.EncodeToString(key), nil
}
