// Package checksums reads and writes the vault's checksum ledger.
//
// The ledger is a gzip of plain text, one file per line:
//
//	/photos/2024/hike.jpg, 9f86d081884c7d659a2feaa0c55ad015a3bf4f1b2b0b822cd15d6c15b0f00a08
//
// the file's full path in the vault, a comma and a space, and the SHA-256 of
// its contents in lowercase hex. It is meant to be read by a person and by
// `zcat checksums.gz` as easily as by this package, which is why it is text
// and why there is nothing else in it.
//
// Two details keep "one file per line" true for any name a vault can hold. A
// path is split from its checksum at the *last* ", " on the line, so a comma
// in a name needs no quoting — the checksum is hex and can contain neither.
// And the three characters that would break a line or the escaping itself —
// newline, carriage return, backslash — are written as \n, \r and \\.
//
// Encoding is deterministic: entries are sorted by path and the gzip header
// carries no time, so the same files and checksums always produce the same
// bytes. That is what lets a caller tell "nothing changed" from the bytes
// alone and skip storing the ledger again.
package checksums

import (
	"bufio"
	"bytes"
	"compress/gzip"
	"encoding/hex"
	"fmt"
	"io"
	"sort"
	"strings"
)

// Name is what the ledger is called in the vault.
const Name = "checksums.gz"

// SumLen is the length of a checksum as written: SHA-256 in hex.
const SumLen = 64

// Entry is one file's line.
type Entry struct {
	Path string
	Sum  string
}

const separator = ", "

var (
	escaper   = strings.NewReplacer(`\`, `\\`, "\n", `\n`, "\r", `\r`)
	unescaper = strings.NewReplacer(`\\`, `\`, `\n`, "\n", `\r`, "\r")
)

// ValidSum reports whether s is a SHA-256 in lowercase hex.
func ValidSum(s string) bool {
	if len(s) != SumLen || strings.ToLower(s) != s {
		return false
	}
	_, err := hex.DecodeString(s)
	return err == nil
}

// Encode writes entries as a ledger. The input is not modified; an entry with
// an empty path or a checksum that is not one is an error rather than a line
// that would not read back.
func Encode(entries []Entry) ([]byte, error) {
	sorted := append([]Entry(nil), entries...)
	sort.Slice(sorted, func(i, j int) bool { return sorted[i].Path < sorted[j].Path })

	var buf bytes.Buffer
	zw, err := gzip.NewWriterLevel(&buf, gzip.BestCompression)
	if err != nil {
		return nil, err
	}
	w := bufio.NewWriter(zw)
	for _, e := range sorted {
		if e.Path == "" {
			return nil, fmt.Errorf("a ledger entry has no path")
		}
		if !ValidSum(e.Sum) {
			return nil, fmt.Errorf("%s: %q is not a SHA-256 checksum", e.Path, e.Sum)
		}
		w.WriteString(escaper.Replace(e.Path))
		w.WriteString(separator)
		w.WriteString(e.Sum)
		w.WriteByte('\n')
	}
	if err := w.Flush(); err != nil {
		return nil, err
	}
	if err := zw.Close(); err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
}

// Decode reads a ledger back, in the order it was written. A line that is not
// a path and a checksum is an error naming the line: a ledger that is partly
// unreadable must not be taken for a shorter one.
func Decode(data []byte) ([]Entry, error) {
	zr, err := gzip.NewReader(bytes.NewReader(data))
	if err != nil {
		return nil, fmt.Errorf("the ledger is not gzip: %w", err)
	}
	defer zr.Close()

	var out []Entry
	r := bufio.NewReader(zr)
	for n := 1; ; n++ {
		line, err := r.ReadString('\n')
		if err != nil && err != io.EOF {
			return nil, fmt.Errorf("reading the ledger: %w", err)
		}
		if line = strings.TrimSuffix(line, "\n"); line != "" {
			at := strings.LastIndex(line, separator)
			if at <= 0 || !ValidSum(line[at+len(separator):]) {
				return nil, fmt.Errorf("line %d of the ledger is not a path and a checksum", n)
			}
			out = append(out, Entry{Path: unescaper.Replace(line[:at]), Sum: line[at+len(separator):]})
		}
		if err == io.EOF {
			return out, nil
		}
	}
}
