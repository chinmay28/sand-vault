package vault

import (
	"errors"
	"strings"
	"testing"
)

func TestNewKitCodeShape(t *testing.T) {
	code, err := NewKitCode()
	if err != nil {
		t.Fatalf("NewKitCode: %v", err)
	}
	if want := "XXXXX-XXXXX-XXXXX-XXXXX-XXXXX"; len(code) != len(want) {
		t.Fatalf("code %q is %d characters, want %d", code, len(code), len(want))
	}
	for i, c := range code {
		if (i+1)%6 == 0 {
			if c != '-' {
				t.Fatalf("code %q: position %d is %q, want a hyphen", code, i, string(c))
			}
			continue
		}
		if !strings.ContainsRune(kitCodeAlphabet, c) {
			t.Fatalf("code %q carries %q, which is not in the alphabet", code, string(c))
		}
	}
	if _, err := NormalizeKitCode(code); err != nil {
		t.Fatalf("a freshly minted code does not normalize: %v", err)
	}
}

// The four confusable letters must not be in the alphabet at all: the whole
// point of leaving them out is that a reader never has to decide.
func TestKitCodeAlphabetExcludesConfusables(t *testing.T) {
	if len(kitCodeAlphabet) != 32 {
		t.Fatalf("alphabet is %d symbols, want 32", len(kitCodeAlphabet))
	}
	for _, c := range "ILOU" {
		if strings.ContainsRune(kitCodeAlphabet, c) {
			t.Errorf("alphabet contains %q", string(c))
		}
	}
	seen := map[rune]bool{}
	for _, c := range kitCodeAlphabet {
		if seen[c] {
			t.Errorf("alphabet repeats %q", string(c))
		}
		seen[c] = true
	}
}

func TestKitCodesAreDistinct(t *testing.T) {
	seen := map[string]bool{}
	for i := 0; i < 2000; i++ {
		code, err := NewKitCode()
		if err != nil {
			t.Fatalf("NewKitCode: %v", err)
		}
		if seen[code] {
			t.Fatalf("%q was generated twice", code)
		}
		seen[code] = true
	}
}

// Everything a person does to a code between writing it down and typing it
// back has to survive: case, grouping, and the letters they were never given.
func TestNormalizeKitCodeForgivesTranscription(t *testing.T) {
	code, err := NewKitCode()
	if err != nil {
		t.Fatalf("NewKitCode: %v", err)
	}
	want, err := NormalizeKitCode(code)
	if err != nil {
		t.Fatalf("NormalizeKitCode: %v", err)
	}

	bare := strings.ReplaceAll(code, "-", "")
	variants := map[string]string{
		"lower case":      strings.ToLower(code),
		"no hyphens":      bare,
		"spaces":          strings.ReplaceAll(code, "-", " "),
		"leading space":   "  " + code + "  ",
		"mixed case":      strings.ToLower(code[:8]) + code[8:],
		"broken grouping": bare[:3] + "-" + bare[3:11] + " " + bare[11:],
	}
	for name, variant := range variants {
		got, err := NormalizeKitCode(variant)
		if err != nil {
			t.Errorf("%s (%q): %v", name, variant, err)
			continue
		}
		if got != want {
			t.Errorf("%s (%q) normalized to %q, want %q", name, variant, got, want)
		}
	}
}

// A code written by hand comes back with I for 1 and O for 0. Folding them is
// what makes the missing letters a kindness rather than a trap.
func TestNormalizeKitCodeFoldsConfusables(t *testing.T) {
	for _, tc := range []struct{ typed, means string }{
		{"I", "1"}, {"i", "1"}, {"L", "1"}, {"l", "1"}, {"O", "0"}, {"o", "0"},
	} {
		// Build a code whose body is all the folded character, then check the
		// typed form normalizes to the same 25 symbols.
		body := strings.Repeat(tc.means, kitCodeSymbols-1)
		raw, err := kitCodeEnc.DecodeString(body)
		if err != nil {
			t.Fatalf("decoding %q: %v", body, err)
		}
		valid := body + string(kitCodeCheck(raw))
		typed := strings.Repeat(tc.typed, kitCodeSymbols-1) + valid[kitCodeSymbols-1:]

		got, err := NormalizeKitCode(typed)
		if err != nil {
			t.Errorf("%q (for %q): %v", typed, tc.means, err)
			continue
		}
		if got != valid {
			t.Errorf("%q normalized to %q, want %q", typed, got, valid)
		}
	}
}

