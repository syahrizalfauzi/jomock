// Package store holds the stub list and the request journal.
package store

import (
	"bytes"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"os"
	"strings"
	"sync"
	"time"

	"github.com/syahrizalfauzi/jomock/internal/matcher"
	"github.com/syahrizalfauzi/jomock/internal/stub"
)

const journalBodyLimit = 4096

// Entry is one observed request.
type Entry struct {
	Time       time.Time         `json:"time"`
	Protocol   string            `json:"protocol"` // "http" or "grpc"
	Method     string            `json:"method"`
	URL        string            `json:"url"`
	Headers    map[string]string `json:"headers"`
	Body       string            `json:"body"`
	StubID     string            `json:"stubId"`
	Status     int               `json:"status"` // HTTP status, or gRPC status code
	DurationMs int64             `json:"durationMs"`
}

// Store is a thread-safe, order-preserving stub list plus a fixed-size request journal.
// ponytail: the journal is a slice in memory and stubs are rewritten whole on every
// mutation. Fine at mock-server scale; move to an embedded DB if the stub count ever
// reaches the thousands or the journal needs to survive a restart.
type Store struct {
	mu      sync.RWMutex
	path    string
	stubs   []stub.Stub
	journal []Entry
	head    int
	size    int
}

// New returns an empty store persisting to path ("" disables persistence).
func New(path string, journalCap int) *Store {
	if journalCap < 1 {
		journalCap = 1
	}
	return &Store{
		path:    path,
		stubs:   []stub.Stub{},
		journal: make([]Entry, journalCap),
	}
}

