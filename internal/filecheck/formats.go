package filecheck

import (
	"bytes"
	"encoding/binary"
	"fmt"
	"hash/crc32"
)

// The per-format checks. Each returns what is wrong, or nothing.

// --- JPEG -----------------------------------------------------------------

// checkJPEG walks the header segments to the start of the image data, then
// looks for the end-of-image marker after it.
//
// The marker is searched for rather than expected at the last byte: cameras
// append things after it — a motion photo's whole film, for one — and a file
// that carries on past its end is not a damaged file. It has to come after the
// scan starts, though, because the thumbnail inside the EXIF block is a whole
// JPEG with an end marker of its own, and a photograph cut off in the middle
// would otherwise pass on the strength of its thumbnail.
func checkJPEG(s *src) []string {
	off := int64(2)
	for {
		seg := s.at(off, 4)
		if seg == nil {
			return []string{"it ends in the middle of its header, before any picture data"}
		}
		if seg[0] != 0xff {
			return []string{fmt.Sprintf("its header is unreadable %d bytes in", off)}
		}
		marker := seg[1]
		switch {
		case marker == 0xff: // fill byte
			off++
			continue
		case marker == 0xd8 || (marker >= 0xd0 && marker <= 0xd7) || marker == 0x01:
			off += 2
			continue
		case marker == 0xd9:
			return []string{"it ends before any picture data"}
		}
		length := int64(binary.BigEndian.Uint16(seg[2:]))
		if length < 2 {
			return []string{fmt.Sprintf("its header is unreadable %d bytes in", off)}
		}
		if marker == 0xda { // start of scan: picture data from here
			break
		}
		off += 2 + length
		if off >= s.size {
			return []string{"it ends in the middle of its header, before any picture data"}
		}
	}

	if !findBackwards(s, off, []byte{0xff, 0xd9}) {
		return []string{"it has no end-of-image marker — the picture is cut short"}
	}
	return nil
}

// findBackwards reports whether needle occurs at or after from, searching
// from the end of the file and giving up after scanLimit bytes.
func findBackwards(s *src, from int64, needle []byte) bool {
	const window = 64 << 10
	end := s.size
	for scanned := int64(0); end > from && scanned < scanLimit; {
		start := max(end-window, from)
		// Overlap by the needle, so one straddling two windows is still seen.
		buf := s.at(start, int(min(end+int64(len(needle))-1, s.size)-start))
		if buf == nil {
			return true // unreadable is reported by the caller, not as damage
		}
		if bytes.Contains(buf, needle) {
			return true
		}
		scanned += end - start
		end = start
	}
	// Past the limit without finding it: not proven missing.
	return end > from
}

// --- PNG ------------------------------------------------------------------

// checkPNG walks every chunk, checks each one's CRC, and wants IEND last.
func checkPNG(s *src) []string {
	off := int64(8)
	seenData := false
	for {
		hdr := s.at(off, 8)
		if hdr == nil {
			return []string{"it ends without its closing chunk — the picture is cut short"}
		}
		length := int64(binary.BigEndian.Uint32(hdr))
		kind := string(hdr[4:8])
		if off+12+length > s.size {
			return []string{fmt.Sprintf("it is cut short inside its %q chunk", printable(kind))}
		}
		body := s.at(off+8, int(length)+4)
		if body == nil {
			return nil
		}
		sum := crc32.NewIEEE()
		sum.Write(hdr[4:8])
		sum.Write(body[:length])
		if sum.Sum32() != binary.BigEndian.Uint32(body[length:]) {
			return []string{fmt.Sprintf("its %q chunk %d bytes in fails its checksum", printable(kind), off)}
		}
		switch kind {
		case "IDAT":
			seenData = true
		case "IEND":
			if !seenData {
				return []string{"it holds no picture data"}
			}
			return nil
		}
		off += 12 + length
	}
}

// --- GIF ------------------------------------------------------------------

func checkGIF(s *src) []string {
	if s.size < 14 {
		return []string{"it is too short to hold a picture"}
	}
	if tail := s.tail(16); tail != nil && !bytes.Contains(tail, []byte{0x3b}) {
		return []string{"it has no trailer — the picture is cut short"}
	}
	return nil
}

// --- RIFF: WebP, AVI, WAV ---------------------------------------------------

func riffForm(form string) func([]byte) bool {
	return func(h []byte) bool {
		return len(h) >= 12 && string(h[:4]) == "RIFF" && string(h[8:12]) == form
	}
}

