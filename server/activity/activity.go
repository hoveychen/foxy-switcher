// Package activity is the daemon's structured event log. Every "interesting"
// thing the daemon does — credential injection, token refresh, usage poll,
// account CRUD, errors — flows through a single Bus that persists to SQLite
// and broadcasts to in-process subscribers (the upcoming SSE endpoint, the
// TUI's eventual live tail, etc.).
//
// Design notes:
//
//   - The bus is fire-and-forget. Callers never see a return value; failures
//     to persist are logged but not propagated. Activity is observability,
//     not business logic.
//   - In-memory ring of last RingCapacity events backs the GET /api/activity
//     fast path so we don't query SQLite on every poll.
//   - SQLite is the source of truth across daemon restarts. We trim the table
//     to RingCapacity rows after each insert so it never grows unboundedly.
//   - Severity is a coarse three-tier (info/warn/error) so the UI's "Errors"
//     filter chip is a single column lookup.
package activity

import (
	"database/sql"
	"encoding/json"
	"fmt"
	"log"
	"strings"
	"sync"
	"time"
)

// RingCapacity is the upper bound on both the in-memory ring and the
// persisted activity table. PRD §5.3 calls for "最多 1000 条".
const RingCapacity = 1000

// Severity is the coarse log level the UI uses to colour a row. Three buckets
// keep the filter chip set tiny ("All / Switches / Refreshes / Errors").
type Severity string

const (
	SeverityInfo  Severity = "info"
	SeverityWarn  Severity = "warn"
	SeverityError Severity = "error"
)

// Common event types. The const block doubles as the canonical taxonomy used
// by the front-end's filter chips — adding a new emitter without listing it
// here will work, but breaks discoverability.
const (
	TypeAccountAdded    = "account.added"
	TypeAccountDeleted  = "account.deleted"
	TypeAccountPaused   = "account.paused"
	TypeAccountResumed  = "account.resumed"
	TypeCredInjected    = "cred.injected"
	TypeCredRestored    = "cred.restored"
	TypeCredFailed      = "cred.failed"
	TypeTokenRefreshed  = "token.refreshed"
	TypeUsagePolled     = "usage.polled"
	TypeCooldownEntered = "cooldown.entered"
	TypeCooldownCleared = "cooldown.cleared"
	TypeDaemonStarted   = "daemon.started"
	TypeDaemonStopped   = "daemon.stopped"
	TypeErrorRefresh    = "error.refresh"
	TypeErrorUsage      = "error.usage"
)

// Event is the wire / storage shape. Payload is an opaque JSON blob the UI
// renders as a code box when the user expands a row — keep it small (we
// store it inline in SQLite TEXT).
type Event struct {
	ID        int64           `json:"id"`
	Timestamp int64           `json:"timestamp"` // unix milliseconds
	Type      string          `json:"type"`
	Severity  Severity        `json:"severity"`
	AccountID int64           `json:"account_id,omitempty"`
	Message   string          `json:"message"`
	Payload   json.RawMessage `json:"payload,omitempty"`
}

// Bus is the singleton event hub. Construct one per daemon via NewBus.
//
// Thread-safety: every public method is safe for concurrent callers. Fast
// readers (List) take a short read lock on the ring; subscribe/unsubscribe
// take a short write lock on the subscriber map; Emit takes both, briefly,
// in addition to one SQLite INSERT.
type Bus struct {
	db     *sql.DB
	logger *log.Logger
	clock  func() time.Time

	mu      sync.RWMutex
	ring    []Event              // newest at end; len ≤ RingCapacity
	subs    map[int]chan<- Event // subscriber id → bounded channel
	nextSub int
}

