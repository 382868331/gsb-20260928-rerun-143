package gamesave

import (
	"encoding/json"
	"fmt"
	"reflect"
)

type savedObject struct {
	id      int
	typeOf  string
	version int
	fields  wireField
}

type saveModel struct {
	objects []savedObject
	roots   map[string]json.RawMessage
}

type decoder struct {
	reg     *Registry
	model   saveModel
	objects []reflect.Value
	refs    []pendingRef
}

// pendingRef records a settable location that must point at another table
// entry once every object exists. Filling these after instantiation is what
// resolves forward references and cycles.
type pendingRef struct {
	set func(reflect.Value)
	id  int
}

func newDecoder(reg *Registry, model saveModel) *decoder {
	return &decoder{reg: reg, model: model, objects: make([]reflect.Value, len(model.objects))}
}

// validate fails fast on unknown names or versions and bad ids before any
// object is built. Unknown types are an explicit error, never silently dropped.
func (d *decoder) validate() error {
	seen := map[int]bool{}
	for i, so := range d.model.objects {
		if so.id != i {
			return corrupt("object table must be ordered by id starting at 0, got id %d at position %d", so.id, i)
		}
		if seen[so.id] {
			return corrupt("duplicate object id %d", so.id)
		}
		seen[so.id] = true
		if _, err := d.reg.lookup(so.typeOf, so.version); err != nil {
			return err
		}
	}
	return nil
}

// instantiate builds every object (in id order), decodes fields, applies
// migrations for old versions, and records reference slots.
func (d *decoder) instantiate() error {
	// Pass 1: construct every object up front, in id order, so any ref marker
	// (including back-references and refs from earlier ids to later ids) points
	// at the single final instance of its target.
	targets := make([]struct {
		entry  *typeEntry
		so     savedObject
		fields wireField
		pv     reflect.Value
	}, len(d.model.objects))
	for _, so := range d.model.objects {
		entry, err := d.reg.lookup(so.typeOf, so.version)
		if err != nil {
			return err
		}
		fields := so.fields
		if entry.legacy || so.version != entry.version {
			cur, err := d.reg.currentEntry(so.typeOf)
			if err != nil {
				return err
			}
			if cur.migrator == nil {
				return &UnknownTypeError{Name: so.typeOf, Version: so.version, KnownVersions: registeredVersions(d.reg, so.typeOf)}
			}
			upgraded, err := cur.migrator.Migrate(so.version, cloneFields(fields))
			if err != nil {
				return fmt.Errorf("gamesave: migrating %s v%d -> v%d: %w", so.typeOf, so.version, cur.version, err)
			}
			fields = wireField(upgraded)
			entry = cur
		}
		proto := entry.new()
		pv := reflect.ValueOf(proto)
		d.objects[so.id] = pv
		targets[so.id] = struct {
			entry  *typeEntry
			so     savedObject
			fields wireField
			pv     reflect.Value
		}{entry: entry, so: so, fields: fields, pv: pv}
	}
	// Pass 2: decode fields. Every pointer ref resolves to the instances above.
	for _, tg := range targets {
		if err := assignStruct(tg.pv.Elem(), tg.fields, d); err != nil {
			return fmt.Errorf("gamesave: object %d (%s v%d): %w", tg.so.id, tg.so.typeOf, tg.so.version, err)
		}
	}
	return nil
}

func registeredVersions(reg *Registry, name string) []int {
	reg.mu.RLock()
	defer reg.mu.RUnlock()
	return versionsOf(reg.byname[name])
}

// resolve fills every recorded reference slot now that all objects exist.
func (d *decoder) resolve() (err error) {
	defer func() {
		if r := recover(); r != nil {
			if e, ok := r.(*refTypeError); ok {
				err = corrupt("%s", e.Error())
				return
			}
			panic(r)
		}
	}()
	for _, pr := range d.refs {
		if pr.id < 0 || pr.id >= len(d.objects) {
			return corrupt("reference id %d out of range", pr.id)
		}
		obj := d.objects[pr.id]
		if !obj.IsValid() {
			return corrupt("dangling reference id %d", pr.id)
		}
		pr.set(obj)
	}
	return nil
}

// decodeRoot decodes a named root into a settable reflect.Value (the caller
// passes a pointer to its destination variable).
func (d *decoder) decodeRoot(name string, dst reflect.Value) error {
	raw, ok := d.model.roots[name]
	if !ok {
		return fmt.Errorf("gamesave: save has no root named %q", name)
	}
	data, err := decodeAny(raw)
	if err != nil {
		return corrupt("root %q: %s", name, err.Error())
	}
	return assignValue(dst.Elem(), data, d, "root "+name)
}

func cloneFields(in wireField) map[string]any {
	out := make(map[string]any, len(in))
	for k, v := range in {
		out[k] = v
	}
	return out
}

func isRefMarker(v any) (map[string]any, bool) {
	m, ok := v.(map[string]any)
	if !ok {
		return nil, false
	}
	if kind, _ := m["kind"].(string); kind != "ref" {
		return nil, false
	}
	return m, true
}

func decodeFields(raw json.RawMessage) (wireField, error) {
	v, err := decodeAny(raw)
	if err != nil {
		return nil, err
	}
	m, ok := v.(map[string]any)
	if !ok {
		return nil, fmt.Errorf("fields must be a JSON object")
	}
	return wireField(m), nil
}

func decodeAny(raw json.RawMessage) (any, error) {
	dec := json.NewDecoder(bytesReader(raw))
	dec.UseNumber()
	var v any
	if err := dec.Decode(&v); err != nil {
		return nil, err
	}
	return v, nil
}
