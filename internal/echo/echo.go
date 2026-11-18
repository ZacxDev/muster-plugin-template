// Package echo is the template's stand-in for a real upstream system: a fake
// inbox that produces one message per tick, and a "sender" that writes an
// approved reply to a writer instead of sending it anywhere.
//
// To make this template your own plugin, replace Upstream with a reader of
// your system (an inbox, a chat, a tracker) and Sender with the call that
// actually delivers a reply there. Nothing else needs to change.
package echo

import (
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/ZacxDev/muster-plugin-template/internal/muster"
)

// CursorFile is the file in the state directory that holds the upstream
// cursor: the number of the last message muster accepted.
const CursorFile = "echo-cursor"

// Upstream produces fake inbound messages: echo-1, echo-2, … Its cursor is
// kept in a state directory, so a restart neither repeats a message muster
// already accepted nor skips one it did not. Make one with OpenUpstream.
type Upstream struct {
	mu  sync.Mutex
	dir string
	n   int // the last message muster accepted
	Now func() time.Time
}

// OpenUpstream reads the cursor from dir/echo-cursor (none yet: start at 0).
func OpenUpstream(dir string) (*Upstream, error) {
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, err
	}
	u := &Upstream{dir: dir}
	b, err := os.ReadFile(filepath.Join(dir, CursorFile))
	switch {
	case errors.Is(err, os.ErrNotExist):
		return u, nil
	case err != nil:
		return nil, err
	}
	n, err := strconv.Atoi(strings.TrimSpace(string(b)))
	if err != nil || n < 0 {
		return nil, fmt.Errorf("echo cursor %s is unreadable; fix or move it before starting", filepath.Join(dir, CursorFile))
	}
	u.n = n
	return u, nil
}

// Next returns the next message, without moving the cursor: until Advance,
// every call returns the same message. The same number always gives the same
// external_id, so a message re-sent after a lost answer is stored once.
func (u *Upstream) Next() muster.Message {
	u.mu.Lock()
	defer u.mu.Unlock()
	n := u.n + 1
	now := time.Now
	if u.Now != nil {
		now = u.Now
	}
	t := now().UTC().Truncate(time.Second)
	return muster.Message{
		ExternalID: "echo-" + strconv.Itoa(n),
		Sender:     "someone@example.com",
		Subject:    fmt.Sprintf("echo %d", n),
		Body:       fmt.Sprintf("Hello from the echo upstream (message %d). Please reply with anything.", n),
		ReceivedAt: &t,
	}
}

// Advance moves the cursor past the message Next returns, and writes it to
// the state directory durably before returning. Call it only once muster has
// answered 2xx for that message (or refused it for good with a 400).
func (u *Upstream) Advance() error {
	u.mu.Lock()
	defer u.mu.Unlock()
	if err := writeFileSync(u.dir, CursorFile, []byte(strconv.Itoa(u.n+1)+"\n")); err != nil {
		return fmt.Errorf("echo cursor: %w", err)
	}
	u.n++
	return nil
}

// writeFileSync replaces dir/name with b: a temporary file, fsynced, renamed
// over it, then the directory fsynced so the rename survives a crash.
func writeFileSync(dir, name string, b []byte) error {
	tmp, err := os.CreateTemp(dir, name+".tmp-*")
	if err != nil {
		return err
	}
	defer os.Remove(tmp.Name()) // a no-op once renamed
	if _, err := tmp.Write(b); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Sync(); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	if err := os.Rename(tmp.Name(), filepath.Join(dir, name)); err != nil {
		return err
	}
	d, err := os.Open(dir)
	if err != nil {
		return err
	}
	defer d.Close()
	return d.Sync()
}

// Sender "sends" an approved reply by writing it to Out.
type Sender struct {
	mu  sync.Mutex
	Out io.Writer
}

// Send writes the reply and returns the upstream's reference for it. A real
// sender returns an error wrapping plugin.ErrUnknown when it cannot tell
// whether the reply went (a timeout after the request left, say), and a plain
// error only when it knows the reply was not sent. This stand-in treats a
// failed write to Out as not sent, so it never returns ErrUnknown.
func (s *Sender) Send(c muster.Claim) (externalRef string, err error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, err := fmt.Fprintf(s.Out, "SEND draft %d (reply to %s):\n%s\n", c.DraftID, c.MessageExternalID, c.Body); err != nil {
		return "", err
	}
	return "echo-" + strconv.FormatInt(c.DraftID, 10), nil
}
