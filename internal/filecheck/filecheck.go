// Package filecheck says whether a file's bytes are a well-formed file of the
// kind they claim to be.
//
// It exists for one question the vault cannot answer from its own records. A
// file that rebuilds to the hash recorded at upload is, as far as SAND is
// concerned, perfect — and can still be a photograph cut off halfway, because
// that is what was handed over. Telling "the vault damaged this" from "this
// arrived damaged" needs somebody to look at the bytes as a JPEG or a PDF or
// a film rather than as bytes, and this is that look.
//
// It is a structural check, not a decoder. Each format is held to the handful
// of things that are cheap to read and that damage reliably breaks: the magic
// at the front, the lengths its containers declare against the length the
// file really has, and the marker a complete file ends with. That catches
// what happens to files in practice — truncation, a zeroed head, a different
// file under the name — and deliberately does not claim more. A verdict of
// Sound means "nothing structurally wrong was found", not "every pixel
// decodes".
//
// The contract is small: Check never returns an error and never panics on
// hostile input; a read that fails is reported in the verdict; nothing is
// read past size; and the cost is bounded by scanLimit however large the file
// is, apart from PNG, whose every chunk carries a checksum worth verifying.
package filecheck

import (
	"fmt"
	"io"
	"path"
	"strings"
)

// Status is what a check came to.
type Status string

const (
	// Sound: recognised, and nothing wrong was found with its structure.
	Sound Status = "sound"
	// Damaged: recognised — by its bytes or, failing that, by a name that
	// promises a format with an unmistakable signature — and broken.
	Damaged Status = "damaged"
	// Unknown: not a format this package knows how to judge. Text, archives
	// of formats without a signature, anything else. Not a finding.
	Unknown Status = "unknown"
)

// Verdict is the result of checking one file.
type Verdict struct {
	// Format names what the bytes are, in the word somebody would use for it:
	// "JPEG", "HEIC", "PDF", "MP4". Empty when unrecognised.
	Format string `json:"format,omitempty"`

	Status Status `json:"status"`

	// Problems says what is wrong, one sentence each, when Status is Damaged.
	Problems []string `json:"problems,omitempty"`

	// Note is something worth saying that is not damage — a file whose name
	// says one format and whose bytes are another, intact.
	Note string `json:"note,omitempty"`
}

// scanLimit bounds how much of a file a search for a marker reads.
const scanLimit = 32 << 20

// src is the file being checked: a ReaderAt that knows its length and
// remembers the first read that failed, so the format checkers can read
// without each of them threading an error.
type src struct {
	r    io.ReaderAt
	size int64
	err  error
}

// at reads n bytes at off, or nil when they are not all there.
func (s *src) at(off int64, n int) []byte {
	if off < 0 || n < 0 || off+int64(n) > s.size {
		return nil
	}
	buf := make([]byte, n)
	if _, err := io.ReadFull(io.NewSectionReader(s.r, off, int64(n)), buf); err != nil {
		if s.err == nil {
			s.err = err
		}
		return nil
	}
	return buf
}

// head reads up to n bytes from the start, and tail up to n from the end.
func (s *src) head(n int) []byte { return s.at(0, int(min(int64(n), s.size))) }
func (s *src) tail(n int) []byte {
	n = int(min(int64(n), s.size))
	return s.at(s.size-int64(n), n)
}

// format is one kind of file: how to recognise it from its first bytes, and
// how to hold the rest of the file to what that promises.
type format struct {
	name  string
	match func(head []byte) bool
	check func(s *src) []string
}

// formats is searched in order, so the more specific signature goes first.
var formats = []format{
	{"JPEG", hasPrefix("\xff\xd8\xff"), checkJPEG},
	{"PNG", hasPrefix("\x89PNG\r\n\x1a\n"), checkPNG},
	{"GIF", func(h []byte) bool { return hasPrefix("GIF87a")(h) || hasPrefix("GIF89a")(h) }, checkGIF},
	{"WebP", riffForm("WEBP"), checkRIFF},
	{"AVI", riffForm("AVI "), checkRIFF},
	{"WAV", riffForm("WAVE"), checkRIFF},
	{"PDF", isPDF, checkPDF},
	{"ISO media", isBMFF, checkBMFF}, // named properly by bmffName
	{"Matroska", hasPrefix("\x1a\x45\xdf\xa3"), checkMatroska},
	{"ZIP", func(h []byte) bool { return hasPrefix("PK\x03\x04")(h) || hasPrefix("PK\x05\x06")(h) }, checkZIP},
	{"Office document", hasPrefix("\xd0\xcf\x11\xe0\xa1\xb1\x1a\xe1"), nil},
	{"TIFF", func(h []byte) bool { return hasPrefix("II*\x00")(h) || hasPrefix("MM\x00*")(h) }, checkTIFF},
	{"BMP", isBMP, checkBMP},
	{"FLAC", hasPrefix("fLaC"), nil},
	{"Ogg", hasPrefix("OggS"), nil},
	{"MP3", func(h []byte) bool { return hasPrefix("ID3")(h) }, nil},
	{"MPEG", func(h []byte) bool { return hasPrefix("\x00\x00\x01\xba")(h) || hasPrefix("\x00\x00\x01\xb3")(h) }, nil},
}

