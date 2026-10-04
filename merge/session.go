package merge

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"

	"savegame/savecore"
)

// Side names for adoption decisions.
const (
	SideLeft  = "left"
	SideRight = "right"
)

// Conflict describes one object the two candidates changed
// incompatibly inside the same region.
type Conflict struct {
	Key  string
	Kind string // "modified-both", "delete-vs-modify", "move-vs-edit", ...
}

// RegionStatus is the operator-facing view of one region.
type RegionStatus struct {
	Name      string
	Left      []Change // what the left candidate changed here
	Right     []Change // what the right candidate changed here
	Conflict  bool
	Conflicts []Conflict
	Decision  string // "", SideLeft or SideRight
}

// PendingItem records content that has not been adopted into the
// merged save. A finalized save is only complete when this is empty.
type PendingItem struct {
	Region   string
	Conflict bool
	Left     []Change
	Right    []Change
}

// RetainedItem records an object kept solely because adopted content
// still references it (e.g. a move whose destination region was not
// adopted). It is listed explicitly so nothing is silently kept.
type RetainedItem struct {
	Key    string
	Source string
	Reason string
}

// Report is the explicit record attached to every finalized save:
// what was adopted, what is still pending, and what was retained for
// reference integrity. The merged save never claims to be complete
// unless Pending is empty.
type Report struct {
	OutPath   string
	Complete  bool
	Decisions map[string]string
	Pending   []PendingItem
	Retained  []RetainedItem
}

// ReportPath is where Finalize writes the machine-readable report.
func (r *Report) ReportPath() string { return r.OutPath + ".report.json" }

// Session is a resumable three-way merge. The three input saves are
// only ever read; all operator progress lives in Decisions and can
// be persisted with SaveState.
type Session struct {
	reg       *savecore.Registry
	layout    Layout
	BasePath  string
	LeftPath  string
	RightPath string

	Base  *Snapshot
	Left  *Snapshot
	Right *Snapshot

	// Decisions maps region name to the adopted side. Undecided
	// regions fall back to the base content.
	Decisions map[string]string
}

// NewSession loads the three saves and starts a merge.
func NewSession(reg *savecore.Registry, layout Layout, basePath, leftPath, rightPath string) (*Session, error) {
	s := &Session{
		reg:       reg,
		layout:    layout,
		BasePath:  basePath,
		LeftPath:  leftPath,
		RightPath: rightPath,
		Decisions: map[string]string{},
	}
	var err error
	if s.Base, err = LoadSnapshot(reg, basePath, layout); err != nil {
		return nil, err
	}
	if s.Left, err = LoadSnapshot(reg, leftPath, layout); err != nil {
		return nil, err
	}
	if s.Right, err = LoadSnapshot(reg, rightPath, layout); err != nil {
		return nil, err
	}
	return s, nil
}

func (s *Session) snapshots() map[string]*Snapshot {
	return map[string]*Snapshot{"base": s.Base, SideLeft: s.Left, SideRight: s.Right}
}

// regionNames returns the union of region names across all three
// saves, in a stable order (base order first, then new ones).
func (s *Session) regionNames() []string {
	seen := map[string]bool{}
	var out []string
	for _, sn := range []*Snapshot{s.Base, s.Left, s.Right} {
		for _, name := range sn.Regions {
			if !seen[name] {
				seen[name] = true
				out = append(out, name)
			}
		}
	}
	return out
}

func changesByRegion(changes []Change) map[string][]Change {
	out := map[string][]Change{}
	for _, c := range changes {
		for _, r := range c.regionsTouched() {
			out[r] = append(out[r], c)
		}
	}
	return out
}

// Regions computes the current per-region status, including
// conflicts between the two candidates.
func (s *Session) Regions() []RegionStatus {
	left := changesByRegion(Diff(s.Base, s.Left))
	right := changesByRegion(Diff(s.Base, s.Right))
	var out []RegionStatus
	for _, name := range s.regionNames() {
		l, r := left[name], right[name]
		st := RegionStatus{
			Name:     name,
			Left:     l,
			Right:    r,
			Decision: s.Decisions[name],
		}
		if len(l) > 0 && len(r) > 0 && !changeSetEqual(l, r) {
			st.Conflict = true
			st.Conflicts = classifyConflicts(l, r)
		}
		out = append(out, st)
	}
	return out
}

// classifyConflicts pairs up changes both sides made to the same
// object and names the shape of each disagreement.
func classifyConflicts(left, right []Change) []Conflict {
	byKeyL := map[string][]Change{}
	byKeyR := map[string][]Change{}
	for _, c := range left {
		byKeyL[c.Key] = append(byKeyL[c.Key], c)
	}
	for _, c := range right {
		byKeyR[c.Key] = append(byKeyR[c.Key], c)
	}
	var out []Conflict
	for key, ls := range byKeyL {
		rs := byKeyR[key]
		if len(rs) == 0 {
			continue
		}
		for _, l := range ls {
			for _, r := range rs {
				if l.fingerprint() == r.fingerprint() {
					continue
				}
				out = append(out, Conflict{Key: key, Kind: conflictKind(l.Kind, r.Kind)})
			}
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Key < out[j].Key })
	return out
}