func checkRIFF(s *src) []string {
	hdr := s.head(12)
	if hdr == nil {
		return nil
	}
	declared := int64(binary.LittleEndian.Uint32(hdr[4:8])) + 8
	// An AVI or WAV past 4 GiB cannot say its real length in this field, and
	// one being recorded says zero; only a length that was written and is
	// more than the file has is a finding.
	if declared > s.size && s.size < 1<<32 {
		return []string{fmt.Sprintf("it says it is %d bytes long and only %d are there — it is cut short",
			declared, s.size)}
	}
	return nil
}

// --- PDF ------------------------------------------------------------------

func isPDF(h []byte) bool {
	// The header may follow a few bytes of junk; readers allow it, so this does.
	return bytes.Contains(h, []byte("%PDF-"))
}

func checkPDF(s *src) []string {
	tail := s.tail(4096)
	if tail == nil {
		return nil
	}
	if !bytes.Contains(tail, []byte("%%EOF")) {
		return []string{"it has no end-of-file marker — the document is cut short"}
	}
	if !bytes.Contains(tail, []byte("startxref")) {
		return []string{"the table that says where its pages are is missing from its end"}
	}
	return nil
}

// --- ISO base media: MP4, QuickTime, HEIC, AVIF -----------------------------

// bmffTop is the box types a file of this family may open with.
var bmffTop = map[string]bool{
	"ftyp": true, "moov": true, "mdat": true, "wide": true, "free": true,
	"skip": true, "pnot": true, "styp": true,
}

func isBMFF(h []byte) bool {
	if len(h) < 12 || !bmffTop[string(h[4:8])] {
		return false
	}
	// The type alone is four letters that could be anywhere; a size that is a
	// box's — at least its own header, or the two special values — makes it one.
	size := binary.BigEndian.Uint32(h)
	return size >= 8 || size == 1 || (size == 0 && string(h[4:8]) == "mdat")
}

// bmffName reads the family member off the major brand.
func bmffName(h []byte) string {
	if len(h) < 12 || string(h[4:8]) != "ftyp" {
		return "QuickTime"
	}
	switch brand := string(h[8:12]); {
	case brand == "heic" || brand == "heix" || brand == "hevc" || brand == "hevx" ||
		brand == "heim" || brand == "heis" || brand == "mif1" || brand == "msf1":
		return "HEIC"
	case brand == "avif" || brand == "avis":
		return "AVIF"
	case brand == "qt  ":
		return "QuickTime"
	case brand == "M4A " || brand == "M4B ":
		return "M4A"
	default:
		return "MP4"
	}
}

// checkBMFF walks the top-level boxes and holds each to the room the file
// has for it, then wants the box that makes the file usable: the item index
// for a picture, the track index for a film.
//
// The second half is what catches the commonest broken film there is. A
// camera or a download writes the samples first and the index last, so a
// recording that never finished, or a copy that stopped early, is a file with
// gigabytes of mdat and no moov — every byte of it authentic, none of it
// playable.
func checkBMFF(s *src) []string {
	seen := map[string]bool{}
	last := ""
	off := int64(0)
	for off < s.size {
		hdr := s.at(off, 8)
		if hdr == nil {
			if s.size-off < 8 {
				// A few stray bytes after the last box are tolerated by
				// every reader, and written by some muxers.
				break
			}
			return nil
		}
		size := int64(binary.BigEndian.Uint32(hdr))
		kind := string(hdr[4:8])
		header := int64(8)
		switch size {
		case 0: // runs to the end of the file
			size = s.size - off
		case 1:
			ext := s.at(off+8, 8)
			if ext == nil {
				return []string{fmt.Sprintf("it is cut short inside its %q box", printable(kind))}
			}
			size = int64(binary.BigEndian.Uint64(ext))
			header = 16
		}
		if !boxType(kind) || size < header {
			if last == "" {
				return []string{"its first box is unreadable"}
			}
			return []string{fmt.Sprintf("its structure is unreadable %d bytes in, after the %q box",
				off, last)}
		}
		if off+size > s.size {
			return []string{fmt.Sprintf(
				"its %q box says it is %d bytes and only %d are there — the file is cut short",
				kind, size, s.size-off)}
		}
		seen[kind] = true
		last = kind
		off += size
	}

	head := s.head(12)
	switch bmffName(head) {
	case "HEIC", "AVIF":
		if !seen["meta"] && !seen["moov"] {
			return []string{"it has no index of the picture it holds (no meta box)"}
		}
	default:
		if !seen["moov"] {
			if seen["mdat"] {
				return []string{"it has its sound and picture data but no index to play them by " +
					"(no moov box) — the recording or the copy it came from never finished"}
			}
			return []string{"it has no index to play it by (no moov box)"}
		}
	}
	return nil
}

func boxType(kind string) bool {
	for i := 0; i < len(kind); i++ {
		// Printable ASCII, plus the © that QuickTime's metadata atoms open with.
		if c := kind[i]; (c < 0x20 || c > 0x7e) && c != 0xa9 {
			return false
		}
	}
	return true
}

