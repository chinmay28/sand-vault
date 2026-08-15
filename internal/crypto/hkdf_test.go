package crypto

import (
	"bytes"
	"testing"
)

func testMaster() []byte { return bytes.Repeat([]byte{0x7C}, MasterKeySize) }

func testArchiveID() [16]byte {
	return [16]byte{0x01, 0x02, 0x03, 0x04, 0x05, 0x06, 0x07, 0x08,
		0x09, 0x0A, 0x0B, 0x0C, 0x0D, 0x0E, 0x0F, 0x10}
}

func TestDeriveChunkKeyIsDeterministic(t *testing.T) {
	id := testArchiveID()

	first, err := DeriveChunkKey(testMaster(), id, 42)
	if err != nil {
		t.Fatalf("DeriveChunkKey: %v", err)
	}
	second, err := DeriveChunkKey(testMaster(), id, 42)
	if err != nil {
		t.Fatalf("DeriveChunkKey: %v", err)
	}

	if !bytes.Equal(first, second) {
		t.Error("same master, archive and index derived different keys")
	}
	if len(first) != 32 {
		t.Errorf("key is %d bytes, want 32 for AES-256", len(first))
	}
}

func TestDeriveChunkKeySeparatesChunks(t *testing.T) {
	id := testArchiveID()

	// Every chunk of a file must get its own key, so that recovering one
	// chunk's key says nothing about its neighbours.
	seen := map[string]uint32{}
	for index := uint32(0); index < 64; index++ {
		key, err := DeriveChunkKey(testMaster(), id, index)
		if err != nil {
			t.Fatalf("chunk %d: %v", index, err)
		}
		if prev, dup := seen[string(key)]; dup {
			t.Fatalf("chunks %d and %d derived the same key", prev, index)
		}
		seen[string(key)] = index
	}
}

func TestDeriveChunkKeySeparatesArchives(t *testing.T) {
	// The archive ID is the salt, so the same master key and index across two
	// files must not land on the same chunk key.
	first, err := DeriveChunkKey(testMaster(), testArchiveID(), 7)
	if err != nil {
		t.Fatalf("DeriveChunkKey: %v", err)
	}

	other := testArchiveID()
	other[15] ^= 0xFF
	second, err := DeriveChunkKey(testMaster(), other, 7)
	if err != nil {
		t.Fatalf("DeriveChunkKey: %v", err)
	}

	if bytes.Equal(first, second) {
		t.Error("different archive IDs derived the same chunk key")
	}
}

func TestDeriveChunkKeySeparatesMasters(t *testing.T) {
	id := testArchiveID()

	first, err := DeriveChunkKey(testMaster(), id, 7)
	if err != nil {
		t.Fatalf("DeriveChunkKey: %v", err)
	}

	other := testMaster()
	other[0] ^= 0xFF
	second, err := DeriveChunkKey(other, id, 7)
	if err != nil {
		t.Fatalf("DeriveChunkKey: %v", err)
	}

	if bytes.Equal(first, second) {
		t.Error("different master keys derived the same chunk key")
	}
}

func TestDeriveChunkKeyRejectsWrongMasterSize(t *testing.T) {
	id := testArchiveID()

	for _, size := range []int{0, 16, 31, 33, 64} {
		if _, err := DeriveChunkKey(bytes.Repeat([]byte{0x01}, size), id, 0); err == nil {
			t.Errorf("accepted a %d-byte master key, want an error", size)
		}
	}
}