// Load reads the stub file. A missing or empty file is not an error.
func (s *Store) Load() error {
	if s.path == "" {
		return nil
	}
	b, err := os.ReadFile(s.path)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	if len(bytes.TrimSpace(b)) == 0 {
		return nil
	}
	var stubs []stub.Stub
	if err := json.Unmarshal(b, &stubs); err != nil {
		return fmt.Errorf("parse %s: %w", s.path, err)
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	for i := range stubs {
		if stubs[i].ID == "" {
			stubs[i].ID = newID()
		}
	}
	s.stubs = stubs
	return nil
}

// List returns a copy of the stubs in match order.
func (s *Store) List() []stub.Stub {
	s.mu.RLock()
	defer s.mu.RUnlock()
	out := make([]stub.Stub, len(s.stubs))
	copy(out, s.stubs)
	return out
}

// Add validates, assigns an ID when missing, appends, and persists.
func (s *Store) Add(sb stub.Stub) (stub.Stub, error) {
	if err := sb.Validate(); err != nil {
		return stub.Stub{}, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if sb.ID == "" {
		sb.ID = newID()
	} else if s.index(sb.ID) >= 0 {
		return stub.Stub{}, fmt.Errorf("stub id %q already exists", sb.ID)
	}
	s.stubs = append(s.stubs, sb)
	return sb, s.save()
}

// Update replaces the stub with the given id.
func (s *Store) Update(id string, sb stub.Stub) (stub.Stub, bool, error) {
	if err := sb.Validate(); err != nil {
		return stub.Stub{}, false, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	i := s.index(id)
	if i < 0 {
		return stub.Stub{}, false, nil
	}
	sb.ID = id
	s.stubs[i] = sb
	return sb, true, s.save()
}

// Replace swaps the whole stub list in one step. Nothing is changed unless
// every incoming stub validates and ids are unique, so a bad import cannot
// leave the store half-loaded.
func (s *Store) Replace(stubs []stub.Stub) ([]stub.Stub, error) {
	next := make([]stub.Stub, len(stubs))
	seen := make(map[string]bool, len(stubs))
	for i, sb := range stubs {
		if err := sb.Validate(); err != nil {
			return nil, fmt.Errorf("stub %d: %w", i, err)
		}
		if sb.ID == "" {
			sb.ID = newID()
		} else if seen[sb.ID] {
			return nil, fmt.Errorf("stub %d: duplicate id %q", i, sb.ID)
		}
		seen[sb.ID] = true
		next[i] = sb
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.stubs = next
	return next, s.save()
}

// Delete removes the stub with the given id.
func (s *Store) Delete(id string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	i := s.index(id)
	if i < 0 {
		return false
	}
	s.stubs = append(s.stubs[:i], s.stubs[i+1:]...)
	_ = s.save()
	return true
}

// Move swaps a stub with its neighbour. At the ends it is a no-op.
func (s *Store) Move(id, direction string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	i := s.index(id)
	if i < 0 {
		return false
	}
	j := i - 1
	if direction == "down" {
		j = i + 1
	}
	if j < 0 || j >= len(s.stubs) {
		return true
	}
	s.stubs[i], s.stubs[j] = s.stubs[j], s.stubs[i]
	_ = s.save()
	return true
}

// Match finds the first stub matching the request and records a journal entry.
// It returns the matched stub (zero value when nothing matched) and a token to
// hand back to Complete once the response has actually been served.
func (s *Store) Match(req *http.Request, body []byte, protocol string) (stub.Stub, int) {
	s.mu.Lock()
	defer s.mu.Unlock()

	var matched stub.Stub
	for i := range s.stubs {
		// A stub only ever answers its own protocol, so an HTTP stub can never
		// swallow a gRPC call that happens to share a path.
		if s.stubs[i].Protocol() != protocol {
			continue
		}
		if matcher.Match(s.stubs[i].Request, req, body) {
			matched = s.stubs[i]
			break
		}
	}

	entry := Entry{
		Time:     time.Now(),
		Protocol: protocol,
		Method:   req.Method,
		URL:      req.URL.RequestURI(),
		Headers:  flatten(req.Header),
		Body:     truncate(string(body), journalBodyLimit),
		StubID:   matched.ID,
	}
	return matched, s.push(entry)
}

// Complete fills in the outcome of a journal entry created by Match.
func (s *Store) Complete(token, status int, dur time.Duration) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if token < 0 || token >= len(s.journal) {
		return
	}
	s.journal[token].Status = status
	s.journal[token].DurationMs = dur.Milliseconds()
}

// Journal returns up to limit entries, newest first.
func (s *Store) Journal(limit int) []Entry {
	s.mu.RLock()
	defer s.mu.RUnlock()
	if limit > s.size {
		limit = s.size
	}
	out := make([]Entry, 0, limit)
	for i := s.size - 1; i >= s.size-limit; i-- {
		out = append(out, s.journal[(s.head+i)%len(s.journal)])
	}
	return out
}

// Verify counts journal entries attributed to stubID and compares them to
// expected (when nil, "at least one" is the assertion).
func (s *Store) Verify(stubID string, expected *int) (int, bool) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	n := 0
	for i := 0; i < s.size; i++ {
		if s.journal[(s.head+i)%len(s.journal)].StubID == stubID {
			n++
		}
	}
	if expected == nil {
		return n, n >= 1
	}
	return n, n == *expected
}

// Save writes the stub file atomically.
func (s *Store) Save() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.save()
}

// save writes atomically. Callers must hold the lock.
func (s *Store) save() error {
	if s.path == "" {
		return nil
	}
	b, err := json.MarshalIndent(s.stubs, "", "  ")
	if err != nil {
		return err
	}
	tmp := s.path + ".tmp"
	if err := os.WriteFile(tmp, b, 0o644); err != nil {
		return err
	}
	return os.Rename(tmp, s.path)
}

// index returns the position of id, or -1. Callers must hold the lock.
func (s *Store) index(id string) int {
	for i := range s.stubs {
		if s.stubs[i].ID == id {
			return i
		}
	}
	return -1
}

// push stores entry, evicting the oldest once the ring is full. Callers must hold the lock.
func (s *Store) push(e Entry) int {
	if s.size < len(s.journal) {
		i := (s.head + s.size) % len(s.journal)
		s.journal[i] = e
		s.size++
		return i
	}
	i := s.head
	s.journal[i] = e
	s.head = (s.head + 1) % len(s.journal)
	return i
}

func newID() string {
	var b [8]byte
	_, _ = rand.Read(b[:])
	return hex.EncodeToString(b[:])
}

func flatten(h http.Header) map[string]string {
	out := make(map[string]string, len(h))
	for k, v := range h {
		out[k] = strings.Join(v, ", ")
	}
	return out
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "…(truncated)"
}
