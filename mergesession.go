package configmigrator

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"time"
)

// DecisionRecord is one entry in the append-only decision log. Undo marks
// the latest active record as revoked instead of deleting it, so the full
// history of human decisions, including revoked ones, is preserved.
type DecisionRecord struct {
	Resolution Resolution `json:"resolution"`
	Timestamp  time.Time  `json:"timestamp"`
	Revoked    bool       `json:"revoked,omitempty"`
	RevokedAt  *time.Time `json:"revoked_at,omitempty"`
}

// AdoptionRecord preserves one accepted merge together with the decisions
// that produced it, so a later audit can see exactly what was adopted and
// why. Records are never deleted; a candidate change marks them superseded.
type AdoptionRecord struct {
	Timestamp    time.Time    `json:"timestamp"`
	Merged       Document     `json:"merged"`
	Fingerprint  string       `json:"fingerprint"`
	Resolutions  []Resolution `json:"resolutions"`
	Superseded   bool         `json:"superseded,omitempty"`
	SupersededAt *time.Time   `json:"superseded_at,omitempty"`
}

// MergeSession is the durable state of one dual-candidate merge. The
// original source and both candidates are stored verbatim and never
// mutated; decisions accumulate in an append-only log.
type MergeSession struct {
	Source     Document         `json:"source"`
	A          Document         `json:"a"`
	B          Document         `json:"b"`
	Links      []OriginLink     `json:"links"`
	References []MergeReference `json:"references,omitempty"`
	Preview    MergePreview     `json:"preview"`
	Decisions  []DecisionRecord `json:"decisions,omitempty"`
	Adopted    []AdoptionRecord `json:"adoptions,omitempty"`
}

// NewMergeSession compares the candidates and starts a session. The
// documents are cloned, so later caller edits cannot corrupt the session.
func NewMergeSession(source, a, b Document, links []OriginLink, refs []MergeReference) (*MergeSession, error) {
	preview, err := CompareCandidates(source, a, b, links, refs)
	if err != nil {
		return nil, err
	}
	return &MergeSession{
		Source:     source.Clone(),
		A:          a.Clone(),
		B:          b.Clone(),
		Links:      append([]OriginLink(nil), links...),
		References: append([]MergeReference(nil), refs...),
		Preview:    preview,
	}, nil
}

// ActiveResolutions returns the decisions currently in effect, in the
// order they were recorded. Revoked decisions stay in the log but are not
// returned.
func (s *MergeSession) ActiveResolutions() []Resolution {
	var active []Resolution
	for _, record := range s.Decisions {
		if !record.Revoked {
			active = append(active, record.Resolution)
		}
	}
	return active
}

// Resolve records one reasoned decision, bound to the current item state.
// A new decision for an already-decided item supersedes the earlier one:
// the earlier record is revoked, not erased.
func (s *MergeSession) Resolve(itemID string, choice CandidateSide, reason string) error {
	item := s.Preview.item(itemID)
	if item == nil {
		return fmt.Errorf("%w: %q", ErrUnknownMergeItem, itemID)
	}
	if reason == "" {
		return fmt.Errorf("%w: decision for item %q has no reason", ErrMergeUnresolved, itemID)
	}
	for i := range s.Decisions {
		if !s.Decisions[i].Revoked && s.Decisions[i].Resolution.ItemID == itemID {
			now := timeNow().UTC()
			s.Decisions[i].Revoked = true
			s.Decisions[i].RevokedAt = &now
		}
	}
	s.Decisions = append(s.Decisions, DecisionRecord{
		Resolution: Resolution{
			ItemID:          itemID,
			Choice:          choice,
			Reason:          reason,
			ItemFingerprint: item.Fingerprint,
		},
		Timestamp: timeNow().UTC(),
	})
	return nil
}

// Undo revokes the most recent active decision. The record itself, the
// original candidates, and all earlier decisions are preserved.
func (s *MergeSession) Undo() (Resolution, bool) {
	for i := len(s.Decisions) - 1; i >= 0; i-- {
		if s.Decisions[i].Revoked {
			continue
		}
		now := timeNow().UTC()
		s.Decisions[i].Revoked = true
		s.Decisions[i].RevokedAt = &now
		return s.Decisions[i].Resolution, true
	}
	return Resolution{}, false
}

// Adopt produces the merged document from the active decisions and records
// the adoption. Unresolved items or incomplete and inconsistent references
// refuse adoption and record nothing.
func (s *MergeSession) Adopt() (Document, error) {
	resolutions := s.ActiveResolutions()
	merged, err := AdoptMerge(s.Source, s.Preview, resolutions)
	if err != nil {
		return nil, err
	}
	s.Adopted = append(s.Adopted, AdoptionRecord{
		Timestamp:   timeNow().UTC(),
		Merged:      merged.Clone(),
		Fingerprint: s.Preview.Fingerprint,
		Resolutions: append([]Resolution(nil), resolutions...),
	})
	return merged, nil
}

// Recheck recompares updated candidates and re-checks only the decisions
// whose items changed; all other decisions stay in effect. The previous
// preview and earlier adoptions remain in the session history.
func (s *MergeSession) Recheck(a, b Document) (RecheckReport, error) {
	next, report, err := RecheckResolutions(s.Source, a, b, s.Links, s.References, s.Preview, s.ActiveResolutions())
	if err != nil {
		return RecheckReport{}, err
	}
	s.A = a.Clone()
	s.B = b.Clone()
	s.Preview = next
	now := timeNow().UTC()
	for i := range s.Adopted {
		if !s.Adopted[i].Superseded {
			s.Adopted[i].Superseded = true
			s.Adopted[i].SupersededAt = &now
		}
	}
	return report, nil
}

// MergeStore persists merge sessions atomically in a single JSON file, so
// a reopened session exposes the original candidates, the full decision
// log, and every adoption exactly as saved.
type MergeStore struct {
	path string
}

// NewMergeStore creates a store located at path.
func NewMergeStore(path string) *MergeStore {
	return &MergeStore{path: path}
}

// Save writes the session to a temporary file and renames it over the
// previous state; an interrupted save leaves the earlier session intact.
func (s *MergeStore) Save(session *MergeSession) error {
	contents, err := json.MarshalIndent(session, "", "  ")
	if err != nil {
		return err
	}
	dir := filepath.Dir(s.path)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	temp, err := os.CreateTemp(dir, ".merge-session-*.tmp")
	if err != nil {
		return err
	}
	defer os.Remove(temp.Name())
	if _, err := temp.Write(append(contents, '\n')); err != nil {
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
	return os.Rename(temp.Name(), s.path)
}

// Load reopens a saved session. Before the first save it returns
// ErrNotFound.
func (s *MergeStore) Load() (*MergeSession, error) {
	contents, err := os.ReadFile(s.path)
	if errors.Is(err, os.ErrNotExist) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, err
	}
	var session MergeSession
	if err := json.Unmarshal(contents, &session); err != nil {
		return nil, fmt.Errorf("decode merge session: %w", err)
	}
	return &session, nil
}
