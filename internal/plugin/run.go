// Package plugin is the template's two loops: bring upstream messages in,
// and send the replies the owner approved out.
//
//	upstream ──POST /plugin/v1/messages──▶ muster ──▶ chief (or an officer)
//	                                         │ a reply is drafted (by the chief, or a
//	                                         │ session it routes to); the owner approves it
//	sender ◀──POST /plugin/v1/drafts/claim── muster
//	sender ──POST /plugin/v1/drafts/{id}/result──▶ muster
//
// muster never calls the plugin; the claim's long-poll is the clock.
package plugin

import (
	"context"
	"errors"
	"fmt"
	"log"
	"sync"
	"time"

	"github.com/ZacxDev/muster-plugin-template/internal/journal"
	"github.com/ZacxDev/muster-plugin-template/internal/muster"
)

// ErrStop wraps a refusal that no retry fixes: the token is revoked (401),
// lacks a capability (403), or the server has plugins off (404).
var ErrStop = errors.New("stopping")

// ErrUnknown is what a Sender returns (wrapped) when it cannot tell whether
// the reply went: a timeout after the request left, a dropped connection
// mid-send. The plugin then reports nothing, and muster marks the draft
// unconfirmed when its lease lapses; it never offers it again, and the owner
// decides. Return a plain error only when the reply is known NOT to have been
// sent: that is reported "ok": false, and the reply goes back to whoever
// wrote it to try again (so a reply that did go may be sent twice).
var ErrUnknown = errors.New("not known whether the reply was sent")

// Upstream is the system the messages come from (internal/echo's fake inbox
// in this template).
type Upstream interface {
	// Next returns the next message, the same one until Advance.
	Next() muster.Message
	// Advance moves the cursor past it, durably. The runner calls it only
	// after muster answered 2xx (or refused the message for good with 400).
	Advance() error
}

// Sender delivers an approved reply upstream and returns the upstream's id
// for it. It returns an error wrapping ErrUnknown when the reply may or may
// not have gone, and any other error only when it certainly did not.
type Sender interface {
	Send(muster.Claim) (externalRef string, err error)
}

// Runner holds the plugin's parts. Every field but Sleep, Backoff,
// MaxBackoff and ResultAttempts is required.
type Runner struct {
	Client   *muster.Client
	Upstream Upstream
	Sender   Sender
	Journal  *journal.Journal
	Interval time.Duration // how often the fake upstream produces a message
	Log      *log.Logger
	// Sleep waits d or until ctx ends; tests replace it.
	Sleep func(ctx context.Context, d time.Duration) error
	// Backoff after a network or server error (default 5 s), and the first
	// wait after a 409 on a message.
	Backoff time.Duration
	// MaxBackoff caps the doubling wait after repeated 409s on a message
	// (default 5 min).
	MaxBackoff time.Duration
	// ResultAttempts bounds re-posting one result (default 5).
	ResultAttempts int
}

func (r *Runner) sleep(ctx context.Context, d time.Duration) error {
	if r.Sleep != nil {
		return r.Sleep(ctx, d)
	}
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-t.C:
		return nil
	}
}

func (r *Runner) backoff() time.Duration {
	if r.Backoff > 0 {
		return r.Backoff
	}
	return 5 * time.Second
}

func (r *Runner) maxBackoff() time.Duration {
	if r.MaxBackoff > 0 {
		return r.MaxBackoff
	}
	return 5 * time.Minute
}

// fatal turns a refusal no retry fixes into ErrStop.
func fatal(err error) error {
	e := muster.AsError(err)
	if e == nil {
		return nil
	}
	switch e.Kind {
	case muster.KindAuth:
		return fmt.Errorf("%w: muster refused the plugin token (revoked, unknown, or its owner is disabled): %v", ErrStop, e)
	case muster.KindCapability:
		return fmt.Errorf("%w: the plugin token lacks a capability this plugin needs: %v", ErrStop, e)
	case muster.KindOff:
		return fmt.Errorf("%w: muster answered 404: plugins are off on this server (set MUSTER_PLUGINS=1 there): %v", ErrStop, e)
	}
	return nil
}

