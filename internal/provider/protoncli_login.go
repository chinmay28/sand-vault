package provider

// Signing a Proton account in.
//
// The client signs in through a browser and hands the session to whatever store
// it was told to use. SAND tells it to use a file, waits for it, and takes what
// it wrote — see the note on stage in protoncli.go for why the file is a pipe
// rather than a home.
//
// The shape of the flow is set by the client rather than chosen here: it prints
// a URL, then blocks until somebody has visited it and finished signing in.
// That is not a redirect SAND can catch, so this is not the OAuth flow the
// other backends use, and the URL is something to show rather than somewhere to
// send the browser. It can be opened on a different device from the one SAND
// runs on, which is the whole reason a headless box can connect at all.

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"strings"
	"time"
)

// ProtonCLILoginTimeout is how long a sign-in may sit unfinished before the
// client is killed. Long enough to find a password manager, a second factor
// and possibly another device; short enough that an abandoned sign-in does not
// leave a process waiting for the rest of the afternoon.
const ProtonCLILoginTimeout = 15 * time.Minute

// ProtonCLISignIn runs the Proton client's browser sign-in for an account and
// returns the settings that connect it.
//
// onURL is called once, as soon as the client says where to sign in, and must
// not block: it is what puts the link in front of somebody, and everything
// after it waits on them following it.
//
// The returned map is the settings the sign-in settled — the session, and
// nothing else — ready to be handed to a connect or an edit, which is where
// they are verified. Nothing here pings anything: this function's business is
// getting a session, and whether that session can reach the folder is the same
// question asked of every other credential and answered in the same place.
func ProtonCLISignIn(ctx context.Context, cfg Config, onURL func(string)) (map[string]string, error) {
	// An account being connected for the first time has no ID yet, and so no
	// state directory of its own to derive from one. Giving it the fallback
	// would hand the same directory to every account connected this way in
	// turn, and each sign-in would land on the last one's cache. A directory
	// that lasts as long as the sign-in avoids the question: what is wanted out
	// of this is the session, and the account gets a directory of its own as
	// soon as it has an ID to name one after.
	if cfg.ID == "" && strings.TrimSpace(cfg.Option("state_dir")) == "" {
		temp, err := os.MkdirTemp("", "sand-proton-login-")
		if err != nil {
			return nil, fmt.Errorf("preparing the Proton sign-in: %w", err)
		}
		defer os.RemoveAll(temp)

		scratch := cfg
		scratch.Options = make(map[string]string, len(cfg.Options)+1)
		for k, v := range cfg.Options {
			scratch.Options[k] = v
		}
		scratch.Options["state_dir"] = temp
		cfg = scratch
	}

	p, err := newProtonCLIProvider(cfg)
	if err != nil {
		return nil, err
	}
	client, ok := p.(*protonCLIProvider)
	if !ok {
		return nil, fmt.Errorf("proton drive: unexpected backend %T", p)
	}
	return client.signIn(ctx, onURL)
}

func (p *protonCLIProvider) signIn(ctx context.Context, onURL func(string)) (map[string]string, error) {
	bin, err := p.resolveBinary()
	if err != nil {
		return nil, err
	}

	p.mu.Lock()
	defer p.mu.Unlock()

	if err := os.MkdirAll(p.stateDir, 0o700); err != nil {
		return nil, fmt.Errorf("creating %s: %w", p.stateDir, err)
	}

	// A sign-in starts from nothing. Staging the session an account already has
	// would have the client decide it is signed in already and return without
	// asking anybody anything, which is exactly the wrong answer when the
	// reason for signing in again is that the old session stopped working.
	if err := os.Remove(p.sessionPath()); err != nil && !os.IsNotExist(err) {
		return nil, fmt.Errorf("clearing the previous Proton session: %w", err)
	}
	defer os.Remove(p.sessionPath())

	ctx, cancel := context.WithTimeout(ctx, ProtonCLILoginTimeout)
	defer cancel()

	cmd := exec.CommandContext(ctx, bin, "auth", "login", "--json")
	cmd.Env = p.environ()

	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return nil, fmt.Errorf("starting the Proton sign-in: %w", err)
	}
	// The client traces to stderr on a failure. Keeping it lets the sentence
	// somebody sees say what went wrong rather than only that it did.
	var stderr strings.Builder
	cmd.Stderr = &stderr

	if err := cmd.Start(); err != nil {
		return nil, fmt.Errorf("starting the Proton sign-in: %w", err)
	}

	// Read the URL out of the first line the client prints, while it goes on
	// waiting. Draining the pipe to the end matters even after the URL is
	// found: a client whose stdout fills up stops, and it would stop while
	// holding a session nobody could then collect.
	urlSeen := false
	scanner := bufio.NewScanner(stdout)
	for scanner.Scan() {
		if urlSeen {
			continue
		}
		var line struct {
			SignInURL string `json:"signInUrl"`
		}
		if err := json.Unmarshal(scanner.Bytes(), &line); err != nil || line.SignInURL == "" {
			continue
		}
		urlSeen = true
		if onURL != nil {
			onURL(line.SignInURL)
		}
	}

	if err := cmd.Wait(); err != nil {
		if ctx.Err() != nil {
			return nil, fmt.Errorf("the Proton sign-in was not finished in %s — start it again",
				ProtonCLILoginTimeout)
		}
		msg := protonCLIFirstMeaningfulLine(stderr.String())
		if msg == "" {
			msg = err.Error()
		}
		return nil, fmt.Errorf("the Proton sign-in failed: %s", msg)
	}
	if !urlSeen {
		return nil, fmt.Errorf("the Proton client never said where to sign in — check that "+
			"%s is a recent build", bin)
	}

	session, err := os.ReadFile(p.sessionPath())
	if err != nil {
		return nil, fmt.Errorf("the Proton sign-in finished but left no session behind: %w", err)
	}
	if len(strings.TrimSpace(string(session))) == 0 {
		return nil, fmt.Errorf("the Proton sign-in finished but left an empty session behind")
	}

	// The session alone. The folder and the binary came from whoever asked for
	// the sign-in and are theirs to keep; handing them back would write this
	// run's defaults into the account as though somebody had chosen them.
	return map[string]string{"session": string(session)}, nil
}

// ProtonCLISignOut tells the client to drop a session, so that disconnecting an
// account in SAND also ends it at Proton rather than leaving a live session
// behind on the account's security page.
//
// It is best-effort by design: an account being removed is being removed, and a
// client that cannot be reached to say so must not be able to prevent it.
func ProtonCLISignOut(ctx context.Context, cfg Config) error {
	p, err := newProtonCLIProvider(cfg)
	if err != nil {
		return err
	}
	client, ok := p.(*protonCLIProvider)
	if !ok {
		return fmt.Errorf("proton drive: unexpected backend %T", p)
	}
	client.mu.Lock()
	defer client.mu.Unlock()
	_, err = client.runLocked(ctx, "auth", "logout", "--json")
	return err
}
