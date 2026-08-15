package thumb

import (
	"bytes"
	"image"
	"image/color"
	"image/jpeg"
	"image/png"
	"strings"
	"testing"
)

// sourcePNG paints a w×h image with a distinct block of colour in each corner,
// so a scaled copy can be checked for having kept the picture rather than just
// the dimensions.
func sourcePNG(t *testing.T, w, h int) []byte {
	t.Helper()

	img := image.NewRGBA(image.Rect(0, 0, w, h))
	for y := 0; y < h; y++ {
		for x := 0; x < w; x++ {
			c := color.RGBA{A: 255}
			if x < w/2 {
				c.R = 255
			} else {
				c.B = 255
			}
			if y >= h/2 {
				c.G = 200
			}
			img.Set(x, y, c)
		}
	}

	var buf bytes.Buffer
	if err := png.Encode(&buf, img); err != nil {
		t.Fatalf("encoding source: %v", err)
	}
	return buf.Bytes()
}

func TestNormalizeShrinksToSize(t *testing.T) {
	out, err := Normalize(sourcePNG(t, 1200, 600))
	if err != nil {
		t.Fatalf("Normalize: %v", err)
	}

	cfg, format, err := image.DecodeConfig(bytes.NewReader(out))
	if err != nil {
		t.Fatalf("decoding the result: %v", err)
	}
	if format != "jpeg" {
		t.Errorf("format = %q, want jpeg — whatever arrives is re-encoded", format)
	}
	if cfg.Width != Size {
		t.Errorf("width = %d, want %d", cfg.Width, Size)
	}
	if cfg.Height != Size/2 {
		t.Errorf("height = %d, want %d — the aspect ratio has to survive", cfg.Height, Size/2)
	}
}

func TestNormalizeKeepsThePicture(t *testing.T) {
	out, err := Normalize(sourcePNG(t, 800, 800))
	if err != nil {
		t.Fatalf("Normalize: %v", err)
	}
	img, err := jpeg.Decode(bytes.NewReader(out))
	if err != nil {
		t.Fatalf("decoding the result: %v", err)
	}

	// The left half of the source is red and the right half blue; a thumbnail
	// that came out grey or mirrored would pass a dimensions-only check.
	left, _, _, _ := img.At(20, 20).RGBA()
	_, _, right, _ := img.At(Size-20, 20).RGBA()
	if left>>8 < 200 {
		t.Errorf("red corner came out at %d, want a strong red", left>>8)
	}
	if right>>8 < 200 {
		t.Errorf("blue corner came out at %d, want a strong blue", right>>8)
	}
}

func TestNormalizeLeavesSmallImagesAlone(t *testing.T) {
	out, err := Normalize(sourcePNG(t, 64, 48))
	if err != nil {
		t.Fatalf("Normalize: %v", err)
	}
	cfg, _, err := image.DecodeConfig(bytes.NewReader(out))
	if err != nil {
		t.Fatalf("decoding the result: %v", err)
	}
	if cfg.Width != 64 || cfg.Height != 48 {
		t.Errorf("got %dx%d, want 64x48 — an image already under the ceiling is not scaled up",
			cfg.Width, cfg.Height)
	}
}

func TestNormalizeRejectsWhatIsNotAnImage(t *testing.T) {
	for name, input := range map[string][]byte{
		"empty":     {},
		"text":      []byte("this is not a picture of anything"),
		"truncated": sourcePNG(t, 100, 100)[:40],
	} {
		t.Run(name, func(t *testing.T) {
			if _, err := Normalize(input); err == nil {
				t.Fatal("expected an error, got none")
			}
		})
	}
}

func TestNormalizeRejectsOversizedPayloads(t *testing.T) {
	_, err := Normalize(make([]byte, MaxInputBytes+1))
	if err == nil {
		t.Fatal("expected an error, got none")
	}
	if !strings.Contains(err.Error(), "larger than") {
		t.Errorf("error = %q, want it to name the limit", err)
	}
}