// Hello is GET /plugin/v1/self, logged. It never logs the token.
func (r *Runner) Hello(ctx context.Context) error {
	s, err := r.Client.Self(ctx)
	if err != nil {
		if f := fatal(err); f != nil {
			return f
		}
		return err
	}
	r.Log.Printf("muster: plugin %q (id %d), owner %s, capabilities %v", s.PluginName, s.PluginID, s.OwnerLogin, s.Capabilities)
	return nil
}

// Deliver posts one upstream message, retrying it until muster answers 2xx,
// and returns nil only then, or on a 400 (logged and dropped: sending it
// again cannot help). A 409 means muster stored nothing (the officer this
// plugin delivers to is not defined): the same message is sent again, waiting
// longer each time up to MaxBackoff. On any non-nil return the caller must
// not advance the upstream cursor.
func (r *Runner) Deliver(ctx context.Context, m muster.Message) error {
	conflictWait := r.backoff()
	for {
		ack, err := r.Client.PostMessage(ctx, m)
		if err == nil {
			if ack.Created {
				r.Log.Printf("message %s stored as %d (%s)", m.ExternalID, ack.ID, ack.Status)
			} else {
				r.Log.Printf("message %s was already stored as %d (%s)", m.ExternalID, ack.ID, ack.Status)
			}
			return nil
		}
		if f := fatal(err); f != nil {
			return f
		}
		e := muster.AsError(err)
		switch {
		case e != nil && e.Kind == muster.KindRateLimited:
			r.Log.Printf("message %s: rate limited, retrying in %s", m.ExternalID, e.RetryAfter)
			if err := r.sleep(ctx, e.RetryAfter); err != nil {
				return err
			}
		case e != nil && e.Retryable():
			r.Log.Printf("message %s: %v; retrying", m.ExternalID, e)
			if err := r.sleep(ctx, r.backoff()); err != nil {
				return err
			}
		case e != nil && e.Kind == muster.KindConflict:
			r.Log.Printf("message %s: muster stored nothing (%v); sending it again in %s", m.ExternalID, e, conflictWait)
			if err := r.sleep(ctx, conflictWait); err != nil {
				return err
			}
			conflictWait = min(2*conflictWait, r.maxBackoff())
		default:
			r.Log.Printf("message %s refused, dropped: %v", m.ExternalID, err)
			return nil
		}
	}
}

// DeliverNext posts the upstream's next message and, once muster has it,
// advances the upstream cursor. A message muster did not store leaves the
// cursor where it was, so it is the next one sent again.
func (r *Runner) DeliverNext(ctx context.Context) error {
	if err := r.Deliver(ctx, r.Upstream.Next()); err != nil {
		return err
	}
	return r.Upstream.Advance()
}

// ClaimOnce long-polls for one approved draft and settles it.
func (r *Runner) ClaimOnce(ctx context.Context) error {
	c, err := r.Client.Claim(ctx, muster.MaxClaimWait)
	if err != nil {
		if f := fatal(err); f != nil {
			return f
		}
		if ctx.Err() != nil {
			return ctx.Err()
		}
		r.Log.Printf("claim: %v; retrying", err)
		return r.sleep(ctx, r.backoff())
	}
	if c == nil {
		return nil // 204: nothing approved yet
	}
	return r.Settle(ctx, *c)
}

