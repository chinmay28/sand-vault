package vault

import "testing"

func TestTransferLinesKeepsUpToTheCapAndCountsTheRest(t *testing.T) {
	var l transferLines[int]
	for i := 0; i < maxTransferLines+7; i++ {
		l.add(i)
	}
	if len(l.lines) != maxTransferLines {
		t.Errorf("kept %d lines, want %d", len(l.lines), maxTransferLines)
	}
	if l.omitted != 7 {
		t.Errorf("counted %d omitted lines, want 7", l.omitted)
	}
	// The first lines are the ones kept: what went wrong first is usually
	// what went wrong.
	if l.lines[0] != 0 || l.lines[maxTransferLines-1] != maxTransferLines-1 {
		t.Errorf("kept the wrong lines: first %d, last %d", l.lines[0], l.lines[maxTransferLines-1])
	}
}

func TestTransferLinesStartsEmpty(t *testing.T) {
	var l transferLines[string]
	if len(l.lines) != 0 || l.omitted != 0 {
		t.Errorf("a fresh list holds %d lines and %d omitted", len(l.lines), l.omitted)
	}
	l.add("one")
	if len(l.lines) != 1 || l.omitted != 0 {
		t.Errorf("after one line: %d lines and %d omitted", len(l.lines), l.omitted)
	}
}
