package savecore

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"
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
	From            string `json:"from"`
	To              string `json:"to"`
	ToType          string `json:"toType"`
	Rescued         string `json:"rescued"` // "base", "A" or "B"
	DependsOnRegion string `json:"dependsOnRegion,omitempty"`
}

// ReferenceDecision is the persisted form of a reference rescue used
// by a review checkpoint. DependsOnRegion identifies the adopted
// region whose chosen object introduced the requirement; it is empty
// for a consequence of an unresolved base fallback.
type ReferenceDecision = PendingReference

// MergeCheckpoint is a named, restorable review snapshot.
type MergeCheckpoint struct {
	Name               string              `json:"name"`
	CreatedAt          time.Time           `json:"createdAt"`
	AdoptedRegions     []string            `json:"adoptedRegions"`
	Decisions          map[string]Side     `json:"decisions"`
	PendingObjects     []PendingObject     `json:"pendingObjects"`
	ReferenceDecisions []ReferenceDecision `json:"referenceDecisions"`
}

// CheckpointDiff compares two named review snapshots. Slices use the
// from-checkpoint as Removed and the to-checkpoint as Added.
type CheckpointDiff struct {
	FromName                  string
	ToName                    string
	AddedAdoptedRegions       []string
	RemovedAdoptedRegions     []string
	AddedDecisions            map[string]Side
	RemovedDecisions          map[string]Side
	AddedPendingObjects       []PendingObject
	RemovedPendingObjects     []PendingObject
	AddedReferenceDecisions   []ReferenceDecision
	RemovedReferenceDecisions []ReferenceDecision
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
	reg                *Registry
	base, a, b         *candidate
	out                *Store
	diff               *Diff
	adoptedRegions     map[string]bool
	decisions          map[string]Side
	referenceDecisions []ReferenceDecision
	statePath          string
}

type mergeStateFile struct {
	AdoptedRegions     []string            `json:"adoptedRegions"`
	Decisions          map[string]Side     `json:"decisions"`
	ReferenceDecisions []ReferenceDecision `json:"referenceDecisions,omitempty"`
}

// MergeStatePath is where the resumable merge state for an output
// save is kept.
func MergeStatePath(outPath string) string { return outPath + ".merge.json" }

// MergeCheckpointPath is where one named review checkpoint is kept.
func MergeCheckpointPath(outPath, name string) string {
	return filepath.Join(outPath+".merge.checkpoints", name+".json")
}

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
	m.referenceDecisions = append(m.referenceDecisions, sf.ReferenceDecisions...)
	return m, nil
}

func (m *MergeSession) persist() error {
	sf := mergeStateFile{Decisions: m.decisions}
	for r := range m.adoptedRegions {
		sf.AdoptedRegions = append(sf.AdoptedRegions, r)
	}
	sort.Strings(sf.AdoptedRegions)
	sf.ReferenceDecisions = append(sf.ReferenceDecisions, m.referenceDecisions...)
	raw, err := json.MarshalIndent(sf, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(m.statePath, raw, 0o644)
}

func (m *MergeSession) refreshPlan() error {
	plan, err := m.buildPlan()
	if err != nil {
		return err
	}
	m.referenceDecisions = plan.report.PendingReferences
	return nil
}

// SaveCheckpoint stores a named review snapshot without modifying the
// base or either candidate.
func (m *MergeSession) SaveCheckpoint(name string) (*MergeCheckpoint, error) {
	if err := validateCheckpointName(name); err != nil {
		return nil, err
	}
	plan, err := m.buildPlan()
	if err != nil {
		return nil, err
	}
	m.referenceDecisions = append([]ReferenceDecision(nil), plan.report.PendingReferences...)
	cp := &MergeCheckpoint{
		Name:               name,
		CreatedAt:          time.Now().UTC(),
		Decisions:          map[string]Side{},
		ReferenceDecisions: append([]ReferenceDecision(nil), m.referenceDecisions...),
	}
	for region := range m.adoptedRegions {
		cp.AdoptedRegions = append(cp.AdoptedRegions, region)
	}
	sort.Strings(cp.AdoptedRegions)
	for sid, side := range m.decisions {
		cp.Decisions[sid] = side
	}
	cp.PendingObjects = append([]PendingObject(nil), plan.report.PendingObjects...)
	raw, err := json.MarshalIndent(cp, "", "  ")
	if err != nil {
		return nil, err
	}
	path := MergeCheckpointPath(m.out.Path, name)
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return nil, fmt.Errorf("savecore: create checkpoint dir: %w", err)
	}
	if err := os.WriteFile(path, raw, 0o644); err != nil {
		return nil, fmt.Errorf("savecore: write checkpoint: %w", err)
	}
	if err := m.persist(); err != nil {
		return nil, err
	}
	return cp, nil
}