// NewBus opens / migrates the activity table inside an existing *sql.DB and
// hydrates the in-memory ring from the most recent rows. The bus shares the
// caller's database — pass store.Store.DB() so activity lives next to
// accounts in state.db.
func NewBus(db *sql.DB, logger *log.Logger) (*Bus, error) {
	if logger == nil {
		logger = log.Default()
	}
	if _, err := db.Exec(activitySchema); err != nil {
		return nil, fmt.Errorf("activity schema: %w", err)
	}
	if _, err := db.Exec(activityIndex); err != nil {
		return nil, fmt.Errorf("activity index: %w", err)
	}
	b := &Bus{
		db:     db,
		logger: logger,
		clock:  time.Now,
		subs:   make(map[int]chan<- Event),
	}
	if err := b.hydrate(); err != nil {
		return nil, fmt.Errorf("hydrate activity ring: %w", err)
	}
	return b, nil
}

const activitySchema = `
CREATE TABLE IF NOT EXISTS activity (
  id         INTEGER PRIMARY KEY AUTOINCREMENT,
  timestamp  INTEGER NOT NULL,
  type       TEXT    NOT NULL,
  severity   TEXT    NOT NULL,
  account_id INTEGER NOT NULL DEFAULT 0,
  message    TEXT    NOT NULL,
  payload    TEXT    NOT NULL DEFAULT ''
);
`

const activityIndex = `
CREATE INDEX IF NOT EXISTS activity_ts ON activity (id DESC);
CREATE INDEX IF NOT EXISTS activity_type_ts ON activity (type, id DESC);
`

func (b *Bus) hydrate() error {
	rows, err := b.db.Query(`
SELECT id, timestamp, type, severity, account_id, message, payload
  FROM activity
 ORDER BY id DESC
 LIMIT ?`, RingCapacity)
	if err != nil {
		return err
	}
	defer rows.Close()

	var newest []Event
	for rows.Next() {
		ev, err := scanRow(rows)
		if err != nil {
			return err
		}
		newest = append(newest, ev)
	}
	if err := rows.Err(); err != nil {
		return err
	}
	// Reverse so the ring is oldest→newest, matching Emit's append order.
	for i, j := 0, len(newest)-1; i < j; i, j = i+1, j-1 {
		newest[i], newest[j] = newest[j], newest[i]
	}
	b.ring = newest
	return nil
}

// scanner is the subset of *sql.Rows / *sql.Row that scanRow needs. Lets us
// share one decoder between hydrate (rows) and any future single-row reads.
type scanner interface {
	Scan(dest ...any) error
}

func scanRow(s scanner) (Event, error) {
	var ev Event
	var payload string
	if err := s.Scan(&ev.ID, &ev.Timestamp, &ev.Type, &ev.Severity,
		&ev.AccountID, &ev.Message, &payload); err != nil {
		return Event{}, err
	}
	if payload != "" {
		ev.Payload = json.RawMessage(payload)
	}
	return ev, nil
}

// Emit persists an event and broadcasts it to live subscribers. The caller
// supplies Type / Severity / AccountID / Message / Payload; ID and Timestamp
// are filled in here so callers can't accidentally lie about either.
//
// Errors are logged, not returned. The caller's job is to do the work that
// generated the event; observability shouldn't be in their failure path.
func (b *Bus) Emit(ev Event) {
	if b == nil {
		return
	}
	if ev.Severity == "" {
		ev.Severity = SeverityInfo
	}
	ev.Timestamp = b.clock().UnixMilli()

	payload := ""
	if len(ev.Payload) > 0 {
		payload = string(ev.Payload)
	}

	res, err := b.db.Exec(`
INSERT INTO activity (timestamp, type, severity, account_id, message, payload)
VALUES (?, ?, ?, ?, ?, ?)`,
		ev.Timestamp, ev.Type, string(ev.Severity), ev.AccountID, ev.Message, payload)
	if err != nil {
		b.logger.Printf("[activity] insert: %v", err)
		return
	}
	id, err := res.LastInsertId()
	if err != nil {
		b.logger.Printf("[activity] last-insert-id: %v", err)
		return
	}
	ev.ID = id

	// Trim the table to RingCapacity. Cheap because activity_ts indexes id DESC.
	if _, err := b.db.Exec(`
DELETE FROM activity
 WHERE id <= (SELECT id FROM activity ORDER BY id DESC LIMIT 1 OFFSET ?)`,
		RingCapacity); err != nil {
		b.logger.Printf("[activity] trim: %v", err)
	}

	b.mu.Lock()
	b.ring = append(b.ring, ev)
	if len(b.ring) > RingCapacity {
		b.ring = b.ring[len(b.ring)-RingCapacity:]
	}
	subs := make([]chan<- Event, 0, len(b.subs))
	for _, ch := range b.subs {
		subs = append(subs, ch)
	}
	b.mu.Unlock()

	for _, ch := range subs {
		// Non-blocking send: a slow subscriber must not stall every emitter.
		// Dropped events are recoverable via List / GET /api/activity.
		select {
		case ch <- ev:
		default:
		}
	}
}

