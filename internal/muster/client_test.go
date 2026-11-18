package muster_test

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/ZacxDev/muster-plugin-template/internal/muster"
	"github.com/ZacxDev/muster-plugin-template/internal/mustertest"
)

func client(t *testing.T, base string) *muster.Client {
	t.Helper()
	c, err := muster.New(base, mustertest.Token)
	if err != nil {
		t.Fatal(err)
	}
	return c
}

func TestCheckBaseURL(t *testing.T) {
	ok := map[string]string{
		"https://muster.example.com":     "https://muster.example.com",
		"https://muster.example.com/":    "https://muster.example.com",
		"https://example.com/muster/":    "https://example.com/muster",
		"http://localhost:8105":          "http://localhost:8105",
		"http://127.0.0.1:8105":          "http://127.0.0.1:8105",
		"http://[::1]:8105":              "http://[::1]:8105",
		"  https://muster.example.com  ": "https://muster.example.com",
	}
	for in, want := range ok {
		got, err := muster.CheckBaseURL(in)
		if err != nil || got != want {
			t.Errorf("CheckBaseURL(%q) = %q, %v; want %q", in, got, err, want)
		}
	}
	for _, bad := range []string{
		"", "muster.example.com", "http://muster.example.com", "ftp://localhost",
		"https://user:pw@muster.example.com", "https://muster.example.com/?x=1",
		"https://muster.example.com/#frag", "http://localhost.example.com",
	} {
		if got, err := muster.CheckBaseURL(bad); err == nil {
			t.Errorf("CheckBaseURL(%q) = %q, want a refusal", bad, got)
		}
	}
}

// A mistaken MUSTER_URL may hold the token (pasted into the wrong variable,
// or into the path): the refusal must not echo it to the log.
func TestCheckBaseURLNeverEchoesTheValue(t *testing.T) {
	const secret = "mpl_s3cretT0ken"
	for _, bad := range []string{
		secret, "muster.example.com/" + secret, "http://muster.example.com/" + secret,
		"https://muster.example.com/?token=" + secret, "https://" + secret + "@muster.example.com",
		"https://muster.example.com/#" + secret, "%zz" + secret,
	} {
		_, err := muster.CheckBaseURL(bad)
		if err == nil {
			t.Errorf("CheckBaseURL(%q) accepted it", bad)
			continue
		}
		if strings.Contains(err.Error(), "s3cretT0ken") {
			t.Errorf("CheckBaseURL refusal echoes the value: %v", err)
		}
	}
}

func TestNewRefusesATokenOfAnotherKind(t *testing.T) {
	for _, tok := range []string{"", "mpl_", "mst_abc", "abc"} {
		if _, err := muster.New("https://muster.example.com", tok); err == nil {
			t.Errorf("New(%q) accepted a non-plugin token", tok)
		}
	}
}

func TestPostMessageSendsTheTokenAndTheBodyAndAnswersARepeat(t *testing.T) {
	f := mustertest.New()
	defer f.Close()
	c := client(t, f.URL)
	at := time.Date(2000, 1, 2, 3, 4, 5, 0, time.UTC)
	m := muster.Message{ExternalID: "echo-1", Sender: "someone@example.com", Subject: "echo 1", Body: "hello", ReceivedAt: &at}

	ack, err := c.PostMessage(context.Background(), m)
	if err != nil || !ack.Created || ack.ID != 1 || ack.Status != "queued" {
		t.Fatalf("first post = %+v, %v; want created id 1 queued", ack, err)
	}
	ack, err = c.PostMessage(context.Background(), m)
	if err != nil || ack.Created || ack.ID != 1 {
		t.Fatalf("repeat = %+v, %v; want the stored id 1, not created", ack, err)
	}
	msgs, _, auth, _ := f.Snapshot()
	want := `{"external_id":"echo-1","sender":"someone@example.com","subject":"echo 1","body":"hello","received_at":"2000-01-02T03:04:05Z","no_reply":false,"sender_verified":false}`
	if len(msgs) != 2 || msgs[0] != want || msgs[1] != want {
		t.Fatalf("bodies = %q, want twice %s", msgs, want)
	}
	if auth[0] != "Bearer mpl_test_token_0123456789" {
		t.Fatalf("Authorization = %q", auth[0])
	}
}

