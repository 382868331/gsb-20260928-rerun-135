package configmigrator

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"time"
)

// MergeEvent is one immutable audit entry. Every entry keeps the exact two
// candidates at that moment and the full set of decisions, so reopening or
// undoing never loses the original candidates or a recorded reason.
type MergeEvent struct {
	Timestamp   time.Time    `json:"timestamp"`
	Kind        string       `json:"kind"`
	ItemID      string       `json:"item_id,omitempty"`
	Choice      string       `json:"choice,omitempty"`
	Reason      string       `json:"reason,omitempty"`
	CustomValue any          `json:"custom_value,omitempty"`
	Request     MergeRequest `json:"request"`
	Items       []MergeItem  `json:"items"`
	Refs        []MergeRef   `json:"refs,omitempty"`
}

// StoredMergeSession is the persisted reconciliation, including every human
// choice and the original candidates it was based on.
type StoredMergeSession struct {
	ID          string       `json:"id"`
	CreatedAt   time.Time    `json:"created_at"`
	UpdatedAt   time.Time    `json:"updated_at"`
	Request     MergeRequest `json:"request"`
	Refs        []MergeRef   `json:"refs"`
	Items       []MergeItem  `json:"items"`
	Fingerprint string       `json:"fingerprint"`
	Adopted     bool         `json:"adopted"`
	Official    Document     `json:"official,omitempty"`
	Events      []MergeEvent `json:"events"`
}

// MergeStore persists merge sessions into a directory with atomic writes.
// Unresolved progress, original candidates, reasons, reopenings, and undos
// are all retained; an adopted version is published as one official file.
type MergeStore struct {
	dir    string
	engine *Engine
}

// NewMergeStore creates a merge store rooted at dir.
func NewMergeStore(dir string) *MergeStore {
	return &MergeStore{dir: dir}
}

// BindEngine attaches the engine used to re-derive lineage when sessions
// are reopened after their candidates changed.
func (s *MergeStore) BindEngine(e *Engine) { s.engine = e }

func (s *MergeStore) path(id string) string {
	return filepath.Join(s.dir, "sessions", id+".json")
}

func (s *MergeStore) officialPath() string {
	return filepath.Join(s.dir, "official.json")
}

// Save persists the session, recording an event that keeps the current
// candidates and all item decisions. kind describes the action (open,
// resolve, update, reopen, undo, adopt).
func (s *MergeStore) Save(session *MergeSession, id, kind, itemID, choice, reason string, custom any) error {
	stored, err := s.loadOr(id)
	if err != nil {
		return err
	}
	now := timeNow().UTC()
	if stored.ID == "" {
		stored = StoredMergeSession{ID: id, CreatedAt: now}
	}
	stored.UpdatedAt = now
	stored.Request = cloneRequest(session.req)
	stored.Refs = append([]MergeRef(nil), session.refs...)
	stored.Items = append([]MergeItem(nil), session.items...)
	stored.Fingerprint = session.fingerprint
	stored.Adopted = session.adopted
	if session.adopted {
		stored.Official = session.buildDraft().Clone()
	}
	stored.Events = append(stored.Events, MergeEvent{
		Timestamp:   now,
		Kind:        kind,
		ItemID:      itemID,
		Choice:      choice,
		Reason:      reason,
		CustomValue: cloneValue(custom),
		Request:     cloneRequest(session.req),
		Items:       append([]MergeItem(nil), session.items...),
		Refs:        append([]MergeRef(nil), session.refs...),
	})
	return s.write(stored)
}

func cloneRequest(req MergeRequest) MergeRequest {
	return MergeRequest{
		Base: req.Base.Clone(),
		A: MergeCandidate{
			Document: req.A.Document.Clone(),
			From:     req.A.From,
			Path:     append([]string(nil), req.A.Path...),
			Labels:   append([]string(nil), req.A.Labels...),
		},
		B: MergeCandidate{
			Document: req.B.Document.Clone(),
			From:     req.B.From,
			Path:     append([]string(nil), req.B.Path...),
			Labels:   append([]string(nil), req.B.Labels...),
		},
	}
}

func (s *MergeStore) loadOr(id string) (StoredMergeSession, error) {
	stored, err := s.Load(id)
	if errors.Is(err, ErrNotFound) {
		return StoredMergeSession{}, nil
	}
	return stored, err
}

// Load reads one persisted session.
func (s *MergeStore) Load(id string) (StoredMergeSession, error) {
	contents, err := os.ReadFile(s.path(id))
	if errors.Is(err, os.ErrNotExist) {
		return StoredMergeSession{}, ErrNotFound
	}
	if err != nil {
		return StoredMergeSession{}, err
	}
	var stored StoredMergeSession
	if err := json.Unmarshal(contents, &stored); err != nil {
		return StoredMergeSession{}, fmt.Errorf("decode merge session: %w", err)
	}
	return stored, nil
}

// List returns every stored session ID in stable order.
func (s *MergeStore) List() ([]string, error) {
	entries, err := os.ReadDir(filepath.Join(s.dir, "sessions"))
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var ids []string
	for _, entry := range entries {
		if entry.IsDir() || filepath.Ext(entry.Name()) != ".json" {
			continue
		}
		ids = append(ids, entry.Name()[:len(entry.Name())-len(".json")])
	}
	sort.Strings(ids)
	return ids, nil
}