// EmitInfo / EmitWarn / EmitError are sugar for the most common pattern:
// type + message, optionally tagged to an account. Use Emit when you need
// a structured payload.
func (b *Bus) EmitInfo(typ string, accountID int64, message string) {
	b.Emit(Event{Type: typ, Severity: SeverityInfo, AccountID: accountID, Message: message})
}

func (b *Bus) EmitWarn(typ string, accountID int64, message string) {
	b.Emit(Event{Type: typ, Severity: SeverityWarn, AccountID: accountID, Message: message})
}

func (b *Bus) EmitError(typ string, accountID int64, message string) {
	b.Emit(Event{Type: typ, Severity: SeverityError, AccountID: accountID, Message: message})
}

// Filter narrows what List returns. Empty fields disable that constraint.
type Filter struct {
	// Limit caps the number of events returned. ≤0 means "use a sensible
	// default" (currently 200). Hard upper bound is RingCapacity.
	Limit int
	// SinceID returns only events strictly newer than this id. Used by the
	// front-end's incremental polling — pass the highest id you've seen.
	SinceID int64
	// Types is an optional whitelist; an event matches if its Type is in
	// this set OR if Types is empty. The PRD's filter chips ("Switches",
	// "Refreshes", "Errors") map to small concrete type sets defined in
	// the front-end.
	Types []string
	// Severity, if non-empty, restricts to that severity (used by the
	// "Errors" filter).
	Severity Severity
}

// List returns events matching the filter, newest first. The fast path reads
// from the in-memory ring; only when the caller needs older events than the
// ring holds (or when the ring is cold after restart) does it touch SQLite.
func (b *Bus) List(f Filter) []Event {
	if b == nil {
		return nil
	}
	limit := f.Limit
	if limit <= 0 {
		limit = 200
	}
	if limit > RingCapacity {
		limit = RingCapacity
	}

	b.mu.RLock()
	snapshot := make([]Event, len(b.ring))
	copy(snapshot, b.ring)
	b.mu.RUnlock()

	out := make([]Event, 0, limit)
	for i := len(snapshot) - 1; i >= 0; i-- {
		ev := snapshot[i]
		if f.SinceID > 0 && ev.ID <= f.SinceID {
			continue
		}
		if f.Severity != "" && ev.Severity != f.Severity {
			continue
		}
		if len(f.Types) > 0 && !typeMatches(ev.Type, f.Types) {
			continue
		}
		out = append(out, ev)
		if len(out) >= limit {
			break
		}
	}
	return out
}

func typeMatches(typ string, allowed []string) bool {
	for _, a := range allowed {
		if a == typ {
			return true
		}
		// Wildcard suffix support: "error.*" matches "error.refresh" etc.
		if strings.HasSuffix(a, ".*") && strings.HasPrefix(typ, strings.TrimSuffix(a, "*")) {
			return true
		}
	}
	return false
}

// Subscribe registers ch to receive every emitted event. The channel must be
// buffered — Emit's send is non-blocking, so an unbuffered or full channel
// silently drops events. Returns an opaque id the caller passes to
// Unsubscribe on shutdown.
func (b *Bus) Subscribe(ch chan<- Event) int {
	b.mu.Lock()
	defer b.mu.Unlock()
	id := b.nextSub
	b.nextSub++
	b.subs[id] = ch
	return id
}

func (b *Bus) Unsubscribe(id int) {
	b.mu.Lock()
	defer b.mu.Unlock()
	delete(b.subs, id)
}
