// Package merge implements region-level three-way merges of savecore
// saves. A base savepoint and two candidate saves branched from it
// are compared region by region; the operator adopts non-conflicting
// regions first and resolves the rest by hand. Object identity is
// stable across saves (each object carries a uid field), so the diff
// can tell a genuinely new object apart from one that moved to
// another region, and a delete on one side from a modify on the
// other. The merged save keeps cross-region references as references
// to a single shared node: never duplicated, never dangling.
//
// Candidates may be written at different data versions. Comparison
// happens on migrated (current-version) data, i.e. on the explained
// meaning of each object; nodes that cannot be migrated keep their
// original raw payload, are compared raw, and are carried into the
// merged save untouched.
//
// A merge session is fully described by its state file; the three
// input saves are only ever read, so the original candidates are
// always recoverable and an interrupted merge can be resumed.
package merge

import (
	"encoding/json"
	"fmt"
	"os"

	"savegame/savecore"
)

// Layout describes how regions appear in the object graph, so the
// merge engine does not hardcode game types.
type Layout struct {
	RootType     string // type name of the save root, e.g. "world.World"
	RegionType   string // type name of region nodes, e.g. "world.Region"
	RegionsField string // root field listing the regions, e.g. "Regions"
	NameField    string // region field holding its name, e.g. "Name"
}

// Node is one object in a saved graph.
type Node struct {
	ID         int            // $id within its own file
	Type       string         // $type
	Ver        int            // $ver as saved
	Raw        map[string]any // data exactly as saved
	Canon      map[string]any // data migrated to the current version; nil if not migratable
	View       any            // canonOrRaw with references replaced by stable keys
	Key        string         // stable identity key, shared across saves
	Region     string         // owning region; "" for structural nodes
	Refs       []int          // ids of nodes this node's data points at
	Structural bool           // root and region nodes, not diffed as content
}

// canonOrRaw returns the data a comparison should look at: the
// explained (migrated) meaning when available, the original bytes
// otherwise.
func (n *Node) canonOrRaw() map[string]any {
	if n.Canon != nil {
		return n.Canon
	}
	return n.Raw
}

// Snapshot is one save file parsed into an identity-keyed node set.
type Snapshot struct {
	Nodes   map[int]*Node
	ByKey   map[string]*Node
	RootID  int
	Regions []string // region names in root order
}

// LoadSnapshot reads a save file without instantiating game objects.
func LoadSnapshot(reg *savecore.Registry, path string, layout Layout) (*Snapshot, error) {
	frame, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("merge: read %s: %w", path, err)
	}
	payload, err := savecore.Unpack(frame)
	if err != nil {
		return nil, err
	}
	root, err := savecore.UnpackEnvelope(payload)
	if err != nil {
		return nil, err
	}
	var decoded any
	if err := json.Unmarshal(root, &decoded); err != nil {
		return nil, fmt.Errorf("merge: malformed root node: %w", err)
	}
	sn := &Snapshot{Nodes: map[int]*Node{}, ByKey: map[string]*Node{}}
	rootNode, err := sn.walk(decoded, reg)
	if err != nil {
		return nil, err
	}
	sn.RootID = rootNode.ID
	rootNode.Structural = true
	rootNode.Key = "root|" + rootNode.Type
	if rootNode.Type != layout.RootType {
		return nil, fmt.Errorf("merge: root type %q, want %q", rootNode.Type, layout.RootType)
	}
	if err := sn.assignKeys(); err != nil {
		return nil, err
	}
	if err := sn.assignRegions(layout); err != nil {
		return nil, err
	}
	sn.buildViews()
	return sn, nil
}

// walk parses one nested node map (and recursively every node
// reachable from it) into the snapshot.
func (sn *Snapshot) walk(v any, reg *savecore.Registry) (*Node, error) {
	m, ok := v.(map[string]any)
	if !ok {
		return nil, fmt.Errorf("merge: expected node object, got %T", v)
	}
	idf, ok := m["$id"].(float64)
	if !ok {
		return nil, fmt.Errorf("merge: node missing $id")
	}
	id := int(idf)
	if n, dup := sn.Nodes[id]; dup {
		return n, nil
	}
	typ, _ := m["$type"].(string)
	verf, _ := m["$ver"].(float64)
	data, _ := m["data"].(map[string]any)
	if typ == "" || data == nil {
		return nil, fmt.Errorf("merge: node $id:%d missing $type or data", id)
	}
	n := &Node{ID: id, Type: typ, Ver: int(verf), Raw: data}
	sn.Nodes[id] = n

	var visit func(any) error
	visit = func(v any) error {
		switch t := v.(type) {
		case map[string]any:
			if ref, isRef := t["$ref"]; isRef {
				rf, ok := ref.(float64)
				if !ok {
					return fmt.Errorf("merge: node $id:%d has malformed $ref", id)
				}
				n.Refs = append(n.Refs, int(rf))
				return nil
			}
			if _, isNode := t["$id"]; isNode {
				child, err := sn.walk(t, reg)
				if err != nil {
					return err
				}
				n.Refs = append(n.Refs, child.ID)
				return nil
			}
			for _, e := range t {
				if err := visit(e); err != nil {
					return err
				}
			}
		case []any:
			for _, e := range t {
				if err := visit(e); err != nil {
					return err
				}
			}
		}
		return nil
	}
	for _, e := range data {
		if err := visit(e); err != nil {
			return nil, err
		}
	}
	if reg != nil {
		if canon, err := reg.MigrateData(typ, n.Ver, data); err == nil {
			n.Canon = canon
		}
	}
	return n, nil
}

