package vault

// What a transfer's summary says about its files, and how much of it.
//
// An import or an export is as big as the selection, and the selection has no
// cap: a person who picks a folder of two hundred thousand photos gets two
// hundred thousand photos. What they do not get is two hundred thousand lines
// about it. A summary that listed every file would be tens of megabytes of
// JSON for a page that shows five lines, held in memory for half an hour
// after a detached transfer ends — on the Raspberry Pi this is built to run
// on, that is the budget for the transfer itself.
//
// So a summary is counts, plus the lines worth reading: the files that failed,
// the files left alone for a reason a second run will not clear, the files
// that arrived with a warning attached. A file that went as planned, or was
// already there, is a number. And even the lines are bounded, because a source
// that dies halfway fails every file after it in the same words.

// maxTransferLines bounds how many per-file lines one summary carries. The
// page shows five; a few hundred is enough to find the shape of what went
// wrong, and small enough to hold and to send without thinking about it.
const maxTransferLines = 500

// transferLines is the bounded list behind a summary's Results.
//
// Lines are kept in the order they arrive until the list is full; after that
// they are counted rather than kept. Whether a line is worth adding at all is
// the caller's call — see ImportResult.worthALine and ExportResult.worthALine.
type transferLines[T any] struct {
	lines   []T
	omitted int
}

// add keeps the line, or counts it once there is no room.
func (l *transferLines[T]) add(line T) {
	if len(l.lines) >= maxTransferLines {
		l.omitted++
		return
	}
	l.lines = append(l.lines, line)
}