// expected is, by extension, the formats a file of that name may honestly be —
// only for formats whose signature is unmistakable, so that "named .jpg and
// does not start like one" is a finding rather than a guess.
var expected = map[string][]string{
	".jpg": {"JPEG"}, ".jpeg": {"JPEG"}, ".png": {"PNG"}, ".gif": {"GIF"},
	".webp": {"WebP"}, ".heic": {"HEIC"}, ".heif": {"HEIC"}, ".avif": {"AVIF"},
	".tif": {"TIFF"}, ".tiff": {"TIFF"}, ".dng": {"TIFF"}, ".bmp": {"BMP"},
	".pdf":  {"PDF"},
	".docx": {"ZIP"}, ".xlsx": {"ZIP"}, ".pptx": {"ZIP"}, ".epub": {"ZIP"},
	".odt": {"ZIP"}, ".ods": {"ZIP"}, ".odp": {"ZIP"}, ".zip": {"ZIP"},
	".doc": {"Office document"}, ".xls": {"Office document"}, ".ppt": {"Office document"},
	".mp4": {"MP4", "QuickTime"}, ".m4v": {"MP4", "QuickTime"}, ".mov": {"QuickTime", "MP4"},
	".3gp": {"MP4"}, ".m4a": {"MP4", "M4A"},
	".mkv": {"Matroska"}, ".webm": {"Matroska"}, ".avi": {"AVI"},
	".wav": {"WAV"}, ".flac": {"FLAC"}, ".ogg": {"Ogg"}, ".opus": {"Ogg"},
}

// Check examines size bytes of r as the file called name.
func Check(r io.ReaderAt, size int64, name string) Verdict {
	if size <= 0 {
		return Verdict{Status: Unknown, Note: "the file is empty"}
	}
	s := &src{r: r, size: size}
	head := s.head(64)
	if head == nil {
		return unreadable(s)
	}

	ext := strings.ToLower(path.Ext(name))
	want := expected[ext]

	for _, f := range formats {
		if !f.match(head) {
			continue
		}
		v := Verdict{Format: f.name, Status: Sound}
		if f.name == "ISO media" {
			v.Format = bmffName(head)
		}
		if f.check != nil {
			v.Problems = f.check(s)
		}
		if s.err != nil {
			return unreadable(s)
		}
		if len(v.Problems) > 0 {
			v.Status = Damaged
		}
		if len(want) > 0 && !contains(want, v.Format) {
			v.Note = fmt.Sprintf("it is named %s but is a %s file", ext, v.Format)
		}
		return v
	}

	// Nothing recognised. That is only a finding when the name promised a
	// format that always announces itself.
	if len(want) == 0 {
		return Verdict{Status: Unknown}
	}
	v := Verdict{Format: want[0], Status: Damaged}
	if zeroes := leadingZeroes(s); zeroes >= len(head) {
		v.Problems = []string{fmt.Sprintf(
			"it begins with %s of zero bytes where a %s file's header should be",
			atLeast(zeroes), want[0])}
	} else {
		v.Problems = []string{fmt.Sprintf(
			"it is named %s but does not begin the way a %s file does", ext, want[0])}
	}
	if s.err != nil {
		return unreadable(s)
	}
	return v
}

func unreadable(s *src) Verdict {
	return Verdict{Status: Unknown, Note: fmt.Sprintf("it could not be read to be checked: %v", s.err)}
}

// leadingZeroes counts the zero bytes a file opens with, up to 64 KiB.
func leadingZeroes(s *src) int {
	buf := s.head(64 << 10)
	for i, b := range buf {
		if b != 0 {
			return i
		}
	}
	return len(buf)
}

func atLeast(n int) string {
	if n >= 64<<10 {
		return "at least 64 KB"
	}
	return fmt.Sprintf("%d bytes", n)
}

func hasPrefix(magic string) func([]byte) bool {
	return func(h []byte) bool { return len(h) >= len(magic) && string(h[:len(magic)]) == magic }
}

func contains(list []string, s string) bool {
	for _, item := range list {
		if item == s {
			return true
		}
	}
	return false
}
