package savecore

import (
	"encoding/json"
	"fmt"
	"os"
	"sort"
)

// Named review checkpoints for a merge session. A checkpoint
// snapshots the operator's review state — adopted regions, pending
// objects and reference decisions — under a name, persisted next to
// the merge state so a later session can reload it and compare two
// checkpoints. Restoring a checkpoint only rewrites the session's
// own state file; the candidate saves are never touched.

// Checkpoint is one named snapshot of a merge review.
type Checkpoint struct {
	Name string `json:"name"`
	// AdoptedRegions and Decisions are the operator inputs.
	AdoptedRegions []string        `json:"adoptedRegions"`
	Decisions      map[string]Side `json:"decisions,omitempty"`
	// PendingObjects and ReferenceDecisions are the computed
	// consequences of those inputs at snapshot time.
	PendingObjects     []PendingObject    `json:"pendingObjects,omitempty"`
	ReferenceDecisions []PendingReference `json:"referenceDecisions,omitempty"`
}

// DecisionChange is one object decision that differs between two
// checkpoints; an empty side means "undecided".
type DecisionChange struct {
	StableID string `json:"stableID"`
	From     Side   `json:"from,omitempty"`
	To       Side   `json:"to,omitempty"`
}

// CheckpointDiff is the comparison of two checkpoints, A -> B.
// ReferencesOnlyInA are reference decisions that lapsed between the
// two snapshots (e.g. because a region adoption was withdrawn).
type CheckpointDiff struct {
	A                 string             `json:"a"`
	B                 string             `json:"b"`
	RegionsOnlyInA    []string           `json:"regionsOnlyInA,omitempty"`
	RegionsOnlyInB    []string           `json:"regionsOnlyInB,omitempty"`
	DecisionsChanged  []DecisionChange   `json:"decisionsChanged,omitempty"`
	PendingOnlyInA    []PendingObject    `json:"pendingOnlyInA,omitempty"`
	PendingOnlyInB    []PendingObject    `json:"pendingOnlyInB,omitempty"`
	ReferencesOnlyInA []PendingReference `json:"referencesOnlyInA,omitempty"`
	ReferencesOnlyInB []PendingReference `json:"referencesOnlyInB,omitempty"`
}

type checkpointStore struct {
	Checkpoints map[string]*Checkpoint `json:"checkpoints"`
}

// CheckpointsPath is where named merge checkpoints are kept.
func CheckpointsPath(outPath string) string { return outPath + ".merge.checkpoints.json" }

func (m *MergeSession) loadCheckpoints() (*checkpointStore, error) {
	cs := &checkpointStore{Checkpoints: map[string]*Checkpoint{}}
	raw, err := os.ReadFile(CheckpointsPath(m.out.Path))
	if err != nil {
		if os.IsNotExist(err) {
			return cs, nil
		}
		return nil, fmt.Errorf("savecore: read checkpoints: %w", err)
	}
	if err := json.Unmarshal(raw, cs); err != nil {
		return nil, fmt.Errorf("savecore: malformed checkpoints file: %w", err)
	}
	if cs.Checkpoints == nil {
		cs.Checkpoints = map[string]*Checkpoint{}
	}
	return cs, nil
}

