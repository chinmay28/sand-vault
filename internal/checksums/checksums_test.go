package checksums

import (
	"bytes"
	"compress/gzip"
	"io"
	"reflect"
	"strings"
	"testing"
)

const (
	sumA = "9f86d081884c7d659a2feaa0c55ad015a3bf4f1b2b0b822cd15d6c15b0f00a08"
	sumB = "e3b0c44298fc1c149afbf4c8996fb92427ae41e4649b934ca495991b7852b855"
)

func TestRoundTripKeepsAwkwardNames(t *testing.T) {
	in := []Entry{
		{"/z last.txt", sumA},
		{"/a, with, commas.txt", sumB},
		{"/line\nbreak and \\ slash\r.txt", sumA},
		{"/ends in a checksum, " + sumB, sumA},
		{"/ünïcödé/файл.jpg", sumB},
	}
	data, err := Encode(in)
	if err != nil {
		t.Fatal(err)
	}
	out, err := Decode(data)
	if err != nil {
		t.Fatal(err)
	}
	if len(out) != len(in) {
		t.Fatalf("got %d entries, want %d", len(out), len(in))
	}
	want := map[string]string{}
	for _, e := range in {
		want[e.Path] = e.Sum
	}
	for i, e := range out {
		if want[e.Path] != e.Sum {
			t.Errorf("entry %q came back as %q", e.Path, e.Sum)
		}
		if i > 0 && out[i-1].Path > e.Path {
			t.Errorf("not sorted: %q before %q", out[i-1].Path, e.Path)
		}
	}
}

func TestTheTextIsWhatItSaysItIs(t *testing.T) {
	data, _ := Encode([]Entry{{"/b.txt", sumB}, {"/a.txt", sumA}})
	zr, err := gzip.NewReader(bytes.NewReader(data))
	if err != nil {
		t.Fatal(err)
	}
	text, _ := io.ReadAll(zr)
	if want := "/a.txt, " + sumA + "\n/b.txt, " + sumB + "\n"; string(text) != want {
		t.Errorf("ledger text = %q, want %q", text, want)
	}
}

func TestEncodingIsDeterministicAndOrderBlind(t *testing.T) {
	a, _ := Encode([]Entry{{"/a", sumA}, {"/b", sumB}})
	b, _ := Encode([]Entry{{"/b", sumB}, {"/a", sumA}})
	if !bytes.Equal(a, b) {
		t.Error("the same entries in another order encoded differently")
	}
	in := []Entry{{"/b", sumB}, {"/a", sumA}}
	Encode(in)
	if !reflect.DeepEqual(in, []Entry{{"/b", sumB}, {"/a", sumA}}) {
		t.Error("Encode reordered its input")
	}
}

func TestEncodeRefusesWhatWouldNotReadBack(t *testing.T) {
	for _, e := range []Entry{{"", sumA}, {"/a", "abc"}, {"/a", strings.ToUpper(sumA)}, {"/a", strings.Repeat("z", 64)}} {
		if _, err := Encode([]Entry{e}); err == nil {
			t.Errorf("Encode accepted %+v", e)
		}
	}
}

func TestDecodeRefusesDamage(t *testing.T) {
	gz := func(text string) []byte {
		var buf bytes.Buffer
		w := gzip.NewWriter(&buf)
		w.Write([]byte(text))
		w.Close()
		return buf.Bytes()
	}
	for name, data := range map[string][]byte{
		"not gzip":      []byte("/a, " + sumA + "\n"),
		"no checksum":   gz("/a\n"),
		"short sum":     gz("/a, abc\n"),
		"no path":       gz(", " + sumA + "\n"),
		"bad line of 2": gz("/a, " + sumA + "\ngarbage\n"),
	} {
		if _, err := Decode(data); err == nil {
			t.Errorf("%s: decoded without error", name)
		}
	}
	good, _ := Encode([]Entry{{"/a", sumA}})
	if _, err := Decode(good[:len(good)-6]); err == nil {
		t.Error("a truncated ledger decoded without error")
	}
	if out, err := Decode(gz("")); err != nil || len(out) != 0 {
		t.Errorf("an empty ledger: %v, %v", out, err)
	}
}