func conflictKind(a, b ChangeKind) string {
	if a == b {
		switch a {
		case Modified:
			return "modified-both"
		case Moved:
			return "moved-to-different-regions"
		case Added:
			return "added-both"
		}
		return "diverged"
	}
	if a == Deleted || b == Deleted {
		return "delete-vs-modify"
	}
	if a == Moved || b == Moved {
		return "move-vs-edit"
	}
	return "diverged"
}

// Adopt records the decision to take a region's content from one
// candidate. It applies to both clean and conflicted regions;
// adopting a side is how the operator resolves a conflict.
func (s *Session) Adopt(region, side string) error {
	if side != SideLeft && side != SideRight {
		return fmt.Errorf("merge: side must be %q or %q, got %q", SideLeft, SideRight, side)
	}
	found := false
	for _, name := range s.regionNames() {
		if name == region {
			found = true
			break
		}
	}
	if !found {
		return fmt.Errorf("merge: unknown region %q", region)
	}
	s.Decisions[region] = side
	return nil
}

// Undo withdraws the adoption of one region. Reference decisions are
// derived from region decisions at finalize time, so undoing a
// region also retracts exactly the retentions it caused and nothing
// else.
func (s *Session) Undo(region string) {
	delete(s.Decisions, region)
}

// AutoAdopt adopts every non-conflicting region from whichever side
// changed it (from the left when both sides made identical changes).
// It returns the names of the regions it decided.
func (s *Session) AutoAdopt() []string {
	var adopted []string
	for _, st := range s.Regions() {
		if st.Decision != "" || st.Conflict {
			continue
		}
		switch {
		case len(st.Left) > 0:
			s.Decisions[st.Name] = SideLeft
		case len(st.Right) > 0:
			s.Decisions[st.Name] = SideRight
		default:
			continue
		}
		adopted = append(adopted, st.Name)
	}
	return adopted
}

// Pending lists the content that would not be adopted if the merge
// finalized now: every undecided region that either candidate
// changed.
func (s *Session) Pending() []PendingItem {
	var out []PendingItem
	for _, st := range s.Regions() {
		if st.Decision != "" {
			continue
		}
		if len(st.Left) == 0 && len(st.Right) == 0 {
			continue
		}
		out = append(out, PendingItem{
			Region:   st.Name,
			Conflict: st.Conflict,
			Left:     st.Left,
			Right:    st.Right,
		})
	}
	return out
}

// stateFile is the on-disk form of a merge in progress.
type stateFile struct {
	Base      string            `json:"base"`
	Left      string            `json:"left"`
	Right     string            `json:"right"`
	Decisions map[string]string `json:"decisions"`
}

// SaveState persists the session so an interrupted merge can be
// resumed with LoadSession. The write is atomic (temp file +
// rename); the three input saves are never touched.
func (s *Session) SaveState(path string) error {
	raw, err := json.MarshalIndent(stateFile{
		Base:      s.BasePath,
		Left:      s.LeftPath,
		Right:     s.RightPath,
		Decisions: s.Decisions,
	}, "", "  ")
	if err != nil {
		return err
	}
	tmp, err := os.CreateTemp(filepath.Dir(path), filepath.Base(path)+".writing-*")
	if err != nil {
		return err
	}
	tmpName := tmp.Name()
	if _, err := tmp.Write(raw); err != nil {
		tmp.Close()
		os.Remove(tmpName)
		return err
	}
	if err := tmp.Sync(); err != nil {
		tmp.Close()
		os.Remove(tmpName)
		return err
	}
	if err := tmp.Close(); err != nil {
		os.Remove(tmpName)
		return err
	}
	return os.Rename(tmpName, path)
}

// LoadSession resumes a merge from its state file, re-reading the
// original candidate saves from their recorded paths.
func LoadSession(reg *savecore.Registry, layout Layout, statePath string) (*Session, error) {
	raw, err := os.ReadFile(statePath)
	if err != nil {
		return nil, fmt.Errorf("merge: read state: %w", err)
	}
	var st stateFile
	if err := json.Unmarshal(raw, &st); err != nil {
		return nil, fmt.Errorf("merge: malformed state file: %w", err)
	}
	s, err := NewSession(reg, layout, st.Base, st.Left, st.Right)
	if err != nil {
		return nil, err
	}
	for region, side := range st.Decisions {
		if err := s.Adopt(region, side); err != nil {
			return nil, fmt.Errorf("merge: state file: %w", err)
		}
	}
	return s, nil
}
