package plugin_test

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"log"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/ZacxDev/muster-plugin-template/internal/echo"
	"github.com/ZacxDev/muster-plugin-template/internal/journal"
	"github.com/ZacxDev/muster-plugin-template/internal/muster"
	"github.com/ZacxDev/muster-plugin-template/internal/mustertest"
	"github.com/ZacxDev/muster-plugin-template/internal/plugin"
)

type syncBuf struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (s *syncBuf) Write(p []byte) (int, error) { s.mu.Lock(); defer s.mu.Unlock(); return s.b.Write(p) }
func (s *syncBuf) String() string              { s.mu.Lock(); defer s.mu.Unlock(); return s.b.String() }

type rig struct {
	fake   *mustertest.Fake
	runner *plugin.Runner
	sender *echo.Sender
	out    *syncBuf
	slept  []time.Duration
	dir    string
}

func newRig(t *testing.T, fake *mustertest.Fake, dir string) *rig {
	t.Helper()
	c, err := muster.New(fake.URL, mustertest.Token)
	if err != nil {
		t.Fatal(err)
	}
	j, err := journal.Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { j.Close() })
	up, err := echo.OpenUpstream(dir)
	if err != nil {
		t.Fatal(err)
	}
	up.Now = func() time.Time { return time.Date(2000, 1, 2, 3, 4, 5, 0, time.UTC) }
	r := &rig{fake: fake, out: &syncBuf{}, dir: dir}
	r.sender = &echo.Sender{Out: r.out}
	r.runner = &plugin.Runner{
		Client:   c,
		Upstream: up,
		Sender:   r.sender,
		Journal:  j,
		Interval: time.Hour,
		Log:      log.New(io.Discard, "", 0),
		Sleep: func(ctx context.Context, d time.Duration) error {
			r.slept = append(r.slept, d)
			return ctx.Err()
		},
	}
	return r
}

const draft17 = `{"draft_id":17,"kind":"reply","message_external_id":"echo-1","body":"Thanks, noon works.","approved_at":"2000-01-02T03:04:05Z"}`

func TestAClaimedDraftIsSentOnceAndItsResultReported(t *testing.T) {
	f := mustertest.New()
	defer f.Close()
	r := newRig(t, f, t.TempDir())
	f.Approve(draft17)

	if err := r.runner.ClaimOnce(context.Background()); err != nil {
		t.Fatal(err)
	}
	if got := r.out.String(); got != "SEND draft 17 (reply to echo-1):\nThanks, noon works.\n" {
		t.Fatalf("sent = %q", got)
	}
	_, results, _, claims := f.Snapshot()
	if claims != 1 {
		t.Fatalf("claims = %d, want 1", claims)
	}
	if len(results) != 1 || results[0].DraftID != 17 || results[0].Body != `{"ok":true,"external_ref":"echo-17"}` {
		t.Fatalf("results = %+v, want one ok result for draft 17", results)
	}
	if e, _ := r.runner.Journal.Get(17); e.State != journal.Reported || e.ExternalRef != "echo-17" {
		t.Fatalf("journal entry = %+v", e)
	}

	// Nothing approved: a 204, nothing sent, nothing reported.
	if err := r.runner.ClaimOnce(context.Background()); err != nil {
		t.Fatal(err)
	}
	if _, results, _, claims = f.Snapshot(); claims != 2 || len(results) != 1 {
		t.Fatalf("after a 204: claims %d results %d", claims, len(results))
	}
}

func TestADraftIsNeverSentTwiceAcrossARestart(t *testing.T) {
	dir := t.TempDir()
	f := mustertest.New()
	defer f.Close()
	first := newRig(t, f, dir)
	f.Approve(draft17)
	if err := first.runner.ClaimOnce(context.Background()); err != nil {
		t.Fatal(err)
	}
	first.runner.Journal.Close()

	// A new process on the same state directory is handed draft 17 again.
	second := newRig(t, f, dir)
	f.Approve(draft17)
	if err := second.runner.ClaimOnce(context.Background()); err != nil {
		t.Fatal(err)
	}
	if got := second.out.String(); got != "" {
		t.Fatalf("draft 17 was sent again after a restart: %q", got)
	}
	_, results, _, _ := f.Snapshot()
	if len(results) != 2 || results[1].Body != `{"ok":true,"external_ref":"echo-17"}` {
		t.Fatalf("results = %+v, want the same ok result reported again", results)
	}
}

