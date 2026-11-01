// Package compress is the zstd step of the pipeline: every chunk is
// compressed before it is cut, and decompressed after it is rebuilt.
package compress

import (
	"fmt"
	"io"
	"sync/atomic"

	"github.com/klauspost/compress/zstd"
)

// DefaultLevel is the zstd speed level used until Configure says otherwise.
//
// It is the library's own scale rather than the zstd command's: 1 is
// fastest, 2 the library's default, 3 better compression, 4 best. Three is
// where SAND has always been, and changing it changes nothing about what is
// already stored — a compressed stream names its own format, and the
// decoder does not care what level produced it.
const DefaultLevel = 3

// DefaultThreads is how many threads one chunk's compression may use until
// Configure says otherwise. Two rather than every core: a 16 MiB chunk is
// small enough that more mostly adds goroutines, and an upload already runs
// a window of chunks abreast.
const DefaultThreads = 2

// Options is what Configure sets: the level and the threads per encoder.
type Options struct {
	Level   int
	Threads int
}

// settings is what the next encoder is built with. An atomic pointer so
// Configure can change it under a running upload; the chunk being encoded
// keeps the encoder it has, and the next one is built the new way.
var settings atomic.Pointer[Options]

func init() {
	settings.Store(&Options{Level: DefaultLevel, Threads: DefaultThreads})
}

// Configure sets the level and thread count every encoder built from now on
// uses. Out-of-range values are clamped rather than refused: this is the
// last hop of a setting that has already been validated, and a compressor
// that refused to compress would be a worse outcome than one a step off.
func Configure(o Options) {
	if o.Level < 1 {
		o.Level = 1
	}
	if o.Level > 4 {
		o.Level = 4
	}
	if o.Threads < 1 {
		o.Threads = 1
	}
	settings.Store(&o)
}

// Current reports what Configure last set.
func Current() Options {
	return *settings.Load()
}

// Compress compresses data at the configured level.
//
// An encoder is built for the call and closed after it, rather than kept.
// Keeping one was tried: an encoder holds its history window and block
// buffers between calls — tens of megabytes after a 16 MiB chunk, times the
// upload window — and on a Pi that retained memory showed up as a higher
// floor under every read that followed. Building one costs a few
// milliseconds of table setup per chunk, which against the compression of
// the chunk itself is noise, and leaves nothing behind.
func Compress(data []byte) ([]byte, error) {
	opts := Current()
	encoder, err := zstd.NewWriter(nil,
		zstd.WithEncoderLevel(zstd.EncoderLevel(opts.Level)),
		zstd.WithEncoderConcurrency(opts.Threads))
	if err != nil {
		return nil, fmt.Errorf("failed to create zstd encoder: %w", err)
	}
	defer encoder.Close()

	return encoder.EncodeAll(data, make([]byte, 0, len(data))), nil
}

// Decompress decompresses zstd-compressed data. The decoder is per call for
// the reason the encoder is: one kept between calls keeps a window's worth
// of history with it.
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
	dec, err := zstd.NewReader(r)
	if err != nil {
		return 0, fmt.Errorf("failed to create zstd decoder: %w", err)
	}
	defer dec.Close()

	n, err := io.Copy(w, dec)
	if err != nil {
		return n, fmt.Errorf("zstd decompression failed: %w", err)
	}
	return n, nil
}
