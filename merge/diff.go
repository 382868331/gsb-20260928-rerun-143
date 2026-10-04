package merge

import (
	"encoding/json"
	"fmt"
	"reflect"
	"sort"
)

// ChangeKind classifies one object-level difference between the base
// and a candidate.
type ChangeKind string

const (
	Added    ChangeKind = "added"    // not present in the base at all
	Deleted  ChangeKind = "deleted"  // present in the base, gone in the candidate
	Modified ChangeKind = "modified" // same region, different data
	Moved    ChangeKind = "moved"    // same object, different region
)

// Change is one object-level difference between base and candidate.
type Change struct {
	Key        string
	Kind       ChangeKind
	FromRegion string // region in the base ("" when added)
	ToRegion   string // region in the candidate ("" when deleted)
	summary    string // canonical form of the candidate-side node
}

// Describe renders the change for an operator.
func (c Change) Describe() string {
	switch c.Kind {
	case Added:
		return fmt.Sprintf("%s: added in region %q", c.Key, c.ToRegion)
	case Deleted:
		return fmt.Sprintf("%s: deleted from region %q", c.Key, c.FromRegion)
	case Modified:
		return fmt.Sprintf("%s: modified in region %q", c.Key, c.ToRegion)
	case Moved:
		return fmt.Sprintf("%s: moved from region %q to region %q", c.Key, c.FromRegion, c.ToRegion)
	}
	return c.Key
}

// regionsTouched lists the regions this change belongs to; a move
// touches both its old and its new region.
func (c Change) regionsTouched() []string {
	switch c.Kind {
	case Added:
		return []string{c.ToRegion}
	case Deleted, Modified:
		return []string{c.FromRegion}
	case Moved:
		if c.FromRegion == c.ToRegion {
			return []string{c.ToRegion}
		}
		return []string{c.FromRegion, c.ToRegion}
	}
	return nil
}

// fingerprint identifies a change for equality checks between the
// two candidates: same object, same kind, same resulting content.
func (c Change) fingerprint() string {
	return c.Key + "|" + string(c.Kind) + "|" + c.FromRegion + "|" + c.ToRegion + "|" + c.summary
}

// normalize drops fields that carry no information (null or empty
// string) so that a migrated node, whose map may simply lack a
// field the current struct writes as a zero value, still compares
// equal to a current-version node.
func normalize(v any) any {
	switch t := v.(type) {
	case map[string]any:
		out := make(map[string]any, len(t))
		for k, e := range t {
			switch e.(type) {
			case nil:
				continue
			case string:
				if e == "" {
					continue
				}
			}
			out[k] = normalize(e)
		}
		return out
	case []any:
		out := make([]any, len(t))
		for i, e := range t {
			out[i] = normalize(e)
		}
		return out
	default:
		return v
	}
}

func sameData(a, b *Node) bool {
	return reflect.DeepEqual(normalize(a.View), normalize(b.View))
}

func summaryOf(n *Node) string {
	raw, err := json.Marshal(normalize(n.View))
	if err != nil {
		return "<unserializable>"
	}
	return string(raw)
}

// Diff compares base against cand at the object level. Identity is
// the stable key, so a moved object is reported as Moved, never as
// delete+add.
func Diff(base, cand *Snapshot) []Change {
	var out []Change
	for key, bn := range base.ByKey {
		if bn.Structural {
			continue
		}
		cn, ok := cand.ByKey[key]
		if !ok {
			out = append(out, Change{Key: key, Kind: Deleted, FromRegion: bn.Region, summary: summaryOf(bn)})
			continue
		}
		switch {
		case bn.Region != cn.Region:
			out = append(out, Change{Key: key, Kind: Moved, FromRegion: bn.Region, ToRegion: cn.Region, summary: summaryOf(cn)})
			if !sameData(bn, cn) {
				out = append(out, Change{Key: key, Kind: Modified, FromRegion: cn.Region, ToRegion: cn.Region, summary: summaryOf(cn)})
			}
		case !sameData(bn, cn):
			out = append(out, Change{Key: key, Kind: Modified, FromRegion: cn.Region, ToRegion: cn.Region, summary: summaryOf(cn)})
		}
	}
	for key, cn := range cand.ByKey {
		if cn.Structural {
			continue
		}
		if _, ok := base.ByKey[key]; !ok {
			out = append(out, Change{Key: key, Kind: Added, ToRegion: cn.Region, summary: summaryOf(cn)})
		}
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Key != out[j].Key {
			return out[i].Key < out[j].Key
		}
		return out[i].Kind < out[j].Kind
	})
	return out
}

// changeSetEqual reports whether two candidates made exactly the
// same changes to a region.
func changeSetEqual(a, b []Change) bool {
	if len(a) != len(b) {
		return false
	}
	fa := make([]string, len(a))
	fb := make([]string, len(b))
	for i := range a {
		fa[i] = a[i].fingerprint()
	}
	for i := range b {
		fb[i] = b[i].fingerprint()
	}
	sort.Strings(fa)
	sort.Strings(fb)
	return reflect.DeepEqual(fa, fb)
}
