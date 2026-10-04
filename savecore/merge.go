package savecore

import (
	"encoding/json"
	"fmt"
	"os"
	"sort"
)

// Three-way, region-grouped merge of two save candidates branched
// from one save point. Identity comes from branch manifests, so the
// diff tells apart genuinely new objects, objects moved to another
// region, and delete-vs-modify conflicts. Comparison runs on migrated
// (current-version) data whenever a migration path exists; objects
// that cannot be converted keep their original bytes and are compared
// and carried through verbatim.

// Side names one candidate in a merge.
type Side string

const (
	SideA Side = "A"
	SideB Side = "B"
)

// ChangeKind classifies what one candidate did to one object,
// relative to the shared base.
type ChangeKind string

const (
	Unchanged ChangeKind = "unchanged"
	Added     ChangeKind = "added"
	Deleted   ChangeKind = "deleted"
	Modified  ChangeKind = "modified"
	Moved     ChangeKind = "moved" // region changed, content identical
)

// ObjectState is one object's comparable meaning inside a candidate.
type ObjectState struct {
	Type   string
	Ver    int            // version of Data (current version unless Raw)
	Region string         // region recorded in the candidate's manifest
	Data   map[string]any // migrated data, or original data when Raw
	Raw    bool           // true when no migration path exists; original kept
	Shared bool           // referenced from more than one place
	hash   string
}

// ObjectDiff is the per-object comparison row.
type ObjectDiff struct {
	StableID string
	Type     string
	Region   string // grouping region: B's, else A's, else base's
	RegionA  string
	RegionB  string
	KindA    ChangeKind
	KindB    ChangeKind
	MovedA   bool // content also identical? no: content changed AND region changed
	MovedB   bool
	Shared   bool   // shared object in at least one candidate
	Conflict string // "", "add-add", "modify-modify", "delete-modify", "move-move"
}

// Diff is the full region-level comparison of two candidates.
type Diff struct {
	Objects  []*ObjectDiff
	byID     map[string]*ObjectDiff
	Regions  map[string][]*ObjectDiff
	Conflict []*ObjectDiff
}

// ByID returns the diff row for one stable id.
func (d *Diff) ByID(sid string) *ObjectDiff { return d.byID[sid] }

type candidate struct {
	graph    *FlatGraph
	manifest *Manifest
	states   map[string]*ObjectState
	rootSID  string
	side     Side
}

func (c *candidate) sideName() string { return string(c.side) }

func loadCandidate(reg *Registry, store *Store, side Side) (*candidate, error) {
	frame, err := os.ReadFile(store.Path)
	if err != nil {
		return nil, fmt.Errorf("savecore: read candidate %s: %w", store.Path, err)
	}
	payload, err := Unpack(frame)
	if err != nil {
		return nil, err
	}
	g, err := Flatten(payload)
	if err != nil {
		return nil, err
	}
	m, err := LoadManifest(ManifestPath(store.Path))
	if err != nil {
		return nil, fmt.Errorf("savecore: candidate %s has no identity manifest: %w", store.Path, err)
	}
	// Reference counts, to flag shared objects in the diff.
	refCount := map[int]int{}
	for _, n := range g.Nodes {
		for _, id := range RefIDs(n.Data) {
			refCount[id]++
		}
	}
	c := &candidate{graph: g, manifest: m, states: map[string]*ObjectState{}, side: side}
	for id, node := range g.Nodes {
		sid, ok := m.Nodes[id]
		if !ok {
			return nil, fmt.Errorf("savecore: candidate %s node $id:%d has no stable identity", store.Path, id)
		}
		meta := m.Objects[sid]
		st := &ObjectState{Type: node.Type, Ver: node.Ver, Shared: refCount[id] > 1}
		if meta != nil {
			st.Region = meta.Region
		}
		data := DeepCopyData(node.Data).(map[string]any)
		codec, terr := reg.codec(node.Type)
		switch {
		case terr != nil:
			st.Raw = true // unregistered type: keep original data
		case node.Ver != codec.Version:
			migrated, merr := reg.migrateTo(node.Type, node.Ver, DeepCopyData(data).(map[string]any))
			if merr != nil {
				st.Raw = true // no migration path: keep original data
			} else {
				data = migrated
				st.Ver = codec.Version
			}
		}
		st.Data = data
		c.states[sid] = st
	}
	// Canonicalize references to stable ids, so identical objects
	// hash identically in every candidate regardless of the local
	// node numbering each save happened to use.
	for _, st := range c.states {
		st.Data = canonicalizeRefs(st.Data, m).(map[string]any)
		st.hash = stateHash(st)
	}
	rootSID, ok := m.Nodes[g.Root]
	if !ok {
		return nil, fmt.Errorf("savecore: candidate %s root has no stable identity", store.Path)
	}
	c.rootSID = rootSID
	return c, nil
}

// canonicalizeRefs rewrites local {"$ref": nodeID} pointers as
// stable {"$ref": stableID} pointers throughout data.
func canonicalizeRefs(v any, m *Manifest) any {
	switch t := v.(type) {
	case map[string]any:
		if ref, isRef := t["$ref"]; isRef {
			var local int
			switch n := ref.(type) {
			case int:
				local = n
			case float64:
				local = int(n)
			}
			return map[string]any{"$ref": m.Nodes[local]}
		}
		out := make(map[string]any, len(t))
		for k, x := range t {
			out[k] = canonicalizeRefs(x, m)
		}
		return out
	case []any:
		out := make([]any, len(t))
		for i, x := range t {
			out[i] = canonicalizeRefs(x, m)
		}
		return out
	default:
		return v
	}
}

