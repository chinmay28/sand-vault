package archive

import (
	"bytes"
	"crypto/rand"
	"crypto/sha256"
	"testing"
)

// openAll opens every blob, failing the test on one that will not.
func openAll(t *testing.T, blobs [][]byte) []*OpenedShard {
	t.Helper()
	out := make([]*OpenedShard, 0, len(blobs))
	for i, blob := range blobs {
		s, err := OpenShard(blob, testMaster())
		if err != nil {
			t.Fatalf("OpenShard(%d): %v", i+1, err)
		}
		out = append(out, s)
	}
	return out
}

// without is blobs with the shard numbered part (1-based) left out.
func without(blobs [][]byte, part int) [][]byte {
	out := make([][]byte, 0, len(blobs)-1)
	for i, blob := range blobs {
		if i != part-1 {
			out = append(out, blob)
		}
	}
	return out
}

// Any one shard, lost, comes back from the others: it opens as the part it
// replaces and rebuilds the chunk alongside each of the survivors.
func TestRemakeShardStandsInForTheOneThatWasLost(t *testing.T) {
	plaintext := make([]byte, 5000)
	if _, err := rand.Read(plaintext); err != nil {
		t.Fatalf("rand: %v", err)
	}

	for _, scheme := range []Scheme{SchemeDefault, {Data: 4, Total: 6}} {
		plan, err := PlanChunks(testArchiveID(t), "film.mkv", sha256.Sum256(plaintext),
			uint64(len(plaintext)), 8192, scheme)
		if err != nil {
			t.Fatalf("%s: PlanChunks: %v", scheme, err)
		}
		encoded, err := EncodeChunk(plan, 0, plaintext, testMaster())
		if err != nil {
			t.Fatalf("%s: EncodeChunk: %v", scheme, err)
		}

		for lost := 1; lost <= scheme.Total; lost++ {
			survivors := without(encoded.Parts, lost)
			// Exactly as many as a rebuild needs, taken from the far end so
			// that parity shards do the rebuilding as often as data ones.
			from := survivors[len(survivors)-scheme.Data:]

			blob, err := RemakeShard(openAll(t, from), lost, testMaster())
			if err != nil {
				t.Fatalf("%s: RemakeShard(%d): %v", scheme, lost, err)
			}
			remade, err := OpenShard(blob, testMaster())
			if err != nil {
				t.Fatalf("%s: the remade shard %d does not open: %v", scheme, lost, err)
			}
			if got := int(remade.Header.PartNumber); got != lost {
				t.Errorf("%s: remade as part %d, want %d", scheme, got, lost)
			}
			if remade.Scheme() != scheme {
				t.Errorf("%s: remade under %s", scheme, remade.Scheme())
			}
			original := openAll(t, [][]byte{encoded.Parts[lost-1]})[0]
			if !bytes.Equal(remade.Data, original.Data) {
				t.Errorf("%s: remade shard %d carries a different share than the one it replaces", scheme, lost)
			}

			// With the first Data−1 survivors it has to give the chunk back.
			set := append([][]byte{blob}, survivors[:scheme.Data-1]...)
			decoded, err := DecodeChunk(set, testMaster())
			if err != nil {
				t.Fatalf("%s: rebuilding with remade shard %d: %v", scheme, lost, err)
			}
			if !bytes.Equal(decoded.Data, plaintext) {
				t.Errorf("%s: remade shard %d rebuilt the wrong bytes", scheme, lost)
			}
		}
	}
}

// A vault written before schemes existed holds version 3 chunks — two halves
// and their XOR — and a part lost from one of those is remade as version 3.
// The odd length is the case that matters: it is the one that carries a pad.
func TestRemakeShardKeepsAVersionThreeChunkVersionThree(t *testing.T) {
	for _, size := range []int{3000, 3001} {
		plaintext := make([]byte, size)
		if _, err := rand.Read(plaintext); err != nil {
			t.Fatalf("rand: %v", err)
		}
		plan, err := PlanChunks(testArchiveID(t), "old.bin", sha256.Sum256(plaintext),
			uint64(len(plaintext)), 4096, SchemeDefault)
		if err != nil {
			t.Fatalf("PlanChunks: %v", err)
		}
		parts := encodeChunkV3(t, plan, 0, plaintext, testMaster())

		for lost := 1; lost <= 3; lost++ {
			survivors := without(parts, lost)
			blob, err := RemakeShard(openAll(t, survivors), lost, testMaster())
			if err != nil {
				t.Fatalf("size %d: RemakeShard(%d): %v", size, lost, err)
			}
			remade, err := OpenShard(blob, testMaster())
			if err != nil {
				t.Fatalf("size %d: remade shard %d does not open: %v", size, lost, err)
			}
			if !remade.Legacy() {
				t.Errorf("size %d: shard %d was remade in the new format", size, lost)
			}
			for _, other := range survivors {
				decoded, err := DecodeChunk([][]byte{blob, other}, testMaster())
				if err != nil {
					t.Fatalf("size %d: rebuilding with remade shard %d: %v", size, lost, err)
				}
				if !bytes.Equal(decoded.Data, plaintext) {
					t.Errorf("size %d: remade shard %d rebuilt the wrong bytes", size, lost)
				}
			}
		}
	}
}

func TestRemakeShardRefusesWhatItCannotMake(t *testing.T) {
	plaintext := make([]byte, 2000)
	if _, err := rand.Read(plaintext); err != nil {
		t.Fatalf("rand: %v", err)
	}
	plan, err := PlanChunks(testArchiveID(t), "a.bin", sha256.Sum256(plaintext),
		uint64(len(plaintext)), 4096, SchemeDefault)
	if err != nil {
		t.Fatalf("PlanChunks: %v", err)
	}
	encoded, err := EncodeChunk(plan, 0, plaintext, testMaster())
	if err != nil {
		t.Fatalf("EncodeChunk: %v", err)
	}
	shards := openAll(t, encoded.Parts)

	if _, err := RemakeShard(shards[:1], 2, testMaster()); err == nil {
		t.Error("remade a shard from fewer shards than the code rebuilds from")
	}
	for _, part := range []int{0, 4, -1} {
		if _, err := RemakeShard(shards, part, testMaster()); err == nil {
			t.Errorf("remade shard %d of a 2-of-3 chunk", part)
		}
	}
	if _, err := RemakeShard(nil, 1, testMaster()); err == nil {
		t.Error("remade a shard from nothing")
	}
}
