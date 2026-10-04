package savecore

import (
	"encoding/json"
	"fmt"
	"sort"
)

// FlatGraph is the diff/merge-friendly form of a save envelope: every
// object node exactly once in Nodes, all intra-graph pointers reduced
// to {"$ref": id}. Flatten and Unflatten are exact inverses, so a
// merged flat graph can be published as a normal save file.
type FlatGraph struct {
	Root  int
	Nodes map[int]*FlatNode
	// Extra carries envelope-level metadata (e.g. merge reports)
	// through Unflatten. Unknown envelope keys are ignored by Decode.
	Extra map[string]any
}

type FlatNode struct {
	ID   int
	Type string
	Ver  int
	Data map[string]any // pointer fields are {"$ref": id}
}

// Flatten parses an encoded save payload into a FlatGraph.
func Flatten(payload []byte) (*FlatGraph, error) {
	var env struct {
		Format    string         `json:"format"`
		FormatVer int            `json:"formatVer"`
		Root      map[string]any `json:"root"`
	}
	if err := json.Unmarshal(payload, &env); err != nil {
		return nil, fmt.Errorf("savecore: malformed save: %w", err)
	}
	if env.Format != formatName {
		return nil, fmt.Errorf("savecore: not a savecore file (format=%q)", env.Format)
	}
	if env.FormatVer != formatVersion {
		return nil, fmt.Errorf("savecore: unsupported format version %d", env.FormatVer)
	}
	g := &FlatGraph{Nodes: map[int]*FlatNode{}}
	root, err := g.flattenNode(env.Root)
	if err != nil {
		return nil, err
	}
	g.Root = root
	return g, nil
}

func (g *FlatGraph) flattenNode(m map[string]any) (int, error) {
	idRaw, ok := m[keyID]
	if !ok {
		return 0, fmt.Errorf("savecore: node missing %q", keyID)
	}
	idf, ok := idRaw.(float64)
	if !ok {
		return 0, fmt.Errorf("savecore: node %q is not a number", keyID)
	}
	id := int(idf)
	if _, dup := g.Nodes[id]; dup {
		return 0, fmt.Errorf("savecore: duplicate node $id:%d", id)
	}
	typ, _ := m[keyType].(string)
	ver, _ := m[keyVer].(float64)
	data, _ := m[keyData].(map[string]any)
	flat := map[string]any{}
	for k, v := range data {
		fv, err := g.flattenValue(v)
		if err != nil {
			return 0, err
		}
		flat[k] = fv
	}
	g.Nodes[id] = &FlatNode{ID: id, Type: typ, Ver: int(ver), Data: flat}
	return id, nil
}

func (g *FlatGraph) flattenValue(v any) (any, error) {
	switch t := v.(type) {
	case map[string]any:
		if _, isRef := t[keyRef]; isRef {
			return t, nil
		}
		if _, isNode := t[keyID]; isNode {
			id, err := g.flattenNode(t)
			if err != nil {
				return nil, err
			}
			return map[string]any{keyRef: id}, nil
		}
		out := map[string]any{}
		for k, x := range t {
			fx, err := g.flattenValue(x)
			if err != nil {
				return nil, err
			}
			out[k] = fx
		}
		return out, nil
	case []any:
		out := make([]any, len(t))
		for i, x := range t {
			fx, err := g.flattenValue(x)
			if err != nil {
				return nil, err
			}
			out[i] = fx
		}
		return out, nil
	default:
		return v, nil
	}
}

// Unflatten rebuilds a standard save envelope. Nodes are emitted
// depth-first from the root so every $ref points at a node the
// decoder has already registered; cycles resolve as in Encode.
func (g *FlatGraph) Unflatten() ([]byte, error) {
	if _, ok := g.Nodes[g.Root]; !ok {
		return nil, fmt.Errorf("savecore: flat graph root $id:%d is missing", g.Root)
	}
	u := &unflattener{g: g, emitted: map[int]bool{}}
	root, err := u.emit(g.Root)
	if err != nil {
		return nil, err
	}
	env := map[string]any{
		"format":    formatName,
		"formatVer": formatVersion,
		"root":      root,
	}
	for k, v := range g.Extra {
		env[k] = v
	}
	return json.Marshal(env)
}

type unflattener struct {
	g       *FlatGraph
	emitted map[int]bool
}

func (u *unflattener) emit(id int) (map[string]any, error) {
	if u.emitted[id] {
		return map[string]any{keyRef: id}, nil
	}
	n, ok := u.g.Nodes[id]
	if !ok {
		return nil, fmt.Errorf("savecore: flat graph has dangling reference $ref:%d", id)
	}
	u.emitted[id] = true
	data := map[string]any{}
	for k, v := range n.Data {
		ev, err := u.emitValue(v)
		if err != nil {
			return nil, err
		}
		data[k] = ev
	}
	return map[string]any{
		keyID:   id,
		keyType: n.Type,
		keyVer:  n.Ver,
		keyData: data,
	}, nil
}

func (u *unflattener) emitValue(v any) (any, error) {
	switch t := v.(type) {
	case map[string]any:
		if ref, isRef := t[keyRef]; isRef {
			idf, ok := ref.(float64)
			if !ok {
				if ii, isInt := ref.(int); isInt {
					return u.emit(ii)
				}
				return nil, fmt.Errorf("savecore: malformed $ref %v", ref)
			}
			return u.emit(int(idf))
		}
		out := map[string]any{}
		for k, x := range t {
			ex, err := u.emitValue(x)
			if err != nil {
				return nil, err
			}
			out[k] = ex
		}
		return out, nil
	case []any:
		out := make([]any, len(t))
		for i, x := range t {
			ex, err := u.emitValue(x)
			if err != nil {
				return nil, err
			}
			out[i] = ex
		}
		return out, nil
	default:
		return v, nil
	}
}

// RefIDs returns the sorted node ids referenced anywhere in data.
func RefIDs(v any) []int {
	seen := map[int]bool{}
	collectRefs(v, seen)
	out := make([]int, 0, len(seen))
	for id := range seen {
		out = append(out, id)
	}
	sort.Ints(out)
	return out
}

func collectRefs(v any, seen map[int]bool) {
	switch t := v.(type) {
	case map[string]any:
		if ref, isRef := t[keyRef]; isRef {
			switch n := ref.(type) {
			case float64:
				seen[int(n)] = true
			case int:
				seen[n] = true
			}
			return
		}
		for _, x := range t {
			collectRefs(x, seen)
		}
	case []any:
		for _, x := range t {
			collectRefs(x, seen)
		}
	}
}

// DeepCopyData clones a decoded JSON-level value so migrations and
// merges never mutate a candidate's original data.
func DeepCopyData(v any) any {
	switch t := v.(type) {
	case map[string]any:
		out := make(map[string]any, len(t))
		for k, x := range t {
			out[k] = DeepCopyData(x)
		}
		return out
	case []any:
		out := make([]any, len(t))
		for i, x := range t {
			out[i] = DeepCopyData(x)
		}
		return out
	default:
		return v
	}
}