type probeWriter func(p []byte) (int, error)

func (f probeWriter) Write(p []byte) (int, error) { return f(p) }

// The draft must be in the journal BEFORE the send starts: a crash mid-send
// then leaves a record that stops the next process sending it again.
func TestTheDraftIsJournalledBeforeTheSendStarts(t *testing.T) {
	f := mustertest.New()
	defer f.Close()
	r := newRig(t, f, t.TempDir())
	var during journal.Entry
	var seen bool
	r.sender.Out = probeWriter(func(p []byte) (int, error) {
		during, seen = r.runner.Journal.Get(17)
		return len(p), nil
	})
	f.Approve(draft17)
	if err := r.runner.ClaimOnce(context.Background()); err != nil {
		t.Fatal(err)
	}
	if !seen || during.State != journal.Claimed {
		t.Fatalf("journal during the send = %+v (present %v), want state claimed", during, seen)
	}
}

func TestADraftClaimedButNotKnownSentIsNeitherSentNorReported(t *testing.T) {
	dir := t.TempDir()
	j, err := journal.Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	if err := j.Record(17, journal.Claimed, ""); err != nil {
		t.Fatal(err)
	}
	j.Close()
	f := mustertest.New()
	defer f.Close()
	r := newRig(t, f, dir)
	f.Approve(draft17)
	if err := r.runner.ClaimOnce(context.Background()); err != nil {
		t.Fatal(err)
	}
	if _, results, _, _ := f.Snapshot(); r.out.String() != "" || len(results) != 0 {
		t.Fatalf("sent %q, results %+v: want neither", r.out.String(), results)
	}
}

func TestDeliverHonoursRetryAfterAndResendsTheSameMessage(t *testing.T) {
	f := mustertest.New()
	defer f.Close()
	f.RateLimit = 1
	r := newRig(t, f, t.TempDir())
	if err := r.runner.Deliver(context.Background(), r.runner.Upstream.Next()); err != nil {
		t.Fatal(err)
	}
	msgs, _, _, _ := f.Snapshot()
	want := `{"external_id":"echo-1","sender":"someone@example.com","subject":"echo 1","body":"Hello from the echo upstream (message 1). Please reply with anything.","received_at":"2000-01-02T03:04:05Z","no_reply":false,"sender_verified":false}`
	if len(msgs) != 2 || msgs[0] != want || msgs[1] != want {
		t.Fatalf("posts = %q, want the same message twice", msgs)
	}
	if len(r.slept) != 1 || r.slept[0] != 7*time.Second {
		t.Fatalf("slept %v, want [7s] (Retry-After)", r.slept)
	}
}

func TestARevokedTokenOrPluginsOffStops(t *testing.T) {
	for name, set := range map[string]func(*mustertest.Fake){
		"401": func(f *mustertest.Fake) { f.Revoked = true },
		"404": func(f *mustertest.Fake) { f.Off = true },
	} {
		f := mustertest.New()
		set(f)
		r := newRig(t, f, t.TempDir())
		for what, err := range map[string]error{
			"deliver": r.runner.Deliver(context.Background(), r.runner.Upstream.Next()),
			"claim":   r.runner.ClaimOnce(context.Background()),
			"run":     r.runner.Run(context.Background()),
		} {
			if !errors.Is(err, plugin.ErrStop) {
				t.Errorf("%s: %s = %v, want ErrStop", name, what, err)
			}
		}
		f.Close()
	}
}

