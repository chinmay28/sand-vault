package filecheck

import (
	"archive/zip"
	"bytes"
	"encoding/binary"
	"errors"
	"image"
	"image/color"
	"image/gif"
	"image/jpeg"
	"image/png"
	"math/rand"
	"strings"
	"testing"
)

func check(data []byte, name string) Verdict {
	return Check(bytes.NewReader(data), int64(len(data)), name)
}

func picture() image.Image {
	img := image.NewRGBA(image.Rect(0, 0, 64, 48))
	rng := rand.New(rand.NewSource(1))
	for y := 0; y < 48; y++ {
		for x := 0; x < 64; x++ {
			img.Set(x, y, color.RGBA{uint8(rng.Intn(256)), uint8(x * 4), uint8(y * 5), 255})
		}
	}
	return img
}

func jpegBytes(t *testing.T) []byte {
	t.Helper()
	var buf bytes.Buffer
	if err := jpeg.Encode(&buf, picture(), nil); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

func pngBytes(t *testing.T) []byte {
	t.Helper()
	var buf bytes.Buffer
	if err := png.Encode(&buf, picture()); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

// box is one ISO media box.
func box(kind string, body []byte) []byte {
	out := make([]byte, 8, 8+len(body))
	binary.BigEndian.PutUint32(out, uint32(8+len(body)))
	copy(out[4:], kind)
	return append(out, body...)
}

func mp4Bytes(brand string, boxes ...[]byte) []byte {
	out := box("ftyp", []byte(brand+"\x00\x00\x00\x00isom"))
	for _, b := range boxes {
		out = append(out, b...)
	}
	return out
}

func zipBytes(t *testing.T) []byte {
	t.Helper()
	var buf bytes.Buffer
	w := zip.NewWriter(&buf)
	f, _ := w.Create("word/document.xml")
	f.Write(bytes.Repeat([]byte("<w:p>hello</w:p>"), 200))
	if err := w.Close(); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

func pdfBytes() []byte {
	return []byte("%PDF-1.7\n1 0 obj\n<< /Type /Catalog >>\nendobj\n" +
		strings.Repeat("% filler\n", 600) +
		"xref\n0 1\n0000000000 65535 f \ntrailer\n<< /Root 1 0 R >>\nstartxref\n5500\n%%EOF\n")
}

func mkvBytes(segment int, declared int) []byte {
	out := []byte{0x1a, 0x45, 0xdf, 0xa3, 0x84, 0x42, 0x86, 0x81, 0x01} // EBML header, 4 bytes of body
	out = append(out, 0x18, 0x53, 0x80, 0x67)
	out = append(out, 0x10, byte(declared>>16), byte(declared>>8), byte(declared)) // 4-byte size
	return append(out, make([]byte, segment)...)
}

func TestSoundFilesAreSound(t *testing.T) {
	var gifBuf bytes.Buffer
	if err := gif.Encode(&gifBuf, picture(), nil); err != nil {
		t.Fatal(err)
	}
	cases := []struct {
		name   string
		data   []byte
		format string
	}{
		{"a.jpg", jpegBytes(t), "JPEG"},
		{"a.png", pngBytes(t), "PNG"},
		{"a.gif", gifBuf.Bytes(), "GIF"},
		{"a.pdf", pdfBytes(), "PDF"},
		{"a.docx", zipBytes(t), "ZIP"},
		{"a.mp4", mp4Bytes("isom", box("moov", make([]byte, 100)), box("mdat", make([]byte, 5000))), "MP4"},
		{"a.mov", mp4Bytes("qt  ", box("mdat", make([]byte, 5000)), box("moov", make([]byte, 100))), "QuickTime"},
		{"IMG_0014.HEIC", mp4Bytes("heic", box("meta", make([]byte, 300)), box("mdat", make([]byte, 5000))), "HEIC"},
		{"a.avif", mp4Bytes("avif", box("meta", make([]byte, 300)), box("mdat", make([]byte, 500))), "AVIF"},
		{"a.mkv", mkvBytes(4000, 4000), "Matroska"},
		{"a.tif", append([]byte("II*\x00\x08\x00\x00\x00"), make([]byte, 64)...), "TIFF"},
	}
	for _, c := range cases {
		v := check(c.data, c.name)
		if v.Status != Sound || v.Format != c.format {
			t.Errorf("%s: got %+v, want sound %s", c.name, v, c.format)
		}
	}
}

// Truncation is the damage files actually suffer, and every format with an
// end to check must notice losing its own.
func TestTruncatedFilesAreDamaged(t *testing.T) {
	mp4 := mp4Bytes("isom", box("moov", make([]byte, 100)), box("mdat", make([]byte, 5000)))
	heic := mp4Bytes("heic", box("meta", make([]byte, 300)), box("mdat", make([]byte, 5000)))
	cases := map[string][]byte{
		"a.jpg":  jpegBytes(t),
		"a.png":  pngBytes(t),
		"a.pdf":  pdfBytes(),
		"a.docx": zipBytes(t),
		"a.mp4":  mp4,
		"a.heic": heic,
		"a.mkv":  mkvBytes(4000, 4000),
	}
	for name, data := range cases {
		for _, keep := range []int{len(data) / 2, len(data) - 3} {
			v := check(data[:keep], name)
			if v.Status != Damaged || len(v.Problems) == 0 {
				t.Errorf("%s cut to %d of %d bytes: got %+v, want damaged", name, keep, len(data), v)
			}
		}
	}
}

func TestJPEGWithTrailingDataIsSound(t *testing.T) {
	// A motion photo: a whole film appended after the picture's end marker.
	data := append(jpegBytes(t), bytes.Repeat([]byte{0x11}, 200<<10)...)
	if v := check(data, "motion.jpg"); v.Status != Sound {
		t.Errorf("got %+v, want sound", v)
	}
}

func TestJPEGThumbnailDoesNotVouchForATruncatedPicture(t *testing.T) {
	full := jpegBytes(t)
	// An APP1 segment holding a complete JPEG — the EXIF thumbnail — ahead of
	// a picture that is then cut off.
	thumb := jpegBytes(t)
	app1 := []byte{0xff, 0xe1, byte((len(thumb) + 2) >> 8), byte(len(thumb) + 2)}
	data := append([]byte{0xff, 0xd8}, app1...)
	data = append(data, thumb...)
	data = append(data, full[2:len(full)-40]...)
	if v := check(data, "a.jpg"); v.Status != Damaged {
		t.Errorf("got %+v, want damaged", v)
	}
}

func TestPNGChecksumFailureIsDamaged(t *testing.T) {
	data := pngBytes(t)
	data[len(data)/2] ^= 0x40
	v := check(data, "a.png")
	if v.Status != Damaged || !strings.Contains(v.Problems[0], "checksum") {
		t.Errorf("got %+v, want a checksum failure", v)
	}
}

func TestFilmWithoutItsIndexIsDamaged(t *testing.T) {
	v := check(mp4Bytes("isom", box("mdat", make([]byte, 9000))), "clip.mp4")
	if v.Status != Damaged || !strings.Contains(v.Problems[0], "moov") {
		t.Errorf("got %+v, want a missing moov", v)
	}
}

func TestZeroedHeadIsDamaged(t *testing.T) {
	data := jpegBytes(t)
	for i := 0; i < 512 && i < len(data); i++ {
		data[i] = 0
	}
	v := check(data, "a.jpg")
	if v.Status != Damaged || !strings.Contains(v.Problems[0], "zero") {
		t.Errorf("got %+v, want a zeroed head", v)
	}
}

func TestNameAndBytesDisagreeing(t *testing.T) {
	// A phone that saves a JPEG under .heic has made an odd file, not a broken one.
	v := check(jpegBytes(t), "IMG_0001.HEIC")
	if v.Status != Sound || v.Format != "JPEG" || v.Note == "" {
		t.Errorf("got %+v, want a sound JPEG with a note", v)
	}
	// Noise under a name that promises a signature is damage.
	noise := make([]byte, 4096)
	rand.New(rand.NewSource(2)).Read(noise)
	noise[0] = 0x7f
	if v := check(noise, "a.pdf"); v.Status != Damaged {
		t.Errorf("got %+v, want damaged", v)
	}
}

func TestUnknownIsNotAFinding(t *testing.T) {
	for name, data := range map[string][]byte{
		"notes.txt": []byte("just some text\n"),
		"blob.bin":  {1, 2, 3, 4, 5, 6, 7, 8, 9, 10, 11, 12, 13, 14},
		"empty.jpg": {},
	} {
		if v := check(data, name); v.Status != Unknown {
			t.Errorf("%s: got %+v, want unknown", name, v)
		}
	}
}

type failingReader struct{}

func (failingReader) ReadAt([]byte, int64) (int, error) { return 0, errors.New("cloud went away") }

func TestReadFailureIsNotDamage(t *testing.T) {
	v := Check(failingReader{}, 5000, "a.jpg")
	if v.Status != Unknown || !strings.Contains(v.Note, "cloud went away") {
		t.Errorf("got %+v, want unknown with the read error", v)
	}
}

// Whatever it is handed, Check answers and reads nothing past the size given.
func TestHostileInputNeverPanics(t *testing.T) {
	rng := rand.New(rand.NewSource(3))
	seeds := [][]byte{jpegBytes(t), pngBytes(t), pdfBytes(), zipBytes(t), mkvBytes(300, 300),
		mp4Bytes("isom", box("moov", make([]byte, 40)), box("mdat", make([]byte, 400)))}
	names := []string{"a.jpg", "a.png", "a.pdf", "a.docx", "a.mkv", "a.mp4", "a.heic", "a.bin"}
	for i := 0; i < 4000; i++ {
		data := append([]byte(nil), seeds[rng.Intn(len(seeds))]...)
		for flips := rng.Intn(8); flips > 0; flips-- {
			data[rng.Intn(len(data))] = byte(rng.Intn(256))
		}
		data = data[:rng.Intn(len(data)+1)]
		check(data, names[rng.Intn(len(names))])
	}
}