// Settle sends one claimed draft at most once and reports its result.
func (r *Runner) Settle(ctx context.Context, c muster.Claim) error {
	if e, ok := r.Journal.Get(c.DraftID); ok {
		switch e.State {
		case journal.Sent, journal.Reported:
			r.Log.Printf("draft %d was already sent; reporting its result again, not sending it", c.DraftID)
			return r.report(ctx, c.DraftID, muster.Result{OK: true, ExternalRef: e.ExternalRef})
		default:
			// Claimed but not known to be sent: it may or may not have gone.
			// Report nothing; muster marks it unconfirmed and never offers it
			// again, and your owner decides.
			r.Log.Printf("draft %d was claimed before a restart and may have been sent; not sending it again", c.DraftID)
			return nil
		}
	}
	if err := r.Journal.Record(c.DraftID, journal.Claimed, ""); err != nil {
		return fmt.Errorf("journal: %w (draft %d not sent)", err, c.DraftID)
	}
	ref, err := r.Sender.Send(c)
	if errors.Is(err, ErrUnknown) {
		// It may have gone. Report nothing: "ok": false would offer it to be
		// written and sent again. muster marks it unconfirmed after the lease.
		r.Log.Printf("draft %d: not known whether it was sent (%v); reporting nothing, muster will mark it unconfirmed", c.DraftID, err)
		return nil
	}
	if err != nil {
		return r.report(ctx, c.DraftID, muster.Result{OK: false, Detail: "send failed: " + err.Error()})
	}
	if err := r.Journal.Record(c.DraftID, journal.Sent, ref); err != nil {
		r.Log.Printf("journal: %v (draft %d WAS sent)", err, c.DraftID)
	}
	return r.report(ctx, c.DraftID, muster.Result{OK: true, ExternalRef: ref})
}

// report posts a result, again on a network or server error (a repeat is
// safe), and records it in the journal once muster has it.
func (r *Runner) report(ctx context.Context, id int64, res muster.Result) error {
	attempts := r.ResultAttempts
	if attempts <= 0 {
		attempts = 5
	}
	for i := 1; ; i++ {
		ack, err := r.Client.Result(ctx, id, res)
		if err == nil {
			r.Log.Printf("draft %d: muster recorded %s", id, ack.Status)
			if res.OK {
				_ = r.Journal.Record(id, journal.Reported, res.ExternalRef)
			}
			return nil
		}
		if f := fatal(err); f != nil && muster.AsError(err).Kind != muster.KindOff {
			return f
		}
		e := muster.AsError(err)
		if e == nil || !e.Retryable() || i >= attempts {
			// 404: not a draft this plugin claimed; 409: it already has the
			// other result. Neither is fixed by sending it again.
			r.Log.Printf("draft %d: result not recorded: %v", id, err)
			return nil
		}
		d := r.backoff()
		if e.Kind == muster.KindRateLimited {
			d = e.RetryAfter
		}
		if err := r.sleep(ctx, d); err != nil {
			return err
		}
	}
}

// Run reports any result a restart left unreported, then runs both loops
// until ctx ends or a refusal stops them.
func (r *Runner) Run(ctx context.Context) error {
	if err := r.Hello(ctx); err != nil {
		return err
	}
	for id, e := range r.Journal.Unreported() {
		if err := r.report(ctx, id, muster.Result{OK: true, ExternalRef: e.ExternalRef}); err != nil {
			return err
		}
	}
	ctx, cancel := context.WithCancelCause(ctx)
	defer cancel(nil)
	var wg sync.WaitGroup
	wg.Add(2)
	go func() {
		defer wg.Done()
		for ctx.Err() == nil {
			if err := r.DeliverNext(ctx); err != nil {
				cancel(err)
				return
			}
			if r.sleep(ctx, r.Interval) != nil {
				return
			}
		}
	}()
	go func() {
		defer wg.Done()
		for ctx.Err() == nil {
			if err := r.ClaimOnce(ctx); err != nil && ctx.Err() == nil {
				cancel(err)
				return
			}
		}
	}()
	wg.Wait()
	if err := context.Cause(ctx); err != nil && !errors.Is(err, context.Canceled) {
		return err
	}
	return nil
}