// uidFields are the data fields that carry a stable identity, in
// priority order. Migrated data is inspected first, so a v1
// "Nickname" and a v2 "Name" still match.
var uidFields = []string{"UID", "ID", "Name", "Title", "Nickname"}

func (sn *Snapshot) assignKeys() error {
	for id, n := range sn.Nodes {
		if n.Key == "" {
			src := n.canonOrRaw()
			for _, f := range uidFields {
				if s, ok := src[f].(string); ok && s != "" {
					n.Key = n.Type + "|" + s
					break
				}
			}
			if n.Key == "" {
				// No identity field: fall back to file-local
				// identity. Such objects can still merge, they
				// just cannot be matched across saves.
				n.Key = fmt.Sprintf("%s#%d", n.Type, id)
			}
		}
		if prev, dup := sn.ByKey[n.Key]; dup {
			return fmt.Errorf("merge: duplicate object identity %q ($id:%d and $id:%d)", n.Key, prev.ID, id)
		}
		sn.ByKey[n.Key] = n
	}
	return nil
}

// buildViews computes each node's comparison view: its explained
// data with every embedded node or reference replaced by the
// target's stable key. Two saves agree on an object exactly when
// their views agree, regardless of file-local $id numbering.
func (sn *Snapshot) buildViews() {
	for _, n := range sn.Nodes {
		n.View = sn.viewOf(n.canonOrRaw())
	}
}

func (sn *Snapshot) viewOf(v any) any {
	switch t := v.(type) {
	case map[string]any:
		if id, err := refID(t); err == nil {
			if target, ok := sn.Nodes[id]; ok {
				return map[string]any{"@key": target.Key}
			}
		}
		out := make(map[string]any, len(t))
		for k, e := range t {
			out[k] = sn.viewOf(e)
		}
		return out
	case []any:
		out := make([]any, len(t))
		for i, e := range t {
			out[i] = sn.viewOf(e)
		}
		return out
	default:
		return v
	}
}

// assignRegions marks region nodes structural and gives every other
// node to the first region (in root order) that reaches it. Shared
// objects stay single nodes; regions after the first simply hold
// references to them.
func (sn *Snapshot) assignRegions(layout Layout) error {
	root := sn.Nodes[sn.RootID]
	list, _ := root.Raw[layout.RegionsField].([]any)
	for _, entry := range list {
		m, ok := entry.(map[string]any)
		if !ok {
			return fmt.Errorf("merge: malformed region entry in root")
		}
		id, err := refID(m)
		if err != nil {
			return err
		}
		rn, ok := sn.Nodes[id]
		if !ok {
			return fmt.Errorf("merge: root references unknown region $id:%d", id)
		}
		if rn.Type != layout.RegionType {
			return fmt.Errorf("merge: region entry $id:%d has type %q, want %q", id, rn.Type, layout.RegionType)
		}
		name, _ := rn.Raw[layout.NameField].(string)
		if name == "" {
			return fmt.Errorf("merge: region $id:%d has no name", id)
		}
		rn.Structural = true
		rn.Key = rn.Type + "|" + name
		delete(sn.ByKey, fmt.Sprintf("%s#%d", rn.Type, rn.ID))
		sn.ByKey[rn.Key] = rn
		sn.Regions = append(sn.Regions, name)
	}
	// Phase 1: direct members (objects the region lists itself),
	// regions in root order. Phase 2: objects only reachable
	// through other objects (e.g. carried items) are claimed
	// breadth-first from their owners. Direct membership wins over
	// transitive reachability, so a cross-region reference does not
	// drag the referenced object into the referencer's region.
	type reach struct {
		id     int
		region string
	}
	var queue []reach
	claim := func(id int, name string) error {
		n, ok := sn.Nodes[id]
		if !ok {
			return fmt.Errorf("merge: region %q references unknown $id:%d", name, id)
		}
		if n.Structural || n.Region != "" {
			return nil
		}
		n.Region = name
		queue = append(queue, reach{id: id, region: name})
		return nil
	}
	for _, name := range sn.Regions {
		rn := sn.ByKey[layout.RegionType+"|"+name]
		for _, id := range rn.Refs {
			if err := claim(id, name); err != nil {
				return err
			}
		}
	}
	for len(queue) > 0 {
		cur := queue[0]
		queue = queue[1:]
		for _, ref := range sn.Nodes[cur.id].Refs {
			if err := claim(ref, cur.region); err != nil {
				return err
			}
		}
	}
	return nil
}

func refID(m map[string]any) (int, error) {
	if ref, ok := m["$ref"]; ok {
		if rf, ok := ref.(float64); ok {
			return int(rf), nil
		}
		return 0, fmt.Errorf("merge: malformed $ref")
	}
	if id, ok := m["$id"]; ok {
		if idf, ok := id.(float64); ok {
			return int(idf), nil
		}
	}
	return 0, fmt.Errorf("merge: expected node or reference")
}