func TestClaimSendsWaitSecondsAnd204IsNothing(t *testing.T) {
	var body string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		body = r.Method + " " + r.URL.Path + " " + string(b)
		w.WriteHeader(http.StatusNoContent)
	}))
	defer srv.Close()
	c, err := client(t, srv.URL).Claim(context.Background(), 99)
	if err != nil || c != nil {
		t.Fatalf("Claim on 204 = %+v, %v; want nil, nil", c, err)
	}
	if body != `POST /plugin/v1/drafts/claim {"wait_seconds":25}` {
		t.Fatalf("claim request = %q", body)
	}
}

func TestClaimReturnsTheApprovedDraft(t *testing.T) {
	f := mustertest.New()
	defer f.Close()
	f.Approve(`{"draft_id":17,"kind":"reply","message_external_id":"echo-1","body":"Thanks, noon works.","approved_at":"2000-01-02T03:04:05Z"}`)
	c, err := client(t, f.URL).Claim(context.Background(), 0)
	if err != nil || c == nil {
		t.Fatalf("Claim = %v, %v", c, err)
	}
	if c.DraftID != 17 || c.Kind != "reply" || c.MessageExternalID != "echo-1" || c.Body != "Thanks, noon works." ||
		!c.ApprovedAt.Equal(time.Date(2000, 1, 2, 3, 4, 5, 0, time.UTC)) {
		t.Fatalf("claim = %+v", *c)
	}
}

func TestResultBody(t *testing.T) {
	f := mustertest.New()
	defer f.Close()
	f.Approve(`{"draft_id":17,"kind":"reply","message_external_id":"echo-1","body":"x","approved_at":"2000-01-02T03:04:05Z"}`)
	c := client(t, f.URL)
	if _, err := c.Claim(context.Background(), 0); err != nil {
		t.Fatal(err)
	}
	ack, err := c.Result(context.Background(), 17, muster.Result{OK: true, ExternalRef: "echo-17"})
	if err != nil || ack.DraftID != 17 || ack.Status != "sent" {
		t.Fatalf("Result = %+v, %v", ack, err)
	}
	_, results, _, _ := f.Snapshot()
	if len(results) != 1 || results[0].DraftID != 17 || results[0].Body != `{"ok":true,"external_ref":"echo-17"}` {
		t.Fatalf("results = %+v", results)
	}
	// A result for a draft this plugin never claimed is muster's 404.
	_, err = c.Result(context.Background(), 18, muster.Result{OK: true})
	if e := muster.AsError(err); e == nil || e.Status != 404 || e.Kind != muster.KindOff {
		t.Fatalf("unclaimed result err = %v", err)
	}
}

func TestErrorsAreTyped(t *testing.T) {
	f := mustertest.New()
	defer f.Close()
	c := client(t, f.URL)
	f.RateLimit = 1
	_, err := c.PostMessage(context.Background(), muster.Message{ExternalID: "a", Body: "b"})
	e := muster.AsError(err)
	if e == nil || e.Status != 429 || e.Kind != muster.KindRateLimited || e.RetryAfter != 7*time.Second || !e.Retryable() {
		t.Fatalf("429 err = %#v", e)
	}
	f.Revoked = true
	_, err = c.Self(context.Background())
	if e := muster.AsError(err); e == nil || e.Status != 401 || e.Kind != muster.KindAuth || e.Retryable() {
		t.Fatalf("401 err = %#v", e)
	}
	srv := httptest.NewServer(http.NotFoundHandler())
	defer srv.Close()
	_, err = client(t, srv.URL).Self(context.Background())
	if e := muster.AsError(err); e == nil || e.Kind != muster.KindOff {
		t.Fatalf("404 err = %#v", e)
	}
}

func TestKindForStatus(t *testing.T) {
	want := map[int]muster.Kind{400: "bad_request", 401: "auth", 403: "capability", 404: "not_found",
		409: "conflict", 413: "bad_request", 429: "rate_limited", 500: "server", 503: "server", 302: "protocol"}
	for s, k := range want {
		if got := muster.KindForStatus(s); got != k {
			t.Errorf("KindForStatus(%d) = %s, want %s", s, got, k)
		}
	}
}