// RestoreCheckpoint replaces the active session state with a named
// checkpoint. Only merge metadata and later output saves are changed;
// the original candidate files are never rewritten.
func (m *MergeSession) RestoreCheckpoint(name string) (*MergeCheckpoint, error) {
	cp, err := LoadMergeCheckpoint(m.out.Path, name)
	if err != nil {
		return nil, err
	}
	m.adoptedRegions = map[string]bool{}
	m.decisions = map[string]Side{}
	for _, region := range cp.AdoptedRegions {
		m.adoptedRegions[region] = true
	}
	for sid, side := range cp.Decisions {
		m.decisions[sid] = side
	}
	m.referenceDecisions = append([]ReferenceDecision(nil), cp.ReferenceDecisions...)
	return cp, m.persist()
}

// ResumeMergeSessionFromCheckpoint resumes from a named checkpoint.
func ResumeMergeSessionFromCheckpoint(reg *Registry, baseStore, storeA, storeB, out *Store, checkpoint string) (*MergeSession, error) {
	m, err := NewMergeSession(reg, baseStore, storeA, storeB, out)
	if err != nil {
		return nil, err
	}
	if _, err := m.RestoreCheckpoint(checkpoint); err != nil {
		return nil, err
	}
	return m, nil
}

// LoadMergeCheckpoint reads one named checkpoint.
func LoadMergeCheckpoint(outPath, name string) (*MergeCheckpoint, error) {
	if err := validateCheckpointName(name); err != nil {
		return nil, err
	}
	raw, err := os.ReadFile(MergeCheckpointPath(outPath, name))
	if err != nil {
		return nil, fmt.Errorf("savecore: no merge checkpoint %q: %w", name, err)
	}
	var cp MergeCheckpoint
	if err := json.Unmarshal(raw, &cp); err != nil {
		return nil, fmt.Errorf("savecore: malformed merge checkpoint %q: %w", name, err)
	}
	if cp.Decisions == nil {
		cp.Decisions = map[string]Side{}
	}
	return &cp, nil
}

// CompareMergeCheckpoints compares two checkpoints stored for outPath.
func CompareMergeCheckpoints(outPath, fromName, toName string) (*CheckpointDiff, error) {
	from, err := LoadMergeCheckpoint(outPath, fromName)
	if err != nil {
		return nil, err
	}
	to, err := LoadMergeCheckpoint(outPath, toName)
	if err != nil {
		return nil, err
	}
	return CompareCheckpointData(from, to), nil
}

// CompareCheckpointData compares checkpoint values in memory.
func CompareCheckpointData(from, to *MergeCheckpoint) *CheckpointDiff {
	d := &CheckpointDiff{FromName: from.Name, ToName: to.Name, AddedDecisions: map[string]Side{}, RemovedDecisions: map[string]Side{}}
	fromRegions := stringSet(from.AdoptedRegions)
	toRegions := stringSet(to.AdoptedRegions)
	d.RemovedAdoptedRegions = removedSortedStrings(fromRegions, toRegions)
	d.AddedAdoptedRegions = removedSortedStrings(toRegions, fromRegions)
	for sid, side := range from.Decisions {
		if to.Decisions[sid] != side {
			d.RemovedDecisions[sid] = side
		}
	}
	for sid, side := range to.Decisions {
		if from.Decisions[sid] != side {
			d.AddedDecisions[sid] = side
		}
	}
	d.RemovedPendingObjects = removedPending(from.PendingObjects, to.PendingObjects)
	d.AddedPendingObjects = removedPending(to.PendingObjects, from.PendingObjects)
	d.RemovedReferenceDecisions = removedRefs(from.ReferenceDecisions, to.ReferenceDecisions)
	d.AddedReferenceDecisions = removedRefs(to.ReferenceDecisions, from.ReferenceDecisions)
	return d
}

