package vault

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"strings"
)

// UploadStage is where an upload was when it failed. An upload goes through
// four places that can each go wrong for a different reason, and the repair is
// different at each: nothing about "upload failed" says which of them to look
// at, so the error carries it.
type UploadStage string

const (
	// UploadPlacing is choosing the accounts and the code the file is cut
	// with. Nothing had been sent anywhere.
	UploadPlacing UploadStage = "placing"

	// UploadReading is reading the file on the machine running SAND — the
	// spool, or the temporary file the browser's upload was parsed into.
	// Nothing had been sent anywhere.
	UploadReading UploadStage = "reading"

	// UploadScattering is sending the parts to the clouds. Whatever landed was
	// erased again, so nothing of the file is on any account.
	UploadScattering UploadStage = "scattering"

	// UploadRecording is writing the file into the index once every part was
	// on the clouds. The parts were removed again when the index could not
	// be written.
	UploadRecording UploadStage = "recording"
)

// ShardFailure is one part that did not land, and which account turned it
// away. Reason is the backend's own account of it, which for every HTTP
// backend starts with the status line the cloud answered with.
type ShardFailure struct {
	Part      int    `json:"part"`
	AccountID string `json:"account_id"`
	Account   string `json:"account"`
	Kind      string `json:"kind"`
	Reason    string `json:"reason"`
}

func (f ShardFailure) String() string {
	return fmt.Sprintf("shard %d → %s: %s", f.Part, f.Account, f.Reason)
}

// UploadError is why an upload did not store a file, with where it was when
// it stopped, which accounts refused it there, and what to do next. Every
// error out of Upload, UploadStream and UploadStreamAt is one of these;
// errors.As finds it through any wrapping a caller adds.
//
// Error() reads the same as the plain error always did, so a log or a CLI
// loses nothing; the fields are for a caller that can say more than one line.
type UploadError struct {
	Stage UploadStage

	// Err is what went wrong, at every stage but a scatter that fell short
	// of the shards it needs, where the Failures are the whole story.
	Err error

	// Stored, Needed and Total describe a scatter that fell short: how many
	// distinct shards landed, how many the code needs to read the file back,
	// and how many it writes. Zero at every other stage.
	Stored, Needed, Total int

	// Failures are the shards that did not land, in shard order, one per
	// account that refused. Set at UploadScattering only.
	Failures []ShardFailure
}

func (e *UploadError) Error() string {
	if e.Err != nil {
		return e.Err.Error()
	}
	reasons := make([]string, 0, len(e.Failures))
	for _, f := range e.Failures {
		reasons = append(reasons, f.String())
	}
	return fmt.Sprintf("stored only %d of %d shards, need at least %d: %s",
		e.Stored, e.Total, e.Needed, strings.Join(reasons, "; "))
}

func (e *UploadError) Unwrap() error { return e.Err }

// Accounts names the accounts that refused a part, each once, in shard order.
func (e *UploadError) Accounts() []string {
	seen := map[string]bool{}
	var names []string
	for _, f := range e.Failures {
		if seen[f.accountKey()] {
			continue
		}
		seen[f.accountKey()] = true
		names = append(names, f.Account)
	}
	return names
}

// accountKey is what tells one refusing account from another: its ID, or
// its name for a failure built without one.
func (f ShardFailure) accountKey() string {
	if f.AccountID != "" {
		return f.AccountID
	}
	return f.Account
}

// Advice is what to do next, in a sentence or two: where to look, and whether
// the upload is worth simply trying again. It is written for the person who
// dropped the file, not for a log, and says first what became of the file.
func (e *UploadError) Advice() string {
	if errors.Is(e.Err, ErrLocked) {
		return "The vault locked while the file was on its way, and nothing of it was kept. Unlock it and upload the file again."
	}
	switch e.Stage {
	case UploadPlacing:
		return e.placingAdvice()
	case UploadReading:
		return "Nothing was sent: the file could not be read on the machine running SAND. " +
			"Try the upload again, and if it keeps failing check the disk the vault runs from."
	case UploadScattering:
		return e.scatteringAdvice()
	case UploadRecording:
		return "Every part was on the clouds, but the index could not be written, so the parts were removed again. " +
			"Check the disk the vault file is on, then upload the file again."
	}
	return ""
}

func (e *UploadError) placingAdvice() string {
	msg := e.Error()
	switch {
	case strings.Contains(msg, "connect at least one cloud"):
		return "Nothing was sent: connect a cloud account under Clouds, then upload again."
	case errors.Is(e.Err, ErrNoAccount), strings.Contains(msg, "no connected account with id"):
		return "Nothing was sent: the clouds chosen for this file are not all connected any more. " +
			"Choose again from the ones that are, or leave the choice to the vault."
	case strings.Contains(msg, "health check"):
		return "Nothing was sent: no connected cloud answered the last health check. " +
			"Check them under Cloud health and try again once one is reachable."
	}
	return "Nothing was sent. Check the clouds and the scheme chosen for this upload, then try again."
}

