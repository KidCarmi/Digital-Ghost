// encrypt.go implements AES-256-GCM per-record encryption and HMAC-SHA256
// integrity verification for Digital Ghost memory nodes.
//
// Security properties:
//   - AES-256-GCM: authenticated encryption (confidentiality + integrity + authenticity)
//   - Per-record random IV (nonce): prevents nonce reuse even across process restarts
//   - HMAC-SHA256 over (node_id + timestamp + ciphertext): detects tampering of
//     record metadata even before decryption
//   - Key is held in memory only; sourced from KeyManager (OS keychain)
package storage

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"runtime"
	"time"
)

const (
	// gcmNonceSize is the standard GCM nonce size (96 bits).
	gcmNonceSize = 12

	// hmacSize is the HMAC-SHA256 output size in bytes.
	hmacSize = 32
)

// ErrTampered is returned when HMAC verification fails, indicating the
// record has been modified after storage.
var ErrTampered = errors.New("record integrity check failed: data has been tampered with")

// EncryptedRecord is the on-disk representation of a memory node.
// Layout: [hmac(32)] [nonce(12)] [ciphertext(variable)]
// The HMAC covers: node_id_bytes + timestamp_bytes + nonce + ciphertext
type EncryptedRecord struct {
	NodeID    [16]byte  // Fixed-size node identifier (UUID without dashes)
	Timestamp time.Time // Capture timestamp
	Data      []byte    // [hmac][nonce][ciphertext]
}

// Encryptor performs encryption and decryption using a fixed key.
// Create one per process lifetime; the key is the AES-256 key from KeyManager.
type Encryptor struct {
	block cipher.Block
	key   []byte
}

// NewEncryptor creates an Encryptor from a 32-byte AES-256 key.
func NewEncryptor(key []byte) (*Encryptor, error) {
	if len(key) != keyLen {
		return nil, fmt.Errorf("key must be %d bytes, got %d", keyLen, len(key))
	}
	block, err := aes.NewCipher(key)
	if err != nil {
		return nil, fmt.Errorf("creating AES cipher: %w", err)
	}
	// Retain key reference for HMAC; do not copy (the KeyManager owns it).
	return &Encryptor{block: block, key: key}, nil
}

// Close zeros the key copy held by the Encryptor.
// The cipher.Block expanded key schedule (internal Go stdlib struct) cannot be
// zeroed through the public API; callers should ensure the key was mlock'd via
// KeyManager so its pages cannot be swapped even if cipher.Block survives GC.
func (e *Encryptor) Close() {
	for i := range e.key {
		e.key[i] = 0
	}
	runtime.KeepAlive(e.key)
}

// Seal encrypts plaintext and returns an EncryptedRecord.
// nodeID and timestamp are authenticated (included in HMAC) but not encrypted,
// allowing LanceDB to index by these fields without decryption.
func (e *Encryptor) Seal(nodeID [16]byte, ts time.Time, plaintext []byte) (*EncryptedRecord, error) {
	gcm, err := cipher.NewGCM(e.block)
	if err != nil {
		return nil, fmt.Errorf("creating GCM: %w", err)
	}

	nonce := make([]byte, gcmNonceSize)
	if _, err := io.ReadFull(rand.Reader, nonce); err != nil {
		return nil, fmt.Errorf("generating nonce: %w", err)
	}

	ciphertext := gcm.Seal(nil, nonce, plaintext, nil)

	// Compute HMAC over: nodeID + timestamp(int64 unix nano) + nonce + ciphertext
	mac := e.computeHMAC(nodeID, ts, nonce, ciphertext)

	// Layout: [hmac(32)][nonce(12)][ciphertext]
	data := make([]byte, 0, hmacSize+gcmNonceSize+len(ciphertext))
	data = append(data, mac...)
	data = append(data, nonce...)
	data = append(data, ciphertext...)

	return &EncryptedRecord{
		NodeID:    nodeID,
		Timestamp: ts,
		Data:      data,
	}, nil
}

// Open verifies the HMAC and decrypts an EncryptedRecord.
// Returns ErrTampered if the HMAC does not match.
func (e *Encryptor) Open(record *EncryptedRecord) ([]byte, error) {
	data := record.Data
	if len(data) < hmacSize+gcmNonceSize {
		return nil, fmt.Errorf("record data too short (%d bytes)", len(data))
	}

	storedMAC := data[:hmacSize]
	nonce := data[hmacSize : hmacSize+gcmNonceSize]
	ciphertext := data[hmacSize+gcmNonceSize:]

	// Verify HMAC before attempting decryption (fail fast on tampered data).
	expectedMAC := e.computeHMAC(record.NodeID, record.Timestamp, nonce, ciphertext)
	if !hmac.Equal(storedMAC, expectedMAC) {
		return nil, ErrTampered
	}

	gcm, err := cipher.NewGCM(e.block)
	if err != nil {
		return nil, fmt.Errorf("creating GCM: %w", err)
	}

	plaintext, err := gcm.Open(nil, nonce, ciphertext, nil)
	if err != nil {
		// GCM authentication tag failure — data is corrupt or tampered.
		return nil, ErrTampered
	}

	return plaintext, nil
}

// computeHMAC computes HMAC-SHA256 over the record's authenticated fields.
func (e *Encryptor) computeHMAC(nodeID [16]byte, ts time.Time, nonce, ciphertext []byte) []byte {
	h := hmac.New(sha256.New, e.key)
	h.Write(nodeID[:])
	var tsBuf [8]byte
	binary.BigEndian.PutUint64(tsBuf[:], uint64(ts.UnixNano()))
	h.Write(tsBuf[:])
	h.Write(nonce)
	h.Write(ciphertext)
	return h.Sum(nil)
}
