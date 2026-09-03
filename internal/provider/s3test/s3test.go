// Package s3test is an in-memory S3 endpoint for tests, with versioning.
//
// It exists because the thing the S3 backend's version handling is for cannot
// be seen through a plain listing: a versioned bucket keeps every Put and turns
// every Delete into a marker, and only a listing of versions shows what is
// actually being stored. A stub that only knows the calls a plain listing makes
// cannot check any of that. This one behaves the way Backblaze B2's default
// ("keep all versions") bucket does, and can be switched to the way an
// unversioned bucket does, so a test can check both.
//
// It speaks exactly the subset the backend uses — PUT, GET, HEAD and DELETE of
// an object, ListObjectsV2, ListObjectVersions and DELETE by versionId, all
// path-style — and nothing else. It checks no signatures: the SigV4 test in the
// provider package covers those.
package s3test

import (
	"encoding/xml"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
)

// Version is one stored version of one key, as the stub holds it.
type Version struct {
	Key          string
	VersionID    string
	Data         []byte
	DeleteMarker bool
	Modified     time.Time
}

// Server is a versioned S3 endpoint on a loopback port. Buckets come into
// being when first written to. Close it when the test is done.
type Server struct {
	*httptest.Server

	mu      sync.Mutex
	buckets map[string][]Version
	clock   int64

	// Versioned is whether a Put keeps the version it replaces and a Delete
	// leaves a marker rather than removing. On by default, because a bucket
	// that keeps every version is the one worth testing against.
	Versioned bool

	// PageSize is how many entries a listing answers with before truncating.
	// Small values exercise the pagination in the backend.
	PageSize int
}

// New starts a versioned server. The caller must Close it.
func New() *Server {
	s := &Server{buckets: map[string][]Version{}, Versioned: true, PageSize: 1000}
	s.Server = httptest.NewServer(http.HandlerFunc(s.serve))
	return s
}

// Options returns the settings that connect the S3 backend to one bucket on
// this server: what a test hands to provider.New or to Vault.AddProvider.
func (s *Server) Options(bucket string) map[string]string {
	return map[string]string{
		"bucket":            bucket,
		"region":            "us-east-1",
		"access_key_id":     "AKIAIOSFODNN7EXAMPLE",
		"secret_access_key": "wJalrXUtnFEMI/K7MDENG/bPxRfiCYEXAMPLEKEY",
		"endpoint":          s.URL,
	}
}

// Versions returns every stored version in a bucket, delete markers included,
// sorted by key and then newest first — the order a listing of versions comes
// back in.
func (s *Server) Versions(bucket string) []Version {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := append([]Version(nil), s.buckets[bucket]...)
	sortVersions(out)
	return out
}

// Objects returns the keys a plain listing would show: those whose latest
// version is not a delete marker.
func (s *Server) Objects(bucket string) []string {
	var keys []string
	for _, v := range s.latest(s.Versions(bucket)) {
		if !v.DeleteMarker {
			keys = append(keys, v.Key)
		}
	}
	return keys
}

// latest picks the newest version of each key out of a sorted list.
func (s *Server) latest(sorted []Version) []Version {
	var out []Version
	for i, v := range sorted {
		if i == 0 || sorted[i-1].Key != v.Key {
			out = append(out, v)
		}
	}
	return out
}

// sortVersions orders by key, and newest first within a key.
func sortVersions(versions []Version) {
	sort.SliceStable(versions, func(i, j int) bool {
		if versions[i].Key != versions[j].Key {
			return versions[i].Key < versions[j].Key
		}
		return versions[i].Modified.After(versions[j].Modified)
	})
}

func (s *Server) serve(w http.ResponseWriter, r *http.Request) {
	bucket, key, _ := strings.Cut(strings.TrimPrefix(r.URL.Path, "/"), "/")
	if bucket == "" {
		http.Error(w, "no bucket", http.StatusBadRequest)
		return
	}
	q := r.URL.Query()

	switch {
	case key == "" && r.Method == http.MethodGet && q.Has("versions"):
		s.listVersions(w, bucket, q)
	case key == "" && r.Method == http.MethodGet:
		s.listObjects(w, bucket, q)
	case key == "":
		http.Error(w, "unsupported bucket operation", http.StatusNotImplemented)
	case r.Method == http.MethodPut:
		body, _ := io.ReadAll(r.Body)
		s.put(bucket, key, body)
		w.WriteHeader(http.StatusOK)
	case r.Method == http.MethodGet, r.Method == http.MethodHead:
		data, ok := s.get(bucket, key)
		if !ok {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		w.Header().Set("Content-Length", strconv.Itoa(len(data)))
		if r.Method == http.MethodGet {
			w.Write(data)
		}
	case r.Method == http.MethodDelete && q.Has("versionId"):
		s.deleteVersion(bucket, key, q.Get("versionId"))
		w.WriteHeader(http.StatusNoContent)
	case r.Method == http.MethodDelete:
		s.delete(bucket, key)
		w.WriteHeader(http.StatusNoContent)
	default:
		http.Error(w, "unsupported", http.StatusNotImplemented)
	}
}

// tick hands out strictly increasing timestamps, so the order versions were
// written in survives a listing however fast the test wrote them.
func (s *Server) tick() time.Time {
	s.clock++
	return time.Unix(1_700_000_000, 0).Add(time.Duration(s.clock) * time.Second).UTC()
}

func (s *Server) put(bucket, key string, data []byte) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if !s.Versioned {
		s.buckets[bucket] = without(s.buckets[bucket], key)
	}
	s.buckets[bucket] = append(s.buckets[bucket], Version{
		Key:       key,
		VersionID: s.newVersionID(),
		Data:      append([]byte(nil), data...),
		Modified:  s.tick(),
	})
}