// Open restores a session as a working MergeSession. All stored decisions
// and reasons come back with it; the session is re-aligned against the
// stored candidates so lineage-based identity survives reopening.
func (s *MergeStore) Open(ctx context.Context, id string) (*MergeSession, StoredMergeSession, error) {
	stored, err := s.Load(id)
	if err != nil {
		return nil, StoredMergeSession{}, err
	}
	if s.engine == nil {
		return nil, StoredMergeSession{}, fmt.Errorf("%w: MergeStore has no bound engine", ErrInvalidStep)
	}
	session, err := s.engine.NewMergeSession(ctx, stored.Request, stored.Refs)
	if err != nil {
		return nil, StoredMergeSession{}, err
	}
	session.items = stored.Items
	session.fingerprint = stored.Fingerprint
	session.adopted = stored.Adopted
	session.relate()
	session.evaluateRefs()
	return session, stored, nil
}

// Events returns the complete audit trail for one session.
func (s *MergeStore) Events(id string) ([]MergeEvent, error) {
	stored, err := s.Load(id)
	if err != nil {
		return nil, err
	}
	return append([]MergeEvent(nil), stored.Events...), nil
}

// Undo reverts the session to its previous recorded state while keeping
// both states in the audit trail. The original two candidates and every
// earlier choice with its reason remain available.
func (s *MergeStore) Undo(ctx context.Context, id, reason string) (*MergeSession, error) {
	stored, err := s.Load(id)
	if err != nil {
		return nil, err
	}
	if len(stored.Events) == 0 {
		return nil, fmt.Errorf("%w: session %s has no recorded action to undo", ErrInvalidMergeDecision, id)
	}
	// Undo removes the effect of the most recent operator action. Adoption
	// and reopen markers are skipped, and the state before that action is
	// restored while every event remains in the trail.
	previousIndex := -1
	for i := len(stored.Events) - 1; i >= 0; i-- {
		kind := stored.Events[i].Kind
		if kind == "adopt" || kind == "reopen" {
			continue
		}
		previousIndex = i - 1
		break
	}
	if previousIndex < 0 {
		previousIndex = 0
	}
	previous := stored.Events[previousIndex]
	if s.engine == nil {
		return nil, fmt.Errorf("%w: MergeStore has no bound engine", ErrInvalidStep)
	}
	session, err := s.engine.NewMergeSession(ctx, previous.Request, previous.Refs)
	if err != nil {
		return nil, err
	}
	session.items = append([]MergeItem(nil), previous.Items...)
	session.relate()
	session.evaluateRefs()
	session.adopted = false
	if err := s.Save(session, id, "undo", "", "", reason, nil); err != nil {
		return nil, err
	}
	return session, nil
}

// Publish returns a Commit callback that writes the official merged
// document. The adoption is persisted together with the session.
func (s *MergeStore) Publish(id string) func(context.Context, MergeRelease) error {
	return func(_ context.Context, release MergeRelease) error {
		return s.writeOfficial(id, release)
	}
}

// AdoptHook returns a session hook that appends the adoption audit entry.
func (s *MergeStore) AdoptHook(id string) func(MergeRelease) error {
	return func(release MergeRelease) error {
		stored, err := s.Load(id)
		if err != nil {
			return err
		}
		now := timeNow().UTC()
		stored.UpdatedAt = now
		stored.Events = append(stored.Events, MergeEvent{
			Timestamp: now,
			Kind:      "adopt",
			Request:   stored.Request,
			Items:     stored.Items,
			Refs:      stored.Refs,
		})
		return s.write(stored)
	}
}

func (s *MergeStore) writeOfficial(id string, release MergeRelease) error {
	stored, err := s.Load(id)
	if err != nil {
		return err
	}
	stored.Adopted = true
	stored.Official = release.Document.Clone()
	stored.UpdatedAt = timeNow().UTC()
	if err := s.write(stored); err != nil {
		return err
	}
	payload := map[string]any{
		"document": release.Document,
		"session":  id,
		"preview":  release.Preview,
	}
	contents, err := json.MarshalIndent(payload, "", "  ")
	if err != nil {
		return err
	}
	if err := os.MkdirAll(s.dir, 0o755); err != nil {
		return err
	}
	temp, err := os.CreateTemp(s.dir, ".official-*.tmp")
	if err != nil {
		return err
	}
	defer os.Remove(temp.Name())
	if _, err := temp.Write(append(contents, byte('\n'))); err != nil {
		temp.Close()
		return err
	}
	if err := temp.Sync(); err != nil {
		temp.Close()
		return err
	}
	if err := temp.Close(); err != nil {
		return err
	}
	return os.Rename(temp.Name(), s.officialPath())
}

// Official reads the adopted, fully verified merged document.
func (s *MergeStore) Official() (Document, error) {
	contents, err := os.ReadFile(s.officialPath())
	if errors.Is(err, os.ErrNotExist) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, err
	}
	var payload struct {
		Document Document `json:"document"`
	}
	if err := json.Unmarshal(contents, &payload); err != nil {
		return nil, fmt.Errorf("decode official merge: %w", err)
	}
	return payload.Document, nil
}

func (s *MergeStore) write(stored StoredMergeSession) error {
	contents, err := json.MarshalIndent(stored, "", "  ")
	if err != nil {
		return err
	}
	dir := filepath.Join(s.dir, "sessions")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	temp, err := os.CreateTemp(dir, ".session-*.tmp")
	if err != nil {
		return err
	}
	defer os.Remove(temp.Name())
	if _, err := temp.Write(append(contents, byte('\n'))); err != nil {
		temp.Close()
		return err
	}
	if err := temp.Sync(); err != nil {
		temp.Close()
		return err
	}
	if err := temp.Close(); err != nil {
		return err
	}
	return os.Rename(temp.Name(), s.path(stored.ID))
}
