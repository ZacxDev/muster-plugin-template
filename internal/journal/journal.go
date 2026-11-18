// Package journal records, durably, what this plugin did with each draft, so
// a draft is sent at most once even across a crash or restart
// (docs/plugins.md, "At most once": record the draft_id before sending, never
// send the same draft_id twice, post the result again if unsure).
//
// It is an append-only JSON-lines file; every record is fsynced before the
// call returns, and the directory is fsynced when the journal is opened.
//
// It grows without bound: up to three short lines (about 60 bytes each) per
// draft, never compacted, and read whole at start. That is a few megabytes
// after tens of thousands of drafts. Entries are never dropped on purpose: a
// draft id missing from the journal is one this plugin would send again if it
// were ever handed it. To shrink it, stop the plugin and keep only the last
// line for each draft_id.
package journal

import (
	"bufio"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sync"
)

// State is the furthest a draft got.
type State string

const (
	Claimed  State = "claimed"  // recorded BEFORE sending: from here it is never sent again
	Sent     State = "sent"     // the send finished; the result may not be reported yet
	Reported State = "reported" // muster recorded the result
)

var rank = map[State]int{Claimed: 1, Sent: 2, Reported: 3}

type record struct {
	DraftID     int64  `json:"draft_id"`
	State       State  `json:"state"`
	ExternalRef string `json:"external_ref,omitempty"`
}

// Entry is one draft's furthest state.
type Entry struct {
	State       State
	ExternalRef string
}

// Journal is safe for concurrent use.
type Journal struct {
	mu      sync.Mutex
	f       *os.File
	entries map[int64]Entry
}

// Open reads (or creates) dir/journal.jsonl.
func Open(dir string) (*Journal, error) {
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, err
	}
	p := filepath.Join(dir, "journal.jsonl")
	f, err := os.OpenFile(p, os.O_CREATE|os.O_RDWR|os.O_APPEND, 0o600)
	if err != nil {
		return nil, err
	}
	// fsync the directory too, so a journal file just created survives a
	// crash (the per-record fsync covers its contents, not its name).
	if err := syncDir(dir); err != nil {
		f.Close()
		return nil, err
	}
	j := &Journal{f: f, entries: map[int64]Entry{}}
	sc := bufio.NewScanner(f)
	for line := 1; sc.Scan(); line++ {
		var r record
		if err := json.Unmarshal(sc.Bytes(), &r); err != nil || rank[r.State] == 0 || r.DraftID <= 0 {
			f.Close()
			return nil, fmt.Errorf("journal %s line %d is unreadable; fix or move it before starting", p, line)
		}
		j.apply(r)
	}
	if err := sc.Err(); err != nil {
		f.Close()
		return nil, err
	}
	return j, nil
}

func syncDir(dir string) error {
	d, err := os.Open(dir)
	if err != nil {
		return err
	}
	defer d.Close()
	return d.Sync()
}

func (j *Journal) apply(r record) {
	cur := j.entries[r.DraftID]
	if rank[r.State] > rank[cur.State] {
		ref := r.ExternalRef
		if ref == "" {
			ref = cur.ExternalRef
		}
		j.entries[r.DraftID] = Entry{State: r.State, ExternalRef: ref}
	}
}

// Get returns a draft's entry, and whether it has one.
func (j *Journal) Get(id int64) (Entry, bool) {
	j.mu.Lock()
	defer j.mu.Unlock()
	e, ok := j.entries[id]
	return e, ok
}

// Record appends a record and fsyncs it.
func (j *Journal) Record(id int64, s State, externalRef string) error {
	j.mu.Lock()
	defer j.mu.Unlock()
	b, err := json.Marshal(record{DraftID: id, State: s, ExternalRef: externalRef})
	if err != nil {
		return err
	}
	if _, err := j.f.Write(append(b, '\n')); err != nil {
		return err
	}
	if err := j.f.Sync(); err != nil {
		return err
	}
	j.apply(record{DraftID: id, State: s, ExternalRef: externalRef})
	return nil
}

// Unreported lists drafts that were sent but whose result muster has not
// recorded yet: their result is posted again on start.
func (j *Journal) Unreported() map[int64]Entry {
	j.mu.Lock()
	defer j.mu.Unlock()
	out := map[int64]Entry{}
	for id, e := range j.entries {
		if e.State == Sent {
			out[id] = e
		}
	}
	return out
}

// Close closes the file.
func (j *Journal) Close() error { return j.f.Close() }
