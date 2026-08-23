package vault

import "io"

// Where an import has got to, while it is still getting there.
//
// This is a window onto a request that is running, and nothing more. It is not
// job state: it lives in memory for as long as the import does, it is never
// written down, and losing it costs nothing, because the import it describes
// went with it. That is the same bargain ImportFromSource makes about resuming
// — there is no job, only a request — and adding a progress bar was not allowed
// to quietly turn one into the other.
//
// What it is for is the case the dialog used to have nothing to say about: one
// very large file. A folder of small files reports itself, a file at a time, in
// the summary at the end; an 18 GB film reports nothing for an hour and looks
// identical to a hang.

// ImportStage is which half of the journey a file is on.
//
// Both halves are worth naming because both are long and they are long for
// different reasons: fetching is the source's upstream, scattering is this
// machine's, and one being slow says something quite different from the other.
type ImportStage string

const (
	// StageFetching is the source coming down to this machine's spool.
	StageFetching ImportStage = "fetching"

	// StageScattering is the spool going back up to the connected accounts,
	// compressed, split and encrypted on the way. See UploadStream.
	StageScattering ImportStage = "scattering"
)

// ImportProgress is one file of an import, mid-flight.
//
// The counts are what the summary would say if the import stopped here, which
// is what makes it readable while it runs: "4 of 12, one already here".
type ImportProgress struct {
	// File is the file being worked on, 1-based, out of Files. Files is what
	// the walk planned, so it is the whole selection rather than what is left.
	File  int `json:"file"`
	Files int `json:"files"`

	// Path is the file on the source, relative to its root; Dest is where it
	// is landing in the vault.
	Path string `json:"path"`
	Dest string `json:"dest"`
	Name string `json:"name"`

	Stage ImportStage `json:"stage"`

	// Done is how much of this file has moved in this stage, out of Size.
	// It restarts at zero when the stage changes, because the two stages are
	// two passes over the same bytes rather than halves of one.
	Done int64 `json:"done"`
	Size int64 `json:"size"`

	// What has become of the files before this one.
	Imported int `json:"imported"`
	Skipped  int `json:"skipped"`
	Failed   int `json:"failed"`
}

// progressEvery is how much has to move before the reader says so again.
//
// A read off an SFTP handle comes back in tens of kilobytes, and an 18 GB file
// is half a million of them — a report per read would be half a million
// callbacks to draw a bar that moves in whole percent. Four megabytes is under
// a second on any connection worth watching a bar for.
const progressEvery = 4 << 20

// progressReader counts what passes through it and says so, not too often.
type progressReader struct {
	r     io.Reader
	say   func(int64)
	done  int64
	since int64
}

func (p *progressReader) Read(b []byte) (int, error) {
	n, err := p.r.Read(b)
	if n > 0 {
		p.done += int64(n)
		p.since += int64(n)
		if p.since >= progressEvery {
			p.since = 0
			p.say(p.done)
		}
	}
	// The end of the file is worth saying whatever the count is at: a bar that
	// stops at 99% because the last read was short is a bar that looks stuck.
	if err != nil && p.since > 0 {
		p.since = 0
		p.say(p.done)
	}
	return n, err
}