func validateCheckpointName(name string) error {
	if name == "" || name == "." || name == ".." || strings.ContainsAny(name, `/\`+"\x00") {
		return fmt.Errorf("savecore: invalid checkpoint name %q", name)
	}
	return nil
}

func stringSet(values []string) map[string]bool {
	out := map[string]bool{}
	for _, value := range values {
		out[value] = true
	}
	return out
}

func removedSortedStrings(a, b map[string]bool) []string {
	var out []string
	for value := range a {
		if !b[value] {
			out = append(out, value)
		}
	}
	sort.Strings(out)
	return out
}

func pendingKey(po PendingObject) string {
	return strings.Join([]string{po.StableID, po.Type, po.Region, po.Reason, po.Kept}, "\x00")
}

func removedPending(a, b []PendingObject) []PendingObject {
	seen := map[string]bool{}
	for _, po := range b {
		seen[pendingKey(po)] = true
	}
	var out []PendingObject
	for _, po := range a {
		if !seen[pendingKey(po)] {
			out = append(out, po)
		}
	}
	sort.Slice(out, func(i, j int) bool { return pendingKey(out[i]) < pendingKey(out[j]) })
	return out
}

func referenceKey(rd ReferenceDecision) string {
	return strings.Join([]string{rd.From, rd.To, rd.ToType, rd.Rescued, rd.DependsOnRegion}, "\x00")
}

func removedRefs(a, b []ReferenceDecision) []ReferenceDecision {
	seen := map[string]bool{}
	for _, rd := range b {
		seen[referenceKey(rd)] = true
	}
	var out []ReferenceDecision
	for _, rd := range a {
		if !seen[referenceKey(rd)] {
			out = append(out, rd)
		}
	}
	sort.Slice(out, func(i, j int) bool { return referenceKey(out[i]) < referenceKey(out[j]) })
	return out
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
	if err := m.refreshPlan(); err != nil {
		delete(m.adoptedRegions, region)
		return err
	}
	return m.persist()
}

// UndoRegion withdraws one region adoption and every object decision
// made inside that region. Reference consequences are recomputed at
// the next Finalize, so undoing an adoption also retracts the
// reference decisions that depended on it.
func (m *MergeSession) UndoRegion(region string) error {
	_, err := m.UndoRegionDetailed(region)
	return err
}

// UndoneRegion describes the effects of withdrawing one region.
type UndoneRegion struct {
	Region                        string
	RetractedObjectDecisions      []string
	InvalidatedReferenceDecisions []ReferenceDecision
}

// UndoRegionDetailed withdraws one region and reports the reference
// decisions that become invalid because that adopted region caused
// them. Adoptions and decisions in other regions remain intact.
func (m *MergeSession) UndoRegionDetailed(region string) (*UndoneRegion, error) {
	if !m.adoptedRegions[region] {
		return nil, fmt.Errorf("savecore: region %q was not adopted", region)
	}
	oldReferenceDecisions := append([]ReferenceDecision(nil), m.referenceDecisions...)
	delete(m.adoptedRegions, region)
	var retracted []string
	for _, od := range m.diff.Regions[region] {
		if _, ok := m.decisions[od.StableID]; ok {
			retracted = append(retracted, od.StableID)
		}
		delete(m.decisions, od.StableID)
	}
	sort.Strings(retracted)
	if err := m.refreshPlan(); err != nil {
		return nil, err
	}
	invalidated := removedRefs(oldReferenceDecisions, m.referenceDecisions)
	if err := m.persist(); err != nil {
		return nil, err
	}
	return &UndoneRegion{
		Region:                        region,
		RetractedObjectDecisions:      retracted,
		InvalidatedReferenceDecisions: invalidated,
	}, nil
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
	if err := m.refreshPlan(); err != nil {
		delete(m.decisions, stableID)
		return err
	}
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
	region string       // review region responsible for this state
}

type mergePlan struct {
	chosen map[string]*chosenState
	sorted []string
	report *MergeReport
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

func (m *MergeSession) buildPlan() (*mergePlan, error) {
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
		cs.region = od.Region
		if cs.state != nil {
			c := cs
			chosen[sid] = &c
		}
	}

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
				rescueSrc := m.base
				if bs == nil {
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
				chosen[target] = &chosenState{state: bs, source: rescueSrc, side: rescued, region: cs.region}
				report.PendingReferences = append(report.PendingReferences, PendingReference{
					From: sid, To: target, ToType: bs.Type, Rescued: rescued, DependsOnRegion: cs.region,
				})
				report.Complete = false
				missing = true
			}
		}
		if !missing {
			break
		}
	}

	for region := range m.adoptedRegions {
		report.AdoptedRegions = append(report.AdoptedRegions, region)
	}
	sort.Strings(report.AdoptedRegions)
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
		left, right := report.PendingReferences[i], report.PendingReferences[j]
		if left.To != right.To {
			return left.To < right.To
		}
		return left.From < right.From
	})
	return &mergePlan{chosen: chosen, sorted: sorted, report: report}, nil
}

// Finalize assembles the merged save and writes it to the output store.
func (m *MergeSession) Finalize() (*MergeReport, error) {
	plan, err := m.buildPlan()
	if err != nil {
		return nil, err
	}
	report := plan.report
	chosen := plan.chosen
	sorted := plan.sorted
	m.referenceDecisions = append([]ReferenceDecision(nil), report.PendingReferences...)

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