func (e *UploadError) scatteringAdvice() string {
	if len(e.Failures) == 0 {
		if errors.Is(e.Err, context.Canceled) || errors.Is(e.Err, context.DeadlineExceeded) {
			return "The upload was interrupted before every part was on the clouds, and what had landed was erased. Upload the file again."
		}
		return "Nothing of the file was kept. Try the upload again."
	}

	var b strings.Builder
	b.WriteString("Nothing of the file was kept: ")
	switch e.Stored {
	case 0:
		b.WriteString("no part landed anywhere.")
	case 1:
		b.WriteString("the one part that landed was erased again.")
	default:
		fmt.Fprintf(&b, "the %d parts that landed were erased again.", e.Stored)
	}

	seen := map[string]bool{}
	for _, f := range e.Failures {
		if seen[f.accountKey()] {
			continue
		}
		seen[f.accountKey()] = true
		b.WriteString(" ")
		b.WriteString(refusalAdvice(f))
	}
	b.WriteString(" Once that is put right, upload the file again — or upload it now, choosing other clouds for it.")
	return b.String()
}

// refusal is the shape of a cloud's "no", as far as it can be read from the
// words. Backends word their failures differently and only some say a status
// code, so this is a reading of the sentence rather than a fact about it —
// enough to point at the right place without claiming to be sure.
type refusal int

const (
	refusalUnknown refusal = iota
	refusalNoRoom
	refusalThrottled
	refusalSignIn
	refusalUnreachable
)

// classifyRefusal reads a backend's reason for turning a part away. Room
// is looked for first: Google answers a full account with a 403, which read
// alone would say "sign in again" to somebody whose sign-in is fine.
func classifyRefusal(reason string) refusal {
	msg := strings.ToLower(reason)
	has := func(words ...string) bool {
		for _, w := range words {
			if strings.Contains(msg, w) {
				return true
			}
		}
		return false
	}
	switch {
	case has("507", "insufficient storage", "insufficient_space", "insufficientspace",
		"quota", "storage limit", "no space", "out of space", "enospc", "is full", "disk full"):
		return refusalNoRoom
	case has("429", "too many requests", "rate limit", "ratelimit", "slow down"):
		return refusalThrottled
	case has("401", "403", "unauthorized", "forbidden", "invalid_grant", "invalid_client",
		"sign in", "signed out", "token", "credential", "access denied", "authentication"):
		return refusalSignIn
	case has("timeout", "timed out", "deadline exceeded", "no such host", "connection refused",
		"connection reset", "unreachable", "not answering", "bad gateway", "service unavailable",
		"gateway timeout", "502", "503", "504", "dial tcp", "broken pipe", "unexpected eof",
		"tls", "does not exist", "not mounted", "not a folder", "not a directory"):
		return refusalUnreachable
	}
	return refusalUnknown
}

// refusalAdvice is the sentence about one account that refused a part: what
// it looks like it was saying, and where to go about it.
func refusalAdvice(f ShardFailure) string {
	switch classifyRefusal(f.Reason) {
	case refusalNoRoom:
		return fmt.Sprintf("%s has no room left for it: free some space there, or choose other clouds for this file.", f.Account)
	case refusalThrottled:
		return fmt.Sprintf("%s asked SAND to slow down: wait a few minutes before trying again.", f.Account)
	case refusalSignIn:
		return fmt.Sprintf("%s turned the part away as not signed in: sign in to it again from its card under Clouds.", f.Account)
	case refusalUnreachable:
		if f.Kind == "local" {
			return fmt.Sprintf("%s is a folder on this machine that could not be reached: check that the drive is mounted and the folder is still there.", f.Account)
		}
		return fmt.Sprintf("%s did not answer: check it under Cloud health, and try again once it is reachable.", f.Account)
	}
	if f.Kind == "local" {
		return fmt.Sprintf("%s is a folder on this machine that could not be written: check that it is mounted and writable.", f.Account)
	}
	return fmt.Sprintf("%s refused the part: check it under Cloud health before trying again.", f.Account)
}

// failedAt says where an upload was when err happened, unless err already
// says: an error that came up through a stage keeps the stage it was given
// where it happened, which is always the more exact one.
func failedAt(stage UploadStage, err error) error {
	if err == nil {
		return nil
	}
	var ue *UploadError
	if errors.As(err, &ue) {
		return err
	}
	return &UploadError{Stage: stage, Err: err}
}

// shortOfShards is the error for a scatter that landed too few distinct
// shards to read the file back: the failures in shard order, and the count.
func shortOfShards(stored, needed, total int, failures map[int]ShardFailure) *UploadError {
	e := &UploadError{Stage: UploadScattering, Stored: stored, Needed: needed, Total: total}
	for _, f := range failures {
		e.Failures = append(e.Failures, f)
	}
	sort.Slice(e.Failures, func(i, j int) bool { return e.Failures[i].Part < e.Failures[j].Part })
	return e
}
