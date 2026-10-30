package sandfile

import "testing"

func TestMinPartSizeIsWhatTheSmallestPartWeighs(t *testing.T) {
	smallest := 0
	for _, version := range []uint8{FormatVersion, ChunkedFormatVersion, ErasureFormatVersion} {
		h := makeTestHeader(1)
		h.Version = version
		if version == ErasureFormatVersion {
			h.DataShards, h.TotalShards = 2, 3
		}
		meta := makeTestMetadata("empty.bin")
		if version != FormatVersion {
			// A chunked layout describes its chunks even when there is one.
			meta.ChunkCount, meta.ChunkSize = 1, 1
		}
		blob, err := Seal(h, meta, nil, testKey())
		if err != nil {
			t.Fatalf("Seal version %d: %v", version, err)
		}
		if len(blob) < MinPartSize {
			t.Errorf("an empty version %d part is %d bytes, under MinPartSize %d", version, len(blob), MinPartSize)
		}
		if smallest == 0 || len(blob) < smallest {
			smallest = len(blob)
		}
	}
	// A real part carries its metadata in the payload too, so even an empty
	// one weighs more than the floor; the floor only has to be one no part
	// can be under.
	if smallest < MinPartSize {
		t.Errorf("the smallest empty part is %d bytes, under MinPartSize %d", smallest, MinPartSize)
	}
	if _, err := ReadPart(make([]byte, MinPartSize-1)); err == nil {
		t.Error("a blob one byte under MinPartSize parsed as a part")
	}
}