func TestRunDoesTheWholeRoundTrip(t *testing.T) {
	f := mustertest.New()
	defer f.Close()
	r := newRig(t, f, t.TempDir())
	r.runner.Sleep = nil
	r.runner.Interval = time.Hour
	f.Approve(draft17)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	done := make(chan error, 1)
	go func() { done <- r.runner.Run(ctx) }()
	for {
		msgs, results, _, _ := f.Snapshot()
		if len(msgs) >= 1 && len(results) >= 1 {
			break
		}
		if ctx.Err() != nil {
			t.Fatalf("timed out: messages %d results %d", len(msgs), len(results))
		}
		time.Sleep(10 * time.Millisecond)
	}
	cancel()
	if err := <-done; err != nil {
		t.Fatalf("Run = %v", err)
	}
	msgs, results, _, _ := f.Snapshot()
	if !strings.Contains(msgs[0], `"external_id":"echo-1"`) || results[0].Body != `{"ok":true,"external_ref":"echo-17"}` {
		t.Fatalf("messages %q results %+v", msgs, results)
	}
}

const echo1 = `{"external_id":"echo-1","sender":"someone@example.com","subject":"echo 1","body":"Hello from the echo upstream (message 1). Please reply with anything.","received_at":"2000-01-02T03:04:05Z","no_reply":false,"sender_verified":false}`

// cursor reads the echo upstream's persisted cursor ("" when none was written).
func cursor(t *testing.T, dir string) string {
	t.Helper()
	b, err := os.ReadFile(filepath.Join(dir, echo.CursorFile))
	if errors.Is(err, os.ErrNotExist) {
		return ""
	}
	if err != nil {
		t.Fatal(err)
	}
	return strings.TrimSpace(string(b))
}

// A 409 on a message means muster stored nothing (a bound plugin's officer is
// not defined): the same message is sent again, waiting longer each time up
// to MaxBackoff, and the cursor moves only once muster stores it.
func TestA409OnAMessageIsSentAgainWithBackoff(t *testing.T) {
	f := mustertest.New()
	defer f.Close()
	f.Conflict = 4
	r := newRig(t, f, t.TempDir())
	r.runner.MaxBackoff = 12 * time.Second
	if err := r.runner.DeliverNext(context.Background()); err != nil {
		t.Fatal(err)
	}
	msgs, _, _, _ := f.Snapshot()
	if len(msgs) != 5 || msgs[0] != echo1 || msgs[4] != echo1 {
		t.Fatalf("409: %d posts %q, want the same message 5 times (4 answered 409, then stored)", len(msgs), msgs)
	}
	want := []time.Duration{5 * time.Second, 10 * time.Second, 12 * time.Second, 12 * time.Second}
	if len(r.slept) != len(want) {
		t.Fatalf("409: slept %v, want %v (doubling, capped at MaxBackoff)", r.slept, want)
	}
	for i := range want {
		if r.slept[i] != want[i] {
			t.Fatalf("409: slept %v, want %v (doubling, capped at MaxBackoff)", r.slept, want)
		}
	}
	if got := cursor(t, r.dir); got != "1" {
		t.Fatalf("409: cursor = %q after muster stored echo-1, want 1", got)
	}
}

// However long muster keeps answering 409, the cursor never moves past the
// message: a stop and a restart send echo-1 again, not echo-2.
func TestA409NeverAdvancesTheCursor(t *testing.T) {
	dir := t.TempDir()
	f := mustertest.New()
	defer f.Close()
	f.Conflict = 1 << 30
	r := newRig(t, f, dir)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	r.runner.Sleep = func(ctx context.Context, d time.Duration) error {
		r.slept = append(r.slept, d)
		if len(r.slept) == 3 {
			cancel() // the operator stops the plugin mid-retry
		}
		return ctx.Err()
	}
	if err := r.runner.DeliverNext(ctx); err == nil {
		t.Fatalf("409: DeliverNext returned nil while muster stored nothing")
	}
	if got := cursor(t, dir); got != "" {
		t.Fatalf("409: the cursor advanced to %q although muster stored nothing", got)
	}
	again, err := echo.OpenUpstream(dir)
	if err != nil {
		t.Fatal(err)
	}
	if id := again.Next().ExternalID; id != "echo-1" {
		t.Fatalf("409: after a restart the next message is %s, want echo-1 again", id)
	}
}