func (s *Server) newVersionID() string {
	return fmt.Sprintf("v%06d", s.clock+1)
}

func (s *Server) get(bucket, key string) ([]byte, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	var newest *Version
	for i := range s.buckets[bucket] {
		v := &s.buckets[bucket][i]
		if v.Key == key && (newest == nil || v.Modified.After(newest.Modified)) {
			newest = v
		}
	}
	if newest == nil || newest.DeleteMarker {
		return nil, false
	}
	return newest.Data, true
}

func (s *Server) delete(bucket, key string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if !s.Versioned {
		s.buckets[bucket] = without(s.buckets[bucket], key)
		return
	}
	s.buckets[bucket] = append(s.buckets[bucket], Version{
		Key:          key,
		VersionID:    s.newVersionID(),
		DeleteMarker: true,
		Modified:     s.tick(),
	})
}

func (s *Server) deleteVersion(bucket, key, versionID string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	kept := s.buckets[bucket][:0]
	for _, v := range s.buckets[bucket] {
		if v.Key != key || v.VersionID != versionID {
			kept = append(kept, v)
		}
	}
	s.buckets[bucket] = kept
}

// without drops every version of a key, which is what an unversioned bucket
// does on overwrite and on delete.
func without(versions []Version, key string) []Version {
	kept := versions[:0]
	for _, v := range versions {
		if v.Key != key {
			kept = append(kept, v)
		}
	}
	return kept
}

// ListObjectsV2, latest live versions only, paged by continuation token.
func (s *Server) listObjects(w http.ResponseWriter, bucket string, q map[string][]string) {
	prefix := first(q, "prefix")
	after := first(q, "continuation-token")
	pageSize := s.pageSize(first(q, "max-keys"))

	type entry struct {
		Key  string `xml:"Key"`
		Size int64  `xml:"Size"`
	}
	var result struct {
		XMLName               xml.Name `xml:"ListBucketResult"`
		IsTruncated           bool     `xml:"IsTruncated"`
		NextContinuationToken string   `xml:"NextContinuationToken,omitempty"`
		Contents              []entry  `xml:"Contents"`
	}

	for _, v := range s.latest(s.Versions(bucket)) {
		if v.DeleteMarker || !strings.HasPrefix(v.Key, prefix) || v.Key <= after {
			continue
		}
		if len(result.Contents) == pageSize {
			result.IsTruncated = true
			result.NextContinuationToken = result.Contents[pageSize-1].Key
			break
		}
		result.Contents = append(result.Contents, entry{Key: v.Key, Size: int64(len(v.Data))})
	}
	writeXML(w, result)
}

// ListObjectVersions, every version and marker, paged by key and version
// markers.
func (s *Server) listVersions(w http.ResponseWriter, bucket string, q map[string][]string) {
	prefix := first(q, "prefix")
	keyMarker, versionMarker := first(q, "key-marker"), first(q, "version-id-marker")
	pageSize := s.pageSize(first(q, "max-keys"))

	type entry struct {
		Key          string `xml:"Key"`
		VersionID    string `xml:"VersionId"`
		IsLatest     bool   `xml:"IsLatest"`
		Size         int64  `xml:"Size"`
		LastModified string `xml:"LastModified"`
	}
	var result struct {
		XMLName             xml.Name `xml:"ListVersionsResult"`
		IsTruncated         bool     `xml:"IsTruncated"`
		NextKeyMarker       string   `xml:"NextKeyMarker,omitempty"`
		NextVersionIdMarker string   `xml:"NextVersionIdMarker,omitempty"`
		Versions            []entry  `xml:"Version"`
		DeleteMarkers       []entry  `xml:"DeleteMarker"`
	}

	// The page starts after the marker pair, in the listing's own order.
	started := keyMarker == ""
	listed := 0
	var lastKey, lastVersion string
	all := s.Versions(bucket)
	for i, v := range all {
		if !started {
			if v.Key == keyMarker && v.VersionID == versionMarker {
				started = true
			}
			continue
		}
		if !strings.HasPrefix(v.Key, prefix) {
			continue
		}
		if listed == pageSize {
			result.IsTruncated = true
			result.NextKeyMarker, result.NextVersionIdMarker = lastKey, lastVersion
			break
		}
		e := entry{
			Key:          v.Key,
			VersionID:    v.VersionID,
			IsLatest:     i == 0 || all[i-1].Key != v.Key,
			Size:         int64(len(v.Data)),
			LastModified: v.Modified.Format(time.RFC3339Nano),
		}
		if v.DeleteMarker {
			result.DeleteMarkers = append(result.DeleteMarkers, e)
		} else {
			result.Versions = append(result.Versions, e)
		}
		listed++
		lastKey, lastVersion = v.Key, v.VersionID
	}
	writeXML(w, result)
}

func (s *Server) pageSize(maxKeys string) int {
	if n, err := strconv.Atoi(maxKeys); err == nil && n > 0 && n < s.PageSize {
		return n
	}
	return s.PageSize
}

func first(q map[string][]string, key string) string {
	if v := q[key]; len(v) > 0 {
		return v[0]
	}
	return ""
}

func writeXML(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/xml")
	w.Write([]byte(xml.Header))
	xml.NewEncoder(w).Encode(v)
}
