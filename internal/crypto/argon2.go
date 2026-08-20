package crypto

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"errors"
	"fmt"
	"io"

	"golang.org/x/crypto/argon2"
)

// Argon2Params holds the parameters for Argon2id key derivation.
type Argon2Params struct {
	Time    uint32
	Memory  uint32 // in KB
	Threads uint8
	SaltLen uint32
	KeyLen  uint32
}

// DefaultArgon2Params returns production-ready Argon2id parameters.
func DefaultArgon2Params() Argon2Params {
	return Argon2Params{
		Time:    3,
		Memory:  64 * 1024, // 64 MB
		Threads: 4,
		SaltLen: 16,
		KeyLen:  32, // AES-256
	}
}

// KitArgon2Params returns the parameters a recovery kit is sealed under.
//
// Deliberately eight times the memory of a vault password's. A vault password
// is typed several times a day and 64 MB is what that cadence can afford; a kit
// is opened once in several years, so a second and a half is a rounding error
// at the only moment it is ever paid.
//
// Against the recovery code SAND generates this is belt and braces — 120 bits
// of uniform randomness needs no stretching at all. It is here for the other
// case: a kit sealed under a password the user chose, which goes through the
// same envelope and needs every megabyte of it.
func KitArgon2Params() Argon2Params {
	return Argon2Params{
		Time:    8,
		Memory:  512 * 1024, // 512 MB
		Threads: 4,
		SaltLen: 16,
		KeyLen:  32,
	}
}

// GenerateSalt creates a cryptographically random salt.
func GenerateSalt(size uint32) ([]byte, error) {
	salt := make([]byte, size)
	if _, err := io.ReadFull(rand.Reader, salt); err != nil {
		return nil, fmt.Errorf("failed to generate salt: %w", err)
	}
	return salt, nil
}

// DeriveKey generates a 32-byte AES key from password + salt using Argon2id.
func DeriveKey(password string, salt []byte, params Argon2Params) []byte {
	return argon2.IDKey(
		[]byte(password),
		salt,
		params.Time,
		params.Memory,
		params.Threads,
		params.KeyLen,
	)
}

// NonceSize is the standard AES-GCM nonce size (12 bytes).
const NonceSize = 12

// GenerateNonce creates a cryptographically random nonce for AES-GCM.
func GenerateNonce() ([]byte, error) {
	nonce := make([]byte, NonceSize)
	if _, err := io.ReadFull(rand.Reader, nonce); err != nil {
		return nil, fmt.Errorf("failed to generate nonce: %w", err)
	}
	return nonce, nil
}

// Encrypt encrypts plaintext with AES-256-GCM.
// The nonce must be provided (12 bytes).
// associatedData is authenticated but not encrypted (can be nil).
// Returns ciphertext with GCM tag appended.
func Encrypt(key, nonce, plaintext, associatedData []byte) ([]byte, error) {
	if len(key) != 32 {
		return nil, errors.New("key must be 32 bytes for AES-256")
	}
	if len(nonce) != NonceSize {
		return nil, fmt.Errorf("nonce must be %d bytes", NonceSize)
	}

	block, err := aes.NewCipher(key)
	if err != nil {
		return nil, fmt.Errorf("failed to create AES cipher: %w", err)
	}

	gcm, err := cipher.NewGCM(block)
	if err != nil {
		return nil, fmt.Errorf("failed to create GCM: %w", err)
	}

	ciphertext := gcm.Seal(nil, nonce, plaintext, associatedData)
	return ciphertext, nil
}

// Decrypt decrypts AES-256-GCM ciphertext.
// The nonce must be the same one used during encryption.
// associatedData must match what was used during encryption.
// Returns the plaintext or an error if authentication fails.
func Decrypt(key, nonce, ciphertext, associatedData []byte) ([]byte, error) {
	if len(key) != 32 {
		return nil, errors.New("key must be 32 bytes for AES-256")
	}
	if len(nonce) != NonceSize {
		return nil, fmt.Errorf("nonce must be %d bytes", NonceSize)
	}

	block, err := aes.NewCipher(key)
	if err != nil {
		return nil, fmt.Errorf("failed to create AES cipher: %w", err)
	}

	gcm, err := cipher.NewGCM(block)
	if err != nil {
		return nil, fmt.Errorf("failed to create GCM: %w", err)
	}

	plaintext, err := gcm.Open(nil, nonce, ciphertext, associatedData)
	if err != nil {
		return nil, fmt.Errorf("decryption failed (wrong password or corrupted data): %w", err)
	}

	return plaintext, nil
}

// ZeroBytes overwrites a byte slice with zeros for security.
func ZeroBytes(b []byte) {
	for i := range b {
		b[i] = 0
	}
}

// DecryptInPlace is Decrypt writing the plaintext over the ciphertext it came
// from, rather than into a second buffer of the same length.
//
// It exists for one caller: rebuilding a file stored in SAND's pre-chunking
// format, where the "ciphertext" is half a film. GCM cannot release any of that
// plaintext before the tag over all of it verifies — which is why the format
// cannot be streamed and why chunking replaced it — but there is no reason to
// hold two copies of it while finding out. Reusing the buffer halves the peak.
//
// ciphertext is destroyed. The returned slice aliases it, and it is shorter by
// the tag, so a caller must not read the original afterwards.
func DecryptInPlace(key, nonce, ciphertext, associatedData []byte) ([]byte, error) {
	if len(key) != 32 {
		return nil, errors.New("key must be 32 bytes for AES-256")
	}
	if len(nonce) != NonceSize {
		return nil, fmt.Errorf("nonce must be %d bytes", NonceSize)
	}

	block, err := aes.NewCipher(key)
	if err != nil {
		return nil, fmt.Errorf("failed to create AES cipher: %w", err)
	}
	gcm, err := cipher.NewGCM(block)
	if err != nil {
		return nil, fmt.Errorf("failed to create GCM: %w", err)
	}

	// ciphertext[:0] is what tells crypto/cipher to write over the input. The
	// plaintext is always shorter than the ciphertext by the tag, so it fits.
	plaintext, err := gcm.Open(ciphertext[:0], nonce, ciphertext, associatedData)
	if err != nil {
		return nil, fmt.Errorf("decryption failed (wrong password or corrupted data): %w", err)
	}
	return plaintext, nil
}
