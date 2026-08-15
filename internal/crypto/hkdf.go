package crypto

import (
	"crypto/sha256"
	"encoding/binary"
	"fmt"
	"io"

	"golang.org/x/crypto/hkdf"
)

// MasterKeySize is the length of the key material chunk keys are derived from.
// It is the size of the vault's random data key.
const MasterKeySize = 32

// chunkKeyLabel domain-separates chunk keys from any other use of the same
// master key. It is part of the HKDF info string, alongside the chunk index.
const chunkKeyLabel = "sand-chunk-key-v3"

// DeriveChunkKey produces the AES-256 key for one chunk of one archive.
//
// It is HKDF-SHA256 rather than Argon2id, and that is not a weakening. Argon2
// exists to make guessing a low-entropy password expensive; master here is the
// vault's data key, which is 256 bits of uniform randomness that nobody typed
// and nobody can guess. Stretching it buys nothing, and the whole-file format
// paid roughly 100 ms and 64 MB per part for that nothing — a cost a chunked
// file would pay thousands of times over.
//
// The archive ID is the salt, so two files never derive the same chunk key even
// from the same master. The chunk index is in the info string, so every chunk of
// a file gets a distinct key: an attacker who somehow recovers one chunk's key
// learns nothing about its neighbours.
//
// Passwords are still Argon2id everywhere they are actually passwords — the
// vault key in §3.1 and standalone mode both derive through DeriveKey.
func DeriveChunkKey(master []byte, archiveID [16]byte, chunkIndex uint32) ([]byte, error) {
	if len(master) != MasterKeySize {
		return nil, fmt.Errorf("master key must be %d bytes, got %d", MasterKeySize, len(master))
	}

	info := make([]byte, 0, len(chunkKeyLabel)+4)
	info = append(info, chunkKeyLabel...)
	info = binary.BigEndian.AppendUint32(info, chunkIndex)

	key := make([]byte, 32)
	if _, err := io.ReadFull(hkdf.New(sha256.New, master, archiveID[:], info), key); err != nil {
		return nil, fmt.Errorf("deriving key for chunk %d: %w", chunkIndex, err)
	}
	return key, nil
}
