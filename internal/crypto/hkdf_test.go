package crypto

import (
	"bytes"
	"encoding/hex"
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

// The derived key is the thing every stored chunk is sealed under, so it must
// not move — not when the HKDF implementation is swapped, not when the info
// string is tidied, not ever. These values were taken from the derivation as it
// first shipped; a change here means every chunk already on someone's accounts
// has become unreadable.
func TestDeriveChunkKeyMatchesItsGoldenValues(t *testing.T) {
	master := make([]byte, MasterKeySize)
	for i := range master {
		master[i] = byte(i)
	}
	var archiveID [16]byte
	for i := range archiveID {
		archiveID[i] = byte(0xA0 + i)
	}

	golden := map[uint32]string{
		0:    "138fc21e246cafa05b34fd24ab8d66538026c9cdf0a952aca5a19328cbbc0c59",
		1:    "f6a4498ece51c35b9f8dc9ab378b3e11038275068af09f02eb30936e96191680",
		4095: "a2c7b7fa3ab31a92800f8e9e2c6afcd303aa1b31554525e63527d332e7fdab85",
	}
	for index, want := range golden {
		key, err := DeriveChunkKey(master, archiveID, index)
		if err != nil {
			t.Fatalf("chunk %d: %v", index, err)
		}
		if got := hex.EncodeToString(key); got != want {
			t.Errorf("chunk %d derived %s, want %s — stored chunks would no longer open",
				index, got, want)
		}
	}
}

// The read history is sealed under a key derived this way, and a build that
// derived a different one would quietly forget every figure anybody had ever
// collected — the file would still be there and would simply refuse to open.
// Nothing about the derivation is allowed to drift without this failing first.
func TestDerivePurposeKeyMatchesItsGoldenValues(t *testing.T) {
	master := make([]byte, MasterKeySize)
	for i := range master {
		master[i] = byte(i)
	}

	golden := map[string]string{
		"sand-read-history-v1": "c9adf856411a85097269ea5482257791534a54c43ce3b03f0e5c58f9a415937b",
		"something-else":       "e8031d0e4bb73ad16068f51739c3da72594f7e7296e9212eabed9af31926523a",
	}
	for purpose, want := range golden {
		key, err := DerivePurposeKey(master, purpose)
		if err != nil {
			t.Fatalf("%s: %v", purpose, err)
		}
		if got := hex.EncodeToString(key); got != want {
			t.Errorf("%s derived %s, want %s — what it sealed would no longer open", purpose, got, want)
		}
	}

	// The purpose is the whole of the separation, so two of them must not meet.
	one, _ := DerivePurposeKey(master, "sand-read-history-v1")
	two, _ := DerivePurposeKey(master, "something-else")
	if hex.EncodeToString(one) == hex.EncodeToString(two) {
		t.Errorf("two purposes derived the same key")
	}

	// And a key derived for a purpose is not the master it came from.
	if hex.EncodeToString(one) == hex.EncodeToString(master) {
		t.Errorf("the derived key is the master key")
	}

	if _, err := DerivePurposeKey(master, ""); err == nil {
		t.Errorf("a key with no purpose to separate it was derived anyway")
	}
	if _, err := DerivePurposeKey(master[:8], "sand-read-history-v1"); err == nil {
		t.Errorf("a short master key was accepted")
	}
}
