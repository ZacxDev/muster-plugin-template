// Package plugin is the template's two loops: bring upstream messages in,
// and send the replies the owner approved out.
//
//	upstream ──POST /plugin/v1/messages──▶ muster ──▶ chief (or an officer)
//	                                         │ drafts a reply; the owner approves it
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

	"github.com/ZacxDev/muster-plugin-template/internal/echo"
	"github.com/ZacxDev/muster-plugin-template/internal/journal"
	"github.com/ZacxDev/muster-plugin-template/internal/muster"
)

// ErrStop wraps a refusal that no retry fixes: the token is revoked (401),
// lacks a capability (403), or the server has plugins off (404).
var ErrStop = errors.New("stopping")

// Runner holds the plugin's parts. Every field but Sleep is required.
type Runner struct {
	Client   *muster.Client
	Upstream *echo.Upstream
	Sender   *echo.Sender
	Journal  *journal.Journal
	Interval time.Duration // how often the fake upstream produces a message
	Log      *log.Logger
	// Sleep waits d or until ctx ends; tests replace it.
	Sleep func(ctx context.Context, d time.Duration) error
	// Backoff after a network or server error (default 5 s).
	Backoff time.Duration
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

// Deliver posts one upstream message, retrying it until muster answers 2xx:
// the upstream cursor (Upstream's counter) only moves on after that. A 400 is
// logged and the message dropped (sending it again cannot help).
func (r *Runner) Deliver(ctx context.Context, m muster.Message) error {
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
		default:
			r.Log.Printf("message %s refused, dropped: %v", m.ExternalID, err)
			return nil
		}
	}
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
			if err := r.Deliver(ctx, r.Upstream.Next()); err != nil {
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
