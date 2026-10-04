package savecore

import (
	"encoding/json"
	"fmt"
	"os"
	"sort"
)

// MergeSession is a resumable, region-oriented merge. The operator
// adopts clean regions first, then resolves the remaining conflicts
// object by object. Every decision is persisted immediately, so an
// interrupted merge resumes exactly where it stopped; the candidate
// files are never touched. Finalize always writes a save, but the
// embedded report states precisely what was not adopted.

// PendingObject is content the official save does not include.
type PendingObject struct {
	StableID string `json:"stableID"`
	Type     string `json:"type"`
	Region   string `json:"region"`
	Reason   string `json:"reason"` // conflict kind or "not adopted"
	Kept     string `json:"kept"`   // "base" or "excluded"
}

// PendingReference records a reference whose target would have
// vanished; the target was rescued (from the base save, or from the
// candidate that introduced it when it is branch-new) so the merged
// save has neither duplicates nor dangling targets.
type PendingReference struct {
	From    string `json:"from"`
	To      string `json:"to"`
	ToType  string `json:"toType"`
	Rescued string `json:"rescued"` // "base", "A" or "B"
}

// MergeReport states exactly what a finalized merge did and left out.
type MergeReport struct {
	Complete          bool               `json:"complete"`
	AdoptedRegions    []string           `json:"adoptedRegions"`
	PendingObjects    []PendingObject    `json:"pendingObjects,omitempty"`
	PendingReferences []PendingReference `json:"pendingReferences,omitempty"`
}

// MergeSession holds the merge state for one (base, A, B) -> out
// combination.
type MergeSession struct {
	reg            *Registry
	base, a, b     *candidate
	out            *Store
	diff           *Diff
	adoptedRegions map[string]bool
	decisions      map[string]Side
	statePath      string
}

type mergeStateFile struct {
	AdoptedRegions []string        `json:"adoptedRegions"`
	Decisions      map[string]Side `json:"decisions"`
}

// MergeStatePath is where the resumable merge state for an output
// save is kept.
func MergeStatePath(outPath string) string { return outPath + ".merge.json" }

// NewMergeSession starts a fresh merge of A and B (both forked from
// base) into out. If a previous merge state exists for out it is
// replaced; use ResumeMergeSession to continue an interrupted merge.
func NewMergeSession(reg *Registry, baseStore, storeA, storeB, out *Store) (*MergeSession, error) {
	base, a, b, err := loadAll(reg, baseStore, storeA, storeB)
	if err != nil {
		return nil, err
	}
	m := &MergeSession{
		reg: reg, base: base, a: a, b: b, out: out,
		adoptedRegions: map[string]bool{},
		decisions:      map[string]Side{},
		statePath:      MergeStatePath(out.Path),
	}
	m.diff = computeDiff(base, a, b)
	return m, m.persist()
}

// ResumeMergeSession reopens an interrupted merge, restoring every
// adoption and resolution recorded before the interruption.
func ResumeMergeSession(reg *Registry, baseStore, storeA, storeB, out *Store) (*MergeSession, error) {
	m, err := NewMergeSession(reg, baseStore, storeA, storeB, out)
	if err != nil {
		return nil, err
	}
	raw, err := os.ReadFile(m.statePath)
	if err != nil {
		return nil, fmt.Errorf("savecore: no interrupted merge to resume: %w", err)
	}
	var sf mergeStateFile
	if err := json.Unmarshal(raw, &sf); err != nil {
		return nil, fmt.Errorf("savecore: malformed merge state: %w", err)
	}
	for _, r := range sf.AdoptedRegions {
		m.adoptedRegions[r] = true
	}
	for sid, side := range sf.Decisions {
		m.decisions[sid] = side
	}
	return m, nil
}

