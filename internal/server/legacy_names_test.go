package server

import (
	"archive/zip"
	"bytes"
	"io"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"testing"
)

// multipartRequest builds a form upload of the named files under one field,
// plus the password.
func multipartRequest(t *testing.T, path, field string, files []struct {
	name    string
	content []byte
}, password string) *http.Request {
	t.Helper()
	body := &bytes.Buffer{}
	writer := multipart.NewWriter(body)
	for _, f := range files {
		fw, err := writer.CreateFormFile(field, f.name)
		if err != nil {
			t.Fatal(err)
		}
		fw.Write(f.content)
	}
	writer.WriteField("password", password)
	writer.Close()

	req := httptest.NewRequest("POST", path, body)
	req.Header.Set("Content-Type", writer.FormDataContentType())
	return req
}

// Two uploads with one name used to land on the same temporary path: the
// second overwrote the first, and the zips came back holding the second one
// twice with nothing said. The parts are named after the file, so there is
// no way to archive both in one request — it is refused instead.
func TestArchiveRefusesTwoFilesWithOneName(t *testing.T) {
	req := multipartRequest(t, "/api/archive", "files[]", []struct {
		name    string
		content []byte
	}{
		{"report.txt", []byte("the first report")},
		{"report.txt", []byte("the second report")},
	}, "pw")
	w := httptest.NewRecorder()
	handleArchive(w, req)

	if w.Code != http.StatusBadRequest {
		t.Fatalf("archive of two files called the same = %d, want 400: %s", w.Code, w.Body.String())
	}
	if !bytes.Contains(w.Body.Bytes(), []byte("DUPLICATE_NAME")) {
		t.Errorf("the refusal does not say why: %s", w.Body.String())
	}
}

// partsOf archives one file and returns its three parts.
func partsOf(t *testing.T, name string, content []byte, password string) [][]byte {
	t.Helper()
	req := createArchiveRequest(t, name, content, password)
	w := httptest.NewRecorder()
	handleArchive(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("archive = %d: %s", w.Code, w.Body.String())
	}
	outer, err := zip.NewReader(bytes.NewReader(w.Body.Bytes()), int64(w.Body.Len()))
	if err != nil {
		t.Fatalf("reading the outer zip: %v", err)
	}
	var parts [][]byte
	for _, zf := range outer.File {
		rc, err := zf.Open()
		if err != nil {
			t.Fatal(err)
		}
		innerData, err := io.ReadAll(rc)
		rc.Close()
		if err != nil {
			t.Fatal(err)
		}
		inner, err := zip.NewReader(bytes.NewReader(innerData), int64(len(innerData)))
		if err != nil {
			t.Fatal(err)
		}
		for _, pf := range inner.File {
			prc, err := pf.Open()
			if err != nil {
				t.Fatal(err)
			}
			part, err := io.ReadAll(prc)
			prc.Close()
			if err != nil {
				t.Fatal(err)
			}
			parts = append(parts, part)
		}
	}
	if len(parts) != 3 {
		t.Fatalf("got %d parts, want 3", len(parts))
	}
	return parts
}

// Two part files that happen to share a name are both kept: a part's name
// means nothing to the restore, and the second used to overwrite the first on
// disk, so the restore saw one part twice and failed with everything in hand.
func TestRestoreKeepsPartsThatShareAName(t *testing.T) {
	original := []byte("the file being rebuilt from two parts saved under one name")
	parts := partsOf(t, "notes.txt", original, "pw")

	req := multipartRequest(t, "/api/restore", "parts[]", []struct {
		name    string
		content []byte
	}{
		{"part.sand", parts[0]},
		{"part.sand", parts[1]},
	}, "pw")
	w := httptest.NewRecorder()
	handleRestore(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("restore from two parts sharing a name = %d: %s", w.Code, w.Body.String())
	}
	if !bytes.Equal(w.Body.Bytes(), original) {
		t.Error("the restored file is not the original")
	}
}
