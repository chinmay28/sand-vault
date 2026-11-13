package server

import (
	"path"
	"sort"
	"strings"

	"github.com/chinmay28/SandHarbor/internal/vault"
)

// Sidecar subtitles: the .srt beside the film.
//
// A films folder is full of them — Film.2010.mkv next to Film.2010.srt, or
// Film.2010.en.srt and Film.2010.fr.srt — and VLC, Plex and Jellyfin all load
// them without being asked. A TV can only reach videos, so the subtitles are
// found here, on the server, from the video the TV asked to play: a file in the
// same folder, with a subtitle extension, whose name starts with the video's
// name. Nothing else in the folder is reachable this way.

// subtitleFormats maps the extensions a player can read to their MIME types,
// in the vocabulary Media3 uses.
var subtitleFormats = map[string]string{
	".srt": "application/x-subrip",
	".vtt": "text/vtt",
	".ass": "text/x-ssa",
	".ssa": "text/x-ssa",
}

// sidecar is one subtitle file found beside a video.
type sidecar struct {
	Entry    *vault.Entry
	MIME     string
	Language string // "en", "fra" — empty when the name does not say
	Label    string // what the player lists it as
}

// sidecarSubtitles finds the subtitle files for a video among the files of its
// folder.
//
// "Film.2010.mkv" claims "Film.2010.srt", "Film.2010.en.srt" and
// "Film.2010.English.forced.srt", case aside, and nothing else: the subtitle's
// name has to be the video's, then a dot or nothing, then the extension.
func sidecarSubtitles(video *vault.Entry, siblings []*vault.Entry) []sidecar {
	base := strings.ToLower(strings.TrimSuffix(video.Name, path.Ext(video.Name)))
	if base == "" {
		return nil
	}

	var out []sidecar
	for _, e := range siblings {
		if e.ID == video.ID || e.Dir != video.Dir {
			continue
		}
		ext := strings.ToLower(path.Ext(e.Name))
		mime, ok := subtitleFormats[ext]
		if !ok {
			continue
		}
		stem := strings.ToLower(strings.TrimSuffix(e.Name, path.Ext(e.Name)))
		var tag string
		switch {
		case stem == base:
		case strings.HasPrefix(stem, base+"."):
			tag = e.Name[len(base)+1 : len(stem)]
		default:
			continue
		}
		out = append(out, sidecar{Entry: e, MIME: mime, Language: languageOf(tag), Label: subtitleLabel(tag, ext)})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Entry.Name < out[j].Entry.Name })
	return out
}

// languageOf reads a language code off the tag between the film's name and the
// extension: the first dot-separated part, when it is two or three letters
// ("en", "fra"). Anything longer is a word — "English", "forced" — and is left
// to the label.
func languageOf(tag string) string {
	first := strings.SplitN(tag, ".", 2)[0]
	if n := len(first); n < 2 || n > 3 {
		return ""
	}
	for _, r := range first {
		if (r < 'a' || r > 'z') && (r < 'A' || r > 'Z') {
			return ""
		}
	}
	return strings.ToLower(first)
}

// subtitleLabel is what the player's subtitle menu lists a sidecar as: its tag
// as written, or the format when the file has none.
func subtitleLabel(tag, ext string) string {
	format := strings.ToUpper(strings.TrimPrefix(ext, "."))
	if tag == "" {
		return "External (" + format + ")"
	}
	return strings.ReplaceAll(tag, ".", " ") + " (" + format + ")"
}
