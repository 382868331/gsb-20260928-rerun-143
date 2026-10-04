package merge

import (
	"encoding/json"
	"fmt"
	"os"
	"sort"

	"savegame/savecore"
)

// Finalize builds the merged save from the base plus the adopted
// regions and publishes it atomically at outPath. Undecided regions
// keep their base content and are listed explicitly in the report
// (written next to the save); the result only counts as fully merged
// when Report.Complete is true. Objects that adopted content still
// references are retained from the referencing side and listed in
// Report.Retained, so the merged graph never contains duplicates of
// the same identity and never a dangling reference.
func (s *Session) Finalize(outPath string) (*Report, error) {
	snaps := s.snapshots()
	srcOf := func(region string) string {
		if d, ok := s.Decisions[region]; ok {
			return d
		}
		return "base"
	}

	// included maps identity key -> the side its node is taken from.
	included := map[string]string{}
	retained := map[string]string{}

	// Structural nodes: the root always comes from the base; each
	// region node comes from the side chosen for that region when
	// that side has the region, otherwise from any side that does.
	rootKey := s.Base.Nodes[s.Base.RootID].Key
	included[rootKey] = "base"
	for _, region := range s.regionNames() {
		key := s.layout.RegionType + "|" + region
		for _, src := range []string{srcOf(region), "base", SideLeft, SideRight} {
			if _, ok := snaps[src].ByKey[key]; ok {
				included[key] = src
				break
			}
		}
		if _, ok := included[key]; !ok {
			return nil, fmt.Errorf("merge: region %q missing from every save", region)
		}
	}

	// Content objects: for each region, take the objects the chosen
	// side has in that region.
	for _, region := range s.regionNames() {
		src := srcOf(region)
		for key, n := range snaps[src].ByKey {
			if !n.Structural && n.Region == region {
				included[key] = src
			}
		}
	}

	// Reference integrity: anything an included node references must
	// be included too. The referencer's own side always has the
	// target, so this never fails; it only retains objects that
	// region decisions would otherwise have dropped (e.g. the
	// destination of a move whose arrival was not adopted).
	for changed := true; changed; {
		changed = false
		for key, src := range included {
			n := snaps[src].ByKey[key]
			for _, refID := range n.Refs {
				target, ok := snaps[src].Nodes[refID]
				if !ok {
					return nil, fmt.Errorf("merge: %s references unknown $id:%d", key, refID)
				}
				if _, ok := included[target.Key]; !ok {
					included[target.Key] = src
					retained[target.Key] = src
					changed = true
				}
			}
		}
	}

	em := &emitter{snaps: snaps, included: included, emitted: map[string]int{}}
	rootMap, err := em.emit(rootKey)
	if err != nil {
		return nil, err
	}
	payload, err := savecore.MarshalEnvelope(rootMap)
	if err != nil {
		return nil, err
	}
	if err := savecore.WritePayload(outPath, payload); err != nil {
		return nil, err
	}

	report := &Report{
		OutPath:   outPath,
		Decisions: map[string]string{},
		Pending:   s.Pending(),
	}
	for k, v := range s.Decisions {
		report.Decisions[k] = v
	}
	for key, src := range retained {
		report.Retained = append(report.Retained, RetainedItem{
			Key:    key,
			Source: src,
			Reason: "referenced by adopted content",
		})
	}
	sort.Slice(report.Retained, func(i, j int) bool { return report.Retained[i].Key < report.Retained[j].Key })
	report.Complete = len(report.Pending) == 0

	raw, err := json.MarshalIndent(report, "", "  ")
	if err != nil {
		return nil, err
	}
	if err := os.WriteFile(report.ReportPath(), raw, 0o644); err != nil {
		return nil, fmt.Errorf("merge: write report: %w", err)
	}
	return report, nil
}

// emitter rebuilds one graph from nodes drawn from different
// snapshots. Every identity is emitted exactly once (the first time
// it is referenced); later mentions become $ref, so shared objects
// stay shared and cycles still resolve.
type emitter struct {
	snaps    map[string]*Snapshot
	included map[string]string
	emitted  map[string]int
	next     int
}

func (e *emitter) emit(key string) (map[string]any, error) {
	if id, ok := e.emitted[key]; ok {
		return map[string]any{"$ref": id}, nil
	}
	src, ok := e.included[key]
	if !ok {
		return nil, fmt.Errorf("merge: internal: %q referenced but not included", key)
	}
	n := e.snaps[src].ByKey[key]
	e.next++
	id := e.next
	e.emitted[key] = id // register before walking data, so cycles resolve
	data, err := e.remap(src, n.Raw)
	if err != nil {
		return nil, err
	}
	return map[string]any{
		"$id":   id,
		"$type": n.Type,
		"$ver":  n.Ver, // version and data stay exactly as the source saved them
		"data":  data,
	}, nil
}

// remap rewrites references inside one node's raw data from
// file-local $id/$ref form to freshly emitted nodes keyed by stable
// identity.
func (e *emitter) remap(src string, v any) (any, error) {
	sn := e.snaps[src]
	switch t := v.(type) {
	case map[string]any:
		if _, isRef := t["$ref"]; isRef {
			id, err := refID(t)
			if err != nil {
				return nil, err
			}
			target, ok := sn.Nodes[id]
			if !ok {
				return nil, fmt.Errorf("merge: dangling $ref:%d in source %q", id, src)
			}
			return e.emit(target.Key)
		}
		if _, isNode := t["$id"]; isNode {
			id, err := refID(t)
			if err != nil {
				return nil, err
			}
			target, ok := sn.Nodes[id]
			if !ok {
				return nil, fmt.Errorf("merge: dangling node $id:%d in source %q", id, src)
			}
			return e.emit(target.Key)
		}
		out := make(map[string]any, len(t))
		for k, elem := range t {
			r, err := e.remap(src, elem)
			if err != nil {
				return nil, err
			}
			out[k] = r
		}
		return out, nil
	case []any:
		out := make([]any, len(t))
		for i, elem := range t {
			r, err := e.remap(src, elem)
			if err != nil {
				return nil, err
			}
			out[i] = r
		}
		return out, nil
	default:
		return v, nil
	}
}
