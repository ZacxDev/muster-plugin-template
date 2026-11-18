// Package mustertest is a fake muster for tests: the four /plugin/v1 routes
// this template calls, with muster's answers (docs/plugins.md, "The plugin
// author's contract"), recording every request it was sent.
package mustertest

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync"
)

// Token is the plugin token the fake accepts.
const Token = "mpl_test_token_0123456789"

// Result is one POST /plugin/v1/drafts/{id}/result the fake received.
type Result struct {
	DraftID int64
	Body    string // the raw JSON body, exactly as sent
}

// Fake is a fake muster server.
type Fake struct {
	*httptest.Server

	mu sync.Mutex
	// Messages are the raw bodies of every POST /plugin/v1/messages.
	Messages []string
	// Results are every result call, in order.
	Results []Result
	// Auth is every Authorization header seen.
	Auth []string
	// Claims counts claim calls.
	Claims int
	// RateLimit answers the next n message posts 429 with Retry-After: 7.
	RateLimit int
	// Conflict answers the next n message posts 409 and stores nothing, as
	// muster does when the officer a bound plugin delivers to is not defined.
	Conflict int
	// Revoked makes every route answer 401, as muster does for a revoked token.
	Revoked bool
	// Off makes every route answer 404, as muster does with plugins off.
	Off bool

	stored   map[string]int64 // external_id → message id
	queue    []string         // approved drafts' claim bodies, served in order
	claimed  map[int64]bool
	nextID   int64
	settled  map[int64]bool // result recorded (sent or failed)
	settleOK map[int64]bool
}

// New starts a fake muster; Close it when done.
func New() *Fake {
	f := &Fake{stored: map[string]int64{}, claimed: map[int64]bool{}, settled: map[int64]bool{}, settleOK: map[int64]bool{}}
	mux := http.NewServeMux()
	mux.HandleFunc("GET /plugin/v1/self", f.gate(func(w http.ResponseWriter, r *http.Request) {
		write(w, 200, `{"plugin_id":3,"plugin_name":"example-echo","owner_id":1,"owner_login":"operator","owner_kind":"person","capabilities":["messages"],"deliver_to":null}`)
	}))
	mux.HandleFunc("POST /plugin/v1/messages", f.gate(f.message))
	mux.HandleFunc("POST /plugin/v1/drafts/claim", f.gate(f.claim))
	mux.HandleFunc("POST /plugin/v1/drafts/{id}/result", f.gate(f.result))
	f.Server = httptest.NewServer(mux)
	return f
}

// Approve queues one approved draft for the next claim. claimJSON is the
// claim body muster answers, e.g.
// {"draft_id":17,"kind":"reply","message_external_id":"echo-1","body":"Hi","approved_at":"2000-01-02T03:04:05Z"}.
func (f *Fake) Approve(claimJSON string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.queue = append(f.queue, claimJSON)
}

// MarkClaimed records draft id as claimed by this plugin, as if an earlier
// process had claimed it (its result is then answered 200, not 404).
func (f *Fake) MarkClaimed(id int64) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.claimed[id] = true
}

// Snapshot returns copies of what the fake recorded.
func (f *Fake) Snapshot() (messages []string, results []Result, auth []string, claims int) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.Messages...), append([]Result(nil), f.Results...), append([]string(nil), f.Auth...), f.Claims
}

func write(w http.ResponseWriter, code int, body string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	io.WriteString(w, body)
}

func (f *Fake) gate(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		f.mu.Lock()
		f.Auth = append(f.Auth, r.Header.Get("Authorization"))
		off, revoked := f.Off, f.Revoked
		f.mu.Unlock()
		if off {
			http.NotFound(w, r)
			return
		}
		if revoked || r.Header.Get("Authorization") != "Bearer "+Token {
			write(w, 401, `{"error":"this route takes a plugin token (mpl_…), minted at /plugins"}`)
			return
		}
		next(w, r)
	}
}

func (f *Fake) message(w http.ResponseWriter, r *http.Request) {
	raw, _ := io.ReadAll(r.Body)
	f.mu.Lock()
	defer f.mu.Unlock()
	f.Messages = append(f.Messages, string(raw))
	if f.RateLimit > 0 {
		f.RateLimit--
		w.Header().Set("Retry-After", "7")
		write(w, 429, `{"error":"rate limited: 60 new messages in the last hour"}`)
		return
	}
	if f.Conflict > 0 {
		f.Conflict--
		write(w, 409, `{"error":"the officer this plugin delivers to is not defined for its owner (or the built-in officer store is off): nothing was stored; send it again once the officer is back"}`)
		return
	}
	var in struct {
		ExternalID string `json:"external_id"`
		Body       string `json:"body"`
	}
	if json.Unmarshal(raw, &in) != nil || in.ExternalID == "" || strings.ContainsAny(in.ExternalID, " \t\n") || in.Body == "" {
		write(w, 400, `{"error":"external_id and body are required"}`)
		return
	}
	if id, ok := f.stored[in.ExternalID]; ok {
		write(w, 200, `{"id":`+strconv.FormatInt(id, 10)+`,"status":"queued"}`)
		return
	}
	f.nextID++
	f.stored[in.ExternalID] = f.nextID
	write(w, 202, `{"id":`+strconv.FormatInt(f.nextID, 10)+`,"status":"queued"}`)
}

func (f *Fake) claim(w http.ResponseWriter, r *http.Request) {
	raw, _ := io.ReadAll(r.Body)
	var in struct {
		WaitSeconds *int `json:"wait_seconds"`
	}
	if len(raw) > 0 && json.Unmarshal(raw, &in) != nil {
		write(w, 400, `{"error":"the body is {\"wait_seconds\": 0-25}"}`)
		return
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	f.Claims++
	if len(f.queue) == 0 {
		w.WriteHeader(http.StatusNoContent)
		return
	}
	body := f.queue[0]
	f.queue = f.queue[1:]
	var c struct {
		DraftID int64 `json:"draft_id"`
	}
	_ = json.Unmarshal([]byte(body), &c)
	f.claimed[c.DraftID] = true
	write(w, 200, body)
}

func (f *Fake) result(w http.ResponseWriter, r *http.Request) {
	id, _ := strconv.ParseInt(r.PathValue("id"), 10, 64)
	raw, _ := io.ReadAll(r.Body)
	f.mu.Lock()
	defer f.mu.Unlock()
	f.Results = append(f.Results, Result{DraftID: id, Body: string(raw)})
	var in struct {
		OK *bool `json:"ok"`
	}
	if json.Unmarshal(raw, &in) != nil || in.OK == nil {
		write(w, 400, `{"error":"the body is {\"ok\": true|false, \"external_ref\", \"detail\"}"}`)
		return
	}
	if !f.claimed[id] {
		write(w, 404, `{"error":"no such draft claimed by this plugin"}`)
		return
	}
	status := "sent"
	if !*in.OK {
		status = "failed"
	}
	if f.settled[id] && f.settleOK[id] != *in.OK {
		write(w, 409, `{"error":"the draft already settled with the other result"}`)
		return
	}
	f.settled[id], f.settleOK[id] = true, *in.OK
	write(w, 200, `{"draft_id":`+strconv.FormatInt(id, 10)+`,"status":"`+status+`"}`)
}