func (m *MergeSession) saveCheckpoints(cs *checkpointStore) error {
	raw, err := json.MarshalIndent(cs, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(CheckpointsPath(m.out.Path), raw, 0o644)
}

// SaveCheckpoint records the current review state under name,
// replacing any older checkpoint with the same name.
func (m *MergeSession) SaveCheckpoint(name string) (*Checkpoint, error) {
	if name == "" {
		return nil, fmt.Errorf("savecore: checkpoint name must not be empty")
	}
	_, _, report, err := m.plan()
	if err != nil {
		return nil, err
	}
	cp := &Checkpoint{
		Name:               name,
		Decisions:          map[string]Side{},
		PendingObjects:     report.PendingObjects,
		ReferenceDecisions: report.PendingReferences,
	}
	cp.AdoptedRegions = append(cp.AdoptedRegions, report.AdoptedRegions...)
	for sid, side := range m.decisions {
		cp.Decisions[sid] = side
	}
	cs, err := m.loadCheckpoints()
	if err != nil {
		return nil, err
	}
	cs.Checkpoints[name] = cp
	if err := m.saveCheckpoints(cs); err != nil {
		return nil, err
	}
	return cp, nil
}

// Checkpoint returns a previously saved checkpoint.
func (m *MergeSession) Checkpoint(name string) (*Checkpoint, error) {
	cs, err := m.loadCheckpoints()
	if err != nil {
		return nil, err
	}
	cp, ok := cs.Checkpoints[name]
	if !ok {
		return nil, fmt.Errorf("savecore: unknown checkpoint %q", name)
	}
	return cp, nil
}

// Checkpoints lists saved checkpoint names, sorted.
func (m *MergeSession) Checkpoints() ([]string, error) {
	cs, err := m.loadCheckpoints()
	if err != nil {
		return nil, err
	}
	names := make([]string, 0, len(cs.Checkpoints))
	for name := range cs.Checkpoints {
		names = append(names, name)
	}
	sort.Strings(names)
	return names, nil
}

// RestoreCheckpoint rolls the session's review state back to a saved
// checkpoint: adopted regions and object decisions become exactly
// what the checkpoint recorded. Only the session state file is
// rewritten; the candidate saves are never modified.
func (m *MergeSession) RestoreCheckpoint(name string) error {
	cp, err := m.Checkpoint(name)
	if err != nil {
		return err
	}
	m.adoptedRegions = map[string]bool{}
	for _, r := range cp.AdoptedRegions {
		m.adoptedRegions[r] = true
	}
	m.decisions = map[string]Side{}
	for sid, side := range cp.Decisions {
		m.decisions[sid] = side
	}
	return m.persist()
}

// DiffCheckpoints compares two saved checkpoints, A -> B. It reads
// the persisted checkpoint file, so it works in any later session
// opened on the same output save.
func (m *MergeSession) DiffCheckpoints(a, b string) (*CheckpointDiff, error) {
	ca, err := m.Checkpoint(a)
	if err != nil {
		return nil, err
	}
	cb, err := m.Checkpoint(b)
	if err != nil {
		return nil, err
	}
	d := &CheckpointDiff{A: a, B: b}
	inA, inB := map[string]bool{}, map[string]bool{}
	for _, r := range ca.AdoptedRegions {
		inA[r] = true
	}
	for _, r := range cb.AdoptedRegions {
		inB[r] = true
	}
	for r := range inA {
		if !inB[r] {
			d.RegionsOnlyInA = append(d.RegionsOnlyInA, r)
		}
	}
	for r := range inB {
		if !inA[r] {
			d.RegionsOnlyInB = append(d.RegionsOnlyInB, r)
		}
	}
	sort.Strings(d.RegionsOnlyInA)
	sort.Strings(d.RegionsOnlyInB)

	sids := map[string]bool{}
	for sid := range ca.Decisions {
		sids[sid] = true
	}
	for sid := range cb.Decisions {
		sids[sid] = true
	}
	for sid := range sids {
		if ca.Decisions[sid] != cb.Decisions[sid] {
			d.DecisionsChanged = append(d.DecisionsChanged, DecisionChange{
				StableID: sid, From: ca.Decisions[sid], To: cb.Decisions[sid],
			})
		}
	}
	sort.Slice(d.DecisionsChanged, func(i, j int) bool {
		return d.DecisionsChanged[i].StableID < d.DecisionsChanged[j].StableID
	})

	d.PendingOnlyInA, d.PendingOnlyInB = diffPending(ca.PendingObjects, cb.PendingObjects)
	d.ReferencesOnlyInA, d.ReferencesOnlyInB = diffReferences(ca.ReferenceDecisions, cb.ReferenceDecisions)
	return d, nil
}

func diffPending(a, b []PendingObject) (onlyA, onlyB []PendingObject) {
	inB := map[string]bool{}
	for _, po := range b {
		inB[po.StableID] = true
	}
	inA := map[string]bool{}
	for _, po := range a {
		inA[po.StableID] = true
	}
	for _, po := range a {
		if !inB[po.StableID] {
			onlyA = append(onlyA, po)
		}
	}
	for _, po := range b {
		if !inA[po.StableID] {
			onlyB = append(onlyB, po)
		}
	}
	return onlyA, onlyB
}

func diffReferences(a, b []PendingReference) (onlyA, onlyB []PendingReference) {
	key := func(pr PendingReference) string { return pr.From + "->" + pr.To }
	inB := map[string]bool{}
	for _, pr := range b {
		inB[key(pr)] = true
	}
	inA := map[string]bool{}
	for _, pr := range a {
		inA[key(pr)] = true
	}
	for _, pr := range a {
		if !inB[key(pr)] {
			onlyA = append(onlyA, pr)
		}
	}
	for _, pr := range b {
		if !inA[key(pr)] {
			onlyB = append(onlyB, pr)
		}
	}
	return onlyA, onlyB
}