func (m *MergeSession) persist() error {
	sf := mergeStateFile{Decisions: m.decisions}
	for r := range m.adoptedRegions {
		sf.AdoptedRegions = append(sf.AdoptedRegions, r)
	}
	sort.Strings(sf.AdoptedRegions)
	raw, err := json.MarshalIndent(sf, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(m.statePath, raw, 0o644)
}

// Diff returns the full comparison the session is working from.
func (m *MergeSession) Diff() *Diff { return m.diff }

// CleanRegions lists regions that can be adopted immediately: every
// changed object in them is conflict-free (or already resolved).
func (m *MergeSession) CleanRegions() []string {
	var out []string
	for region, objs := range m.diff.Regions {
		adoptable := true
		for _, od := range objs {
			if m.conflictOpen(od) {
				adoptable = false
				break
			}
		}
		if adoptable {
			out = append(out, region)
		}
	}
	sort.Strings(out)
	return out
}

// conflictOpen reports whether a diff row still blocks adoption.
func (m *MergeSession) conflictOpen(od *ObjectDiff) bool {
	return od.Conflict != "" && m.decisions[od.StableID] == ""
}

// AdoptRegion accepts the automatic three-way resolution for every
// object in a region. It refuses regions that still contain
// conflicts; those objects need Resolve first.
func (m *MergeSession) AdoptRegion(region string) error {
	objs, ok := m.diff.Regions[region]
	if !ok {
		return fmt.Errorf("savecore: unknown region %q", region)
	}
	for _, od := range objs {
		if m.conflictOpen(od) {
			return fmt.Errorf("savecore: region %q has unresolved conflict on %s (%s)",
				region, od.StableID, od.Conflict)
		}
	}
	m.adoptedRegions[region] = true
	return m.persist()
}

// UndoRegion withdraws one region adoption and every object decision
// made inside that region. Reference consequences are recomputed at
// the next Finalize, so undoing an adoption also retracts the
// reference decisions that depended on it.
func (m *MergeSession) UndoRegion(region string) error {
	if !m.adoptedRegions[region] {
		return fmt.Errorf("savecore: region %q was not adopted", region)
	}
	delete(m.adoptedRegions, region)
	for _, od := range m.diff.Regions[region] {
		delete(m.decisions, od.StableID)
	}
	return m.persist()
}

// Resolve records the operator's choice for one conflicting object.
func (m *MergeSession) Resolve(stableID string, side Side) error {
	od := m.diff.byID[stableID]
	if od == nil {
		return fmt.Errorf("savecore: unknown object %q", stableID)
	}
	if side != SideA && side != SideB {
		return fmt.Errorf("savecore: side must be %q or %q", SideA, SideB)
	}
	m.decisions[stableID] = side
	return m.persist()
}

// Pending lists changed objects the merge has not decided yet:
// neither adopted via their region nor resolved by hand.
func (m *MergeSession) Pending() []*ObjectDiff {
	var out []*ObjectDiff
	for _, od := range m.diff.Objects {
		if od.KindA == Unchanged && od.KindB == Unchanged {
			continue
		}
		if m.decisions[od.StableID] != "" || m.adoptedRegions[od.Region] {
			continue
		}
		out = append(out, od)
	}
	return out
}

// chosenState is the winning version of one object.
type chosenState struct {
	state  *ObjectState // nil: object is deleted in the merged save
	source *candidate   // candidate the state (and its refs) came from
	side   string       // "A", "B", "base"
}

// resolve picks the winning state for one object.
func (m *MergeSession) resolve(od *ObjectDiff) (chosenState, bool) {
	if side, ok := m.decisions[od.StableID]; ok {
		return m.fromSide(od.StableID, side), true
	}
	if m.adoptedRegions[od.Region] && od.Conflict == "" {
		return m.autoResolve(od), true
	}
	return chosenState{}, false
}

func (m *MergeSession) fromSide(sid string, side Side) chosenState {
	if side == SideA {
		return chosenState{state: m.a.states[sid], source: m.a, side: "A"}
	}
	return chosenState{state: m.b.states[sid], source: m.b, side: "B"}
}

// autoResolve applies plain three-way rules to a conflict-free row.
func (m *MergeSession) autoResolve(od *ObjectDiff) chosenState {
	sid := od.StableID
	as, bs := m.a.states[sid], m.b.states[sid]
	switch {
	case od.KindA == Unchanged:
		return chosenState{state: bs, source: m.b, side: "B"}
	case od.KindB == Unchanged:
		return chosenState{state: as, source: m.a, side: "A"}
	case as == nil && bs == nil:
		return chosenState{side: "A"} // deleted on both sides
	case as != nil && bs != nil && as.hash == bs.hash && as.Region != bs.Region:
		// Moved to different regions with identical content: keep A's.
		return chosenState{state: as, source: m.a, side: "A"}
	case as != nil && bs != nil && as.hash == bs.hash:
		return chosenState{state: as, source: m.a, side: "A"}
	case as != nil && bs != nil && od.KindA == Moved && od.KindB == Modified:
		// Compatible changes: B's content in A's region.
		merged := *bs
		merged.Region = as.Region
		return chosenState{state: &merged, source: m.b, side: "A+B"}
	case as != nil && bs != nil && od.KindA == Modified && od.KindB == Moved:
		merged := *as
		merged.Region = bs.Region
		return chosenState{state: &merged, source: m.a, side: "A+B"}
	default:
		return chosenState{state: as, source: m.a, side: "A"}
	}
}

// Finalize assembles the merged save and writes it to the output
// store. Undecided objects fall back to their base version (or stay
// excluded if they are new), and the report names every one of them:
// the official save never claims a complete merge it did not do.
func (m *MergeSession) Finalize() (*MergeReport, error) {
	report := &MergeReport{Complete: true}
	chosen := map[string]*chosenState{}

	sids := map[string]bool{}
	for sid := range m.base.states {
		sids[sid] = true
	}
	for sid := range m.a.states {
		sids[sid] = true
	}
	for sid := range m.b.states {
		sids[sid] = true
	}
	sorted := make([]string, 0, len(sids))
	for sid := range sids {
		sorted = append(sorted, sid)
	}
	sort.Strings(sorted)

	for _, sid := range sorted {
		od := m.diff.byID[sid]
		cs, decided := m.resolve(od)
		if !decided {
			// Not adopted: keep the base version, say so explicitly.
			report.Complete = false
			reason := od.Conflict
			if reason == "" {
				reason = "not adopted"
			}
			kept := "base"
			if m.base.states[sid] == nil {
				kept = "excluded"
			}
			report.PendingObjects = append(report.PendingObjects, PendingObject{
				StableID: sid, Type: od.Type, Region: od.Region, Reason: reason, Kept: kept,
			})
			if bs := m.base.states[sid]; bs != nil {
				chosen[sid] = &chosenState{state: bs, source: m.base, side: "base"}
			}
			continue
		}
		if cs.state != nil {
			c := cs
			chosen[sid] = &c
		}
	}

	// Reference integrity: every reference of every chosen object must
	// land on a chosen object. Missing targets are rescued from the
	// base and reported, never duplicated and never left dangling.
	for {
		missing := false
		for _, sid := range sorted {
			cs := chosen[sid]
			if cs == nil {
				continue
			}
			for _, target := range m.targetsOf(cs) {
				if chosen[target] != nil {
					continue
				}
				bs := m.base.states[target]
				rescued := "base"
				var rescueSrc *candidate = m.base
				if bs == nil {
					// Branch-new target with no base version to fall back
					// to: rescue the version from the candidate whose
					// chosen reference points at it, instead of silently
					// dropping the object. Prefer the reference's own side
					// so the graph stays internally consistent.
					var src *candidate
					switch cs.side {
					case "A":
						src = m.a
						rescued = "A"
					case "B":
						src = m.b
						rescued = "B"
					default:
						if m.a.states[target] != nil {
							src = m.a
							rescued = "A"
						} else {
							src = m.b
							rescued = "B"
						}
					}
					if src == nil {
						src = m.a
						rescued = "A"
					}
					bs = src.states[target]
					if bs == nil {
						src = m.b
						rescued = "B"
						bs = src.states[target]
					}
					if bs == nil {
						return nil, fmt.Errorf("savecore: reference %s -> %s has no rescue source", sid, target)
					}
					rescueSrc = src
				}
				chosen[target] = &chosenState{state: bs, source: rescueSrc, side: rescued}
				report.PendingReferences = append(report.PendingReferences, PendingReference{
					From: sid, To: target, ToType: bs.Type, Rescued: rescued,
				})
				report.Complete = false
				missing = true
			}
		}
		if !missing {
			break
		}
	}

	// Assemble the merged flat graph with fresh local node ids.
	newID := map[string]int{}
	for i, sid := range sorted {
		if chosen[sid] != nil {
			newID[sid] = i + 1
		}
	}
	merged := &FlatGraph{Nodes: map[int]*FlatNode{}, Extra: map[string]any{}}
	outManifest := newManifest()
	outManifest.Nodes = map[int]string{}
	for sid, id := range newID {
		cs := chosen[sid]
		merged.Nodes[id] = &FlatNode{
			ID:   id,
			Type: cs.state.Type,
			Ver:  cs.state.Ver,
			Data: m.remapRefs(cs, newID),
		}
		outManifest.Nodes[id] = sid
		outManifest.Objects[sid] = &ObjectMeta{Type: cs.state.Type, Region: cs.state.Region}
	}
	rootID, ok := newID[m.base.rootSID]
	if !ok {
		return nil, fmt.Errorf("savecore: merged graph lost its root object %s", m.base.rootSID)
	}
	merged.Root = rootID

	for r := range m.adoptedRegions {
		report.AdoptedRegions = append(report.AdoptedRegions, r)
	}
	sort.Strings(report.AdoptedRegions)
	// A branch-new object first listed as an unadopted exclusion may
	// have been pulled back in by the reference-rescue pass; keep one
	// consistent story instead of reporting it both excluded and kept.
	if len(report.PendingReferences) > 0 {
		rescued := map[string]bool{}
		for _, pr := range report.PendingReferences {
			rescued[pr.To] = true
		}
		kept := report.PendingObjects[:0]
		for _, po := range report.PendingObjects {
			if po.Kept == "excluded" && rescued[po.StableID] {
				continue
			}
			kept = append(kept, po)
		}
		report.PendingObjects = kept
	}
	sort.Slice(report.PendingObjects, func(i, j int) bool {
		return report.PendingObjects[i].StableID < report.PendingObjects[j].StableID
	})
	sort.Slice(report.PendingReferences, func(i, j int) bool {
		return report.PendingReferences[i].To < report.PendingReferences[j].To
	})
	merged.Extra["merge"] = report

	payload, err := merged.Unflatten()
	if err != nil {
		return nil, err
	}
	if err := m.out.SavePayload(payload); err != nil {
		return nil, err
	}
	if err := outManifest.save(ManifestPath(m.out.Path)); err != nil {
		return nil, err
	}
	if err := m.persist(); err != nil {
		return nil, err
	}
	return report, nil
}

// targetsOf lists the stable ids a chosen object references.
func (m *MergeSession) targetsOf(cs *chosenState) []string {
	return RefSIDs(cs.state.Data)
}

// remapRefs rewrites a chosen object's data so its references point
// at the merged graph's node ids.
func (m *MergeSession) remapRefs(cs *chosenState, newID map[string]int) map[string]any {
	out := map[string]any{}
	for k, v := range cs.state.Data {
		out[k] = remapValue(v, cs, newID)
	}
	return out
}

func remapValue(v any, cs *chosenState, newID map[string]int) any {
	switch t := v.(type) {
	case map[string]any:
		if ref, isRef := t["$ref"]; isRef {
			sid, _ := ref.(string)
			return map[string]any{"$ref": newID[sid]}
		}
		out := map[string]any{}
		for k, x := range t {
			out[k] = remapValue(x, cs, newID)
		}
		return out
	case []any:
		out := make([]any, len(t))
		for i, x := range t {
			out[i] = remapValue(x, cs, newID)
		}
		return out
	default:
		return v
	}
}