type sendFunc func(muster.Claim) (string, error)

func (f sendFunc) Send(c muster.Claim) (string, error) { return f(c) }

// A sender that cannot tell whether the reply went reports nothing: muster
// marks it unconfirmed after its lease, and it is never sent again.
func TestAnUnknownSendReportsNothing(t *testing.T) {
	f := mustertest.New()
	defer f.Close()
	r := newRig(t, f, t.TempDir())
	r.runner.Sender = sendFunc(func(muster.Claim) (string, error) {
		return "", fmt.Errorf("%w: the upstream timed out after the request left", plugin.ErrUnknown)
	})
	f.Approve(draft17)
	if err := r.runner.ClaimOnce(context.Background()); err != nil {
		t.Fatal(err)
	}
	if _, results, _, _ := f.Snapshot(); len(results) != 0 {
		t.Fatalf("unknown send: results %+v, want none (a result would claim to know)", results)
	}
	if e, _ := r.runner.Journal.Get(17); e.State != journal.Claimed {
		t.Fatalf("unknown send: journal entry %+v, want claimed (never sent again)", e)
	}
}

// A sender that knows the reply was not sent reports "ok": false with the
// reason, so the reply goes back to be written again.
func TestAKnownFailedSendReportsOKFalse(t *testing.T) {
	f := mustertest.New()
	defer f.Close()
	r := newRig(t, f, t.TempDir())
	r.runner.Sender = sendFunc(func(muster.Claim) (string, error) {
		return "", errors.New("the upstream refused it: 550 no such mailbox")
	})
	f.Approve(draft17)
	if err := r.runner.ClaimOnce(context.Background()); err != nil {
		t.Fatal(err)
	}
	_, results, _, _ := f.Snapshot()
	if len(results) != 1 || results[0].Body != `{"ok":false,"detail":"send failed: the upstream refused it: 550 no such mailbox"}` {
		t.Fatalf("known failure: results %+v, want one ok:false with the detail", results)
	}
}

// A result the journal holds as sent but muster never recorded (a crash
// between the send and the result) is posted when Run starts.
func TestRunPostsAResultTheJournalHoldsAsSent(t *testing.T) {
	dir := t.TempDir()
	j, err := journal.Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	if err := j.Record(17, journal.Claimed, ""); err != nil {
		t.Fatal(err)
	}
	if err := j.Record(17, journal.Sent, "echo-17"); err != nil {
		t.Fatal(err)
	}
	j.Close()
	f := mustertest.New()
	defer f.Close()
	f.MarkClaimed(17) // the earlier process claimed it
	r := newRig(t, f, dir)
	r.runner.Sleep = nil
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	done := make(chan error, 1)
	go func() { done <- r.runner.Run(ctx) }()
	// Run posts recovered results before it starts the loops, so the first
	// message proves recovery is over.
	for {
		if msgs, _, _, _ := f.Snapshot(); len(msgs) >= 1 {
			break
		}
		if ctx.Err() != nil {
			t.Fatalf("recovery: Run never reached its loops")
		}
		time.Sleep(10 * time.Millisecond)
	}
	cancel()
	if err := <-done; err != nil {
		t.Fatalf("Run = %v", err)
	}
	_, results, _, _ := f.Snapshot()
	if len(results) != 1 || results[0].DraftID != 17 || results[0].Body != `{"ok":true,"external_ref":"echo-17"}` {
		t.Fatalf("recovery: results %+v, want the journal's sent draft 17 reported ok with echo-17", results)
	}
	if e, _ := r.runner.Journal.Get(17); e.State != journal.Reported {
		t.Fatalf("recovery: journal entry %+v, want reported", e)
	}
	if r.out.String() != "" {
		t.Fatalf("recovery: sent %q, want nothing sent again", r.out.String())
	}
}