// --- Matroska / WebM -------------------------------------------------------

// checkMatroska reads past the EBML header to the Segment and, where the
// Segment says how long it is, holds the file to that.
func checkMatroska(s *src) []string {
	off := int64(4)
	size, n := vint(s, off)
	if n == 0 {
		return []string{"its header is unreadable"}
	}
	off += int64(n) + size
	id := s.at(off, 4)
	if id == nil {
		return []string{"it ends after its header, before any of the film"}
	}
	if !bytes.Equal(id, []byte{0x18, 0x53, 0x80, 0x67}) {
		return []string{"what follows its header is not a film segment"}
	}
	size, n = vint(s, off+4)
	if n == 0 {
		return []string{"it ends after its header, before any of the film"}
	}
	if size < 0 { // unknown length: a live recording, legitimately
		return nil
	}
	if end := off + 4 + int64(n) + size; end > s.size {
		return []string{fmt.Sprintf(
			"it says its film runs to byte %d and the file ends at %d — it is cut short", end, s.size)}
	}
	return nil
}

// vint reads an EBML variable-length size at off: the value and how many bytes
// it took, zero for unreadable. A size with every value bit set means
// "unknown" and comes back as −1.
func vint(s *src, off int64) (int64, int) {
	first := s.at(off, 1)
	if first == nil || first[0] == 0 {
		return 0, 0
	}
	n := 1
	for mask := byte(0x80); first[0]&mask == 0; mask >>= 1 {
		n++
	}
	buf := s.at(off, n)
	if buf == nil {
		return 0, 0
	}
	value := int64(buf[0] & (0xff >> n))
	unknown := value == int64(0xff>>n)
	for _, b := range buf[1:] {
		value = value<<8 | int64(b)
		unknown = unknown && b == 0xff
	}
	if unknown {
		return -1, n
	}
	return value, n
}

// --- ZIP, and everything that is one: docx, xlsx, pptx, epub, odt -----------

// checkZIP finds the end-of-central-directory record and holds the directory
// it points at to being inside the file.
func checkZIP(s *src) []string {
	const eocdMin, commentMax = 22, 65535
	tail := s.tail(eocdMin + commentMax)
	if tail == nil {
		return nil
	}
	at := bytes.LastIndex(tail, []byte("PK\x05\x06"))
	if at < 0 || len(tail)-at < eocdMin {
		return []string{"it has no table of contents at its end — the file is cut short"}
	}
	eocd := tail[at:]
	dirSize := int64(binary.LittleEndian.Uint32(eocd[12:16]))
	dirOff := int64(binary.LittleEndian.Uint32(eocd[16:20]))
	if dirOff == 0xffffffff || dirSize == 0xffffffff {
		return nil // ZIP64: the real figures are elsewhere, and present
	}
	eocdPos := s.size - int64(len(tail)) + int64(at)
	if dirOff+dirSize > eocdPos {
		return []string{"its table of contents points past where the file's contents end — part of it is missing"}
	}
	if dirSize > 0 {
		if sig := s.at(dirOff, 4); sig != nil && !bytes.Equal(sig, []byte("PK\x01\x02")) {
			return []string{"its table of contents is not where the file says it is"}
		}
	}
	return nil
}

// --- TIFF, and the raw formats built on it -----------------------------------

func checkTIFF(s *src) []string {
	hdr := s.head(8)
	if hdr == nil {
		return []string{"it is too short to hold a picture"}
	}
	order := binary.ByteOrder(binary.LittleEndian)
	if hdr[0] == 'M' {
		order = binary.BigEndian
	}
	if dir := int64(order.Uint32(hdr[4:8])); dir < 8 || dir+2 > s.size {
		return []string{"its picture directory lies past the end of the file — it is cut short"}
	}
	return nil
}

// --- BMP ------------------------------------------------------------------

func isBMP(h []byte) bool {
	// "BM" alone is two letters; the reserved fields being zero makes it a header.
	return len(h) >= 14 && h[0] == 'B' && h[1] == 'M' && binary.LittleEndian.Uint32(h[6:10]) == 0
}

func checkBMP(s *src) []string {
	hdr := s.head(14)
	if hdr == nil {
		return nil
	}
	if declared := int64(binary.LittleEndian.Uint32(hdr[2:6])); declared > s.size {
		return []string{fmt.Sprintf("it says it is %d bytes long and only %d are there — it is cut short",
			declared, s.size)}
	}
	return nil
}

// printable makes a four-character code safe to put in a sentence.
func printable(kind string) string {
	out := []byte(kind)
	for i, c := range out {
		if c < 0x20 || c > 0x7e {
			out[i] = '?'
		}
	}
	return string(out)
}