// RefSIDs returns the sorted stable ids referenced anywhere in
// canonicalized data.
func RefSIDs(v any) []string {
	seen := map[string]bool{}
	collectSIDs(v, seen)
	out := make([]string, 0, len(seen))
	for sid := range seen {
		out = append(out, sid)
	}
	sort.Strings(out)
	return out
}

func collectSIDs(v any, seen map[string]bool) {
	switch t := v.(type) {
	case map[string]any:
		if ref, isRef := t["$ref"]; isRef {
			if sid, ok := ref.(string); ok {
				seen[sid] = true
			}
			return
		}
		for _, x := range t {
			collectSIDs(x, seen)
		}
	case []any:
		for _, x := range t {
			collectSIDs(x, seen)
		}
	}
}

func stateHash(st *ObjectState) string {
	raw, _ := json.Marshal(st.Data) // map keys are sorted by encoding/json
	if st.Raw {
		return fmt.Sprintf("raw:%d:%s", st.Ver, raw)
	}
	return string(raw)
}

// DiffCandidates compares two branched saves against their common
// base and groups the result by region.
func DiffCandidates(reg *Registry, baseStore, storeA, storeB *Store) (*Diff, error) {
	base, a, b, err := loadAll(reg, baseStore, storeA, storeB)
	if err != nil {
		return nil, err
	}
	return computeDiff(base, a, b), nil
}

func loadAll(reg *Registry, baseStore, storeA, storeB *Store) (*candidate, *candidate, *candidate, error) {
	base, err := loadCandidate(reg, baseStore, Side("base"))
	if err != nil {
		return nil, nil, nil, err
	}
	a, err := loadCandidate(reg, storeA, SideA)
	if err != nil {
		return nil, nil, nil, err
	}
	b, err := loadCandidate(reg, storeB, SideB)
	if err != nil {
		return nil, nil, nil, err
	}
	return base, a, b, nil
}

func computeDiff(base, a, b *candidate) *Diff {
	d := &Diff{byID: map[string]*ObjectDiff{}, Regions: map[string][]*ObjectDiff{}}
	sids := map[string]bool{}
	for sid := range base.states {
		sids[sid] = true
	}
	for sid := range a.states {
		sids[sid] = true
	}
	for sid := range b.states {
		sids[sid] = true
	}
	sorted := make([]string, 0, len(sids))
	for sid := range sids {
		sorted = append(sorted, sid)
	}
	sort.Strings(sorted)
	for _, sid := range sorted {
		bs, as, bs2 := base.states[sid], a.states[sid], b.states[sid]
		od := &ObjectDiff{StableID: sid}
		od.KindA, od.MovedA = changeOf(bs, as)
		od.KindB, od.MovedB = changeOf(bs, bs2)
		od.Type = firstType(bs, as, bs2)
		od.RegionA, od.RegionB = regionOf(as), regionOf(bs2)
		od.Region = od.RegionB
		if od.Region == "" {
			od.Region = od.RegionA
		}
		if od.Region == "" {
			od.Region = regionOf(bs)
		}
		od.Shared = (as != nil && as.Shared) || (bs2 != nil && bs2.Shared)
		od.Conflict = conflictOf(bs, as, bs2, od.KindA, od.KindB)
		d.Objects = append(d.Objects, od)
		d.byID[sid] = od
		d.Regions[od.Region] = append(d.Regions[od.Region], od)
		if od.Conflict != "" {
			d.Conflict = append(d.Conflict, od)
		}
	}
	return d
}

func changeOf(base, side *ObjectState) (ChangeKind, bool) {
	switch {
	case base == nil && side == nil:
		return Unchanged, false
	case base == nil:
		return Added, false
	case side == nil:
		return Deleted, false
	case base.hash != side.hash:
		// Content changed; flag whether it also moved.
		return Modified, base.Region != side.Region
	case base.Region != side.Region:
		return Moved, false
	default:
		return Unchanged, false
	}
}

func conflictOf(base, a, b *ObjectState, ka, kb ChangeKind) string {
	if ka == Unchanged || kb == Unchanged {
		return ""
	}
	if ka == Deleted && kb == Deleted {
		return ""
	}
	if ka == Deleted || kb == Deleted {
		return "delete-modify"
	}
	if ka == Added && kb == Added {
		if a.hash != b.hash {
			return "add-add"
		}
		return ""
	}
	if a.Region != b.Region && (ka == Moved || kb == Moved || ka == Modified || kb == Modified) &&
		a.Region != baseRegion(base) && b.Region != baseRegion(base) {
		return "move-move"
	}
	if a.hash != b.hash {
		return "modify-modify"
	}
	return ""
}

func baseRegion(base *ObjectState) string {
	if base == nil {
		return ""
	}
	return base.Region
}

func firstType(states ...*ObjectState) string {
	for _, s := range states {
		if s != nil {
			return s.Type
		}
	}
	return ""
}

func regionOf(s *ObjectState) string {
	if s == nil {
		return ""
	}
	return s.Region
}
