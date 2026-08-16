package compress

import (
	"fmt"
	"io"

	"github.com/klauspost/compress/zstd"
)

// DefaultLevel is the zstd compression level used by SAND.
const DefaultLevel = 3

// Compress compresses data using zstd at the default compression level.
func Compress(data []byte) ([]byte, error) {
	encoder, err := zstd.NewWriter(nil, zstd.WithEncoderLevel(zstd.EncoderLevel(DefaultLevel)))
	if err != nil {
		return nil, fmt.Errorf("failed to create zstd encoder: %w", err)
	}
	defer encoder.Close()

	return encoder.EncodeAll(data, make([]byte, 0, len(data))), nil
}

// Decompress decompresses zstd-compressed data.
func Decompress(data []byte) ([]byte, error) {
	decoder, err := zstd.NewReader(nil)
	if err != nil {
		return nil, fmt.Errorf("failed to create zstd decoder: %w", err)
	}
	defer decoder.Close()

	result, err := decoder.DecodeAll(data, nil)
	if err != nil {
		return nil, fmt.Errorf("zstd decompression failed: %w", err)
	}

	return result, nil
}

// DecompressStream decompresses r into w without either side being held whole.
//
// Decompress is the right shape when the payload is small and already in
// memory — a thumbnail pack, a manifest. It is the wrong shape for a film: the
// compressed form and the decompressed one are both allocated in full, so
// rebuilding a 4 GB video costs 8 GB before anything else has happened. This
// costs the decoder's window instead, whatever the length.
func DecompressStream(r io.Reader, w io.Writer) (int64, error) {
	decoder, err := zstd.NewReader(r)
	if err != nil {
		return 0, fmt.Errorf("failed to create zstd decoder: %w", err)
	}
	defer decoder.Close()

	n, err := io.Copy(w, decoder)
	if err != nil {
		return n, fmt.Errorf("zstd decompression failed: %w", err)
	}
	return n, nil
}
