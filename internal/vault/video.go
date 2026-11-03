package vault

import (
	"path"
	"strings"
)

// IsVideo reports whether a file is a video — the kind of thing the film
// database is asked about, and the kind the duplicate finder asks "which film
// is this?" of.
//
// Deliberately generous, and by extension as well as by type: a .mkv usually
// arrives with no recognised MIME type at all — Go's built-in table stops at
// the handful of web formats — and it is the single most likely thing in a
// films folder.
func IsVideo(e *Entry) bool {
	if strings.HasPrefix(strings.ToLower(e.MIME), "video/") {
		return true
	}
	switch strings.ToLower(path.Ext(e.Name)) {
	case ".mkv", ".mp4", ".m4v", ".mov", ".avi", ".wmv", ".flv", ".mpg", ".mpeg",
		".m2ts", ".ts", ".webm", ".ogv", ".divx", ".vob", ".iso", ".3gp":
		return true
	}
	return false
}
