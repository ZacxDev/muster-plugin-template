// Package echo is the template's stand-in for a real upstream system: a fake
// inbox that produces one message per tick, and a "sender" that writes an
// approved reply to a writer instead of sending it anywhere.
//
// To make this template your own plugin, replace Upstream with a reader of
// your system (an inbox, a chat, a tracker) and Sender with the call that
// actually delivers a reply there. Nothing else needs to change.
package echo

import (
	"fmt"
	"io"
	"strconv"
	"sync"
	"time"

	"github.com/ZacxDev/muster-plugin-template/internal/muster"
)

// Upstream produces fake inbound messages: echo-1, echo-2, …
type Upstream struct {
	mu  sync.Mutex
	n   int
	Now func() time.Time
}

// Next returns the next message. The same n always gives the same
// external_id, so a message re-sent after a lost answer is stored once.
func (u *Upstream) Next() muster.Message {
	u.mu.Lock()
	defer u.mu.Unlock()
	u.n++
	now := time.Now
	if u.Now != nil {
		now = u.Now
	}
	t := now().UTC().Truncate(time.Second)
	return muster.Message{
		ExternalID: "echo-" + strconv.Itoa(u.n),
		Sender:     "someone@example.com",
		Subject:    fmt.Sprintf("echo %d", u.n),
		Body:       fmt.Sprintf("Hello from the echo upstream (message %d). Please reply with anything.", u.n),
		ReceivedAt: &t,
	}
}

// Sender "sends" an approved reply by writing it to Out.
type Sender struct {
	mu  sync.Mutex
	Out io.Writer
}

// Send writes the reply and returns the upstream's reference for it.
func (s *Sender) Send(c muster.Claim) (externalRef string, err error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, err := fmt.Fprintf(s.Out, "SEND draft %d (reply to %s):\n%s\n", c.DraftID, c.MessageExternalID, c.Body); err != nil {
		return "", err
	}
	return "echo-" + strconv.FormatInt(c.DraftID, 10), nil
}