// The check symbol's whole job: catch the slip locally, so nobody pays a second
// and a half of Argon2id to be told "wrong code" about their own typing.
func TestKitCodeCatchesEverySingleCharacterTypo(t *testing.T) {
	code, err := NewKitCode()
	if err != nil {
		t.Fatalf("NewKitCode: %v", err)
	}
	bare := strings.ReplaceAll(code, "-", "")

	missed := 0
	total := 0
	for i := 0; i < len(bare); i++ {
		for _, replacement := range kitCodeAlphabet {
			if byte(replacement) == bare[i] {
				continue
			}
			total++
			typo := bare[:i] + string(replacement) + bare[i+1:]
			if _, err := NormalizeKitCode(typo); err == nil {
				missed++
			}
		}
	}
	// A five-bit check symbol lets through one in 32 by construction; what it
	// must never do is let through a lot more than that.
	if limit := total / 20; missed > limit {
		t.Fatalf("%d of %d single-character typos went undetected, want at most %d",
			missed, total, limit)
	}
}

func TestKitCodeCatchesTranspositions(t *testing.T) {
	code, err := NewKitCode()
	if err != nil {
		t.Fatalf("NewKitCode: %v", err)
	}
	bare := strings.ReplaceAll(code, "-", "")

	swaps, missed := 0, 0
	for i := 0; i+1 < len(bare); i++ {
		if bare[i] == bare[i+1] {
			continue
		}
		swaps++
		swapped := bare[:i] + string(bare[i+1]) + string(bare[i]) + bare[i+2:]
		if _, err := NormalizeKitCode(swapped); err == nil {
			missed++
		}
	}
	if swaps == 0 {
		t.Skip("this code has no adjacent distinct pair to swap")
	}
	if missed > swaps/4 {
		t.Fatalf("%d of %d transpositions went undetected", missed, swaps)
	}
}

func TestNormalizeKitCodeRejections(t *testing.T) {
	code, err := NewKitCode()
	if err != nil {
		t.Fatalf("NewKitCode: %v", err)
	}
	bare := strings.ReplaceAll(code, "-", "")

	t.Run("too short", func(t *testing.T) {
		if _, err := NormalizeKitCode(bare[:20]); !errors.Is(err, ErrKitCodeLength) {
			t.Fatalf("got %v, want ErrKitCodeLength", err)
		}
	})
	t.Run("too long", func(t *testing.T) {
		if _, err := NormalizeKitCode(bare + "7"); !errors.Is(err, ErrKitCodeLength) {
			t.Fatalf("got %v, want ErrKitCodeLength", err)
		}
	})
	t.Run("empty", func(t *testing.T) {
		if _, err := NormalizeKitCode(""); !errors.Is(err, ErrKitCodeLength) {
			t.Fatalf("got %v, want ErrKitCodeLength", err)
		}
	})
	t.Run("a character the alphabet does not use", func(t *testing.T) {
		_, err := NormalizeKitCode(bare[:10] + "%" + bare[11:])
		if err == nil {
			t.Fatal("a stray character was accepted")
		}
		// Localisable, unlike a bad check symbol, so it names the character.
		if !strings.Contains(err.Error(), "%") {
			t.Fatalf("error does not name the offending character: %v", err)
		}
	})
}

func TestFormatKitCodeRoundTrip(t *testing.T) {
	code, err := NewKitCode()
	if err != nil {
		t.Fatalf("NewKitCode: %v", err)
	}
	bare, err := NormalizeKitCode(code)
	if err != nil {
		t.Fatalf("NormalizeKitCode: %v", err)
	}
	if got := FormatKitCode(bare); got != code {
		t.Fatalf("FormatKitCode(%q) = %q, want %q", bare, got, code)
	}
}

func TestLooksLikeKitCode(t *testing.T) {
	code, err := NewKitCode()
	if err != nil {
		t.Fatalf("NewKitCode: %v", err)
	}
	if !LooksLikeKitCode(code) {
		t.Error("a real code does not look like one")
	}
	if LooksLikeKitCode("correct horse battery staple") {
		t.Error("a password looks like a code")
	}
}
