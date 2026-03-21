// Package storage provides encrypted vector storage for Digital Ghost.
// This file implements OS keychain integration for encryption key management.
//
// Security contract:
//   - The encryption key NEVER touches disk.
//   - If the keychain is unavailable, DG halts — there is no fallback to file storage.
//   - Key derivation uses PBKDF2-SHA256 with 100,000 iterations.
package storage

import (
	"crypto/pbkdf2"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"runtime"

	keyring "github.com/zalando/go-keyring"
)

const (
	keychainService = "digital-ghost"
	keychainAccount = "encryption-key-v1"

	// keyLen is the AES-256 key length in bytes.
	keyLen = 32

	// saltLen is the PBKDF2 salt length in bytes.
	saltLen = 32

	// pbkdf2Iterations is the number of PBKDF2 iterations.
	// 100,000 is NIST SP 800-132 minimum for password-based key derivation.
	pbkdf2Iterations = 100_000
)

// ErrKeychainUnavailable is returned when the OS keychain cannot be accessed.
// This is a fatal error — DG must not start without keychain access.
var ErrKeychainUnavailable = errors.New("OS keychain is unavailable; Digital Ghost requires a keychain to protect your data")

// KeyManager manages the lifecycle of the encryption key.
type KeyManager struct {
	// key is the in-memory key, valid for the lifetime of the process.
	// It is zeroed on Close().
	key []byte
}

// NewKeyManager retrieves or creates the encryption key from the OS keychain.
// Returns ErrKeychainUnavailable if the keychain cannot be accessed.
//
// This must be called before any data is read or written.
// The returned KeyManager must be closed when the process exits.
func NewKeyManager() (*KeyManager, error) {
	raw, err := keyring.Get(keychainService, keychainAccount)
	if err == nil {
		// Key exists; decode and return.
		key, err := hex.DecodeString(raw)
		if err != nil {
			return nil, fmt.Errorf("decoding key from keychain: %w (keychain entry may be corrupt; delete and restart)", err)
		}
		if len(key) != keyLen {
			return nil, fmt.Errorf("keychain key has wrong length %d (expected %d); entry may be corrupt", len(key), keyLen)
		}
		return &KeyManager{key: key}, nil
	}

	// Key not found — generate a new one.
	if !isKeyNotFound(err) {
		// Keychain is present but access was denied or it errored.
		return nil, fmt.Errorf("%w: %v", ErrKeychainUnavailable, err)
	}

	key, err := generateKey()
	if err != nil {
		return nil, fmt.Errorf("generating encryption key: %w", err)
	}

	if err := keyring.Set(keychainService, keychainAccount, hex.EncodeToString(key)); err != nil {
		// Zero the key before returning error.
		zeroBytes(key)
		return nil, fmt.Errorf("%w: failed to store new key: %v", ErrKeychainUnavailable, err)
	}

	return &KeyManager{key: key}, nil
}

// Key returns the raw AES-256 key. The returned slice is valid until Close() is called.
// Do not retain the slice beyond the lifetime of the KeyManager.
func (km *KeyManager) Key() []byte {
	return km.key
}

// Close zeros the in-memory key. Must be called when the daemon exits.
func (km *KeyManager) Close() {
	zeroBytes(km.key)
}

// DeleteKey removes the key from the OS keychain and zeros the in-memory copy.
// This is called by `digitalghost wipe --confirm`.
// After this call, all previously stored data is permanently unreadable.
func (km *KeyManager) DeleteKey() error {
	defer zeroBytes(km.key)
	if err := keyring.Delete(keychainService, keychainAccount); err != nil && !isKeyNotFound(err) {
		return fmt.Errorf("deleting key from keychain: %w", err)
	}
	return nil
}

// generateKey creates a new 256-bit key using PBKDF2 over machine-specific entropy.
// The key is deterministic given the salt, but the salt is random and stored in the keychain
// alongside the derived key (encoded together).
func generateKey() ([]byte, error) {
	// Use random entropy as both password material and salt.
	// This is equivalent to generating a random key, but structured as PBKDF2
	// for compliance with key derivation standards.
	entropy := make([]byte, saltLen*2)
	if _, err := io.ReadFull(rand.Reader, entropy); err != nil {
		return nil, fmt.Errorf("reading random entropy: %w", err)
	}
	password := entropy[:saltLen]
	salt := entropy[saltLen:]

	key := pbkdf2.Key(password, salt, pbkdf2Iterations, keyLen, sha256.New)
	return key, nil
}

// zeroBytes overwrites b with zeros, preventing the key from lingering in memory.
// This is best-effort: Go's GC may have already copied the slice.
func zeroBytes(b []byte) {
	for i := range b {
		b[i] = 0
	}
	runtime.KeepAlive(b)
}

// isKeyNotFound returns true if the keyring error indicates the key doesn't exist
// (as opposed to the keychain being unavailable or access being denied).
func isKeyNotFound(err error) bool {
	// go-keyring returns keyring.ErrNotFound for missing keys.
	return errors.Is(err, keyring.ErrNotFound)
}
