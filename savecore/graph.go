package savecore

import (
	"encoding/json"
	"fmt"
	"reflect"
	"strings"
)

// Graph envelope keys. Objects are nodes {"$id","$type","$ver","data"};
// references are {"$ref":id}. Identity belongs to pointer values, so
// a shared/cyclic pointer survives a save/load round trip exactly.
const (
	keyID   = "$id"
	keyRef  = "$ref"
	keyType = "$type"
	keyVer  = "$ver"
	keyData = "data"
)

// Encode walks the object graph rooted at root and returns its JSON
// serialization. Every registered object must be a *struct; shared
// pointers become one node plus refs, cycles are supported.
func (r *Registry) Encode(root any) ([]byte, error) {
	return r.EncodeWithHook(root, nil)
}

// EncodeWithHook is Encode plus a callback fired for every object
// node, in first-visit order. Branch uses it to attach stable
// identities without a second graph walk.
func (r *Registry) EncodeWithHook(root any, hook func(obj any, id int)) ([]byte, error) {
	if root == nil {
		return nil, fmt.Errorf("savecore: cannot encode nil root")
	}
	rv := reflect.ValueOf(root)
	if rv.Kind() != reflect.Ptr || rv.IsNil() {
		return nil, fmt.Errorf("savecore: root must be a non-nil pointer")
	}
	e := &encoder{
		reg:   r,
		seen:  map[ptrKey]int{},
		types: map[reflect.Type]string{},
		hook:  hook,
	}
	for name, c := range r.codecs {
		e.types[reflect.TypeOf(c.New()).Elem()] = name
	}
	node, err := e.encodePtr(rv)
	if err != nil {
		return nil, err
	}
	env := map[string]any{
		"format":    formatName,
		"formatVer": formatVersion,
		"root":      node,
	}
	return json.Marshal(env)
}

type encoder struct {
	reg   *Registry
	seen  map[ptrKey]int
	types map[reflect.Type]string
	next  int
	hook  func(obj any, id int)
}

// ptrKey identifies a pointer: reflect.Value itself must not be used
// as a map key (it contains uncomparable internals).
type ptrKey struct {
	ptr uintptr
	typ reflect.Type
}

// encodePtr encodes a registered pointer as a node, or a reference if
// this pointer was already visited.
func (e *encoder) encodePtr(rv reflect.Value) (map[string]any, error) {
	key := ptrKey{ptr: rv.Pointer(), typ: rv.Elem().Type()}
	if id, ok := e.seen[key]; ok {
		return map[string]any{keyRef: id}, nil
	}
	name, ok := e.types[rv.Elem().Type()]
	if !ok {
		return nil, fmt.Errorf("savecore: type %s is not registered", rv.Elem().Type())
	}
	e.next++
	id := e.next
	e.seen[key] = id
	if e.hook != nil {
		e.hook(rv.Interface(), id)
	}
	c := e.reg.codecs[name]
	data, err := e.encodeStruct(rv.Elem())
	if err != nil {
		return nil, err
	}
	return map[string]any{
		keyID:   id,
		keyType: name,
		keyVer:  c.Version,
		keyData: data,
	}, nil
}

func (e *encoder) encodeStruct(rv reflect.Value) (map[string]any, error) {
	rt := rv.Type()
	out := map[string]any{}
	for i := 0; i < rt.NumField(); i++ {
		f := rt.Field(i)
		if !f.IsExported() {
			continue
		}
		v, err := e.encodeValue(rv.Field(i))
		if err != nil {
			return nil, fmt.Errorf("field %s.%s: %w", rt.Name(), f.Name, err)
		}
		out[f.Name] = v
	}
	return out, nil
}

func (e *encoder) encodeValue(rv reflect.Value) (any, error) {
	switch rv.Kind() {
	case reflect.Ptr:
		if rv.IsNil() {
			return nil, nil
		}
		if rv.Elem().Kind() != reflect.Struct {
			return nil, fmt.Errorf("pointer to %s is not a registered object", rv.Elem().Kind())
		}
		return e.encodePtr(rv)
	case reflect.Interface:
		if rv.IsNil() {
			return nil, nil
		}
		return nil, fmt.Errorf("interface fields are not supported (prevents hidden code execution)")
	case reflect.Struct:
		return e.encodeStruct(rv)
	case reflect.Slice, reflect.Array:
		if rv.Kind() == reflect.Slice && rv.IsNil() {
			return nil, nil
		}
		s := make([]any, rv.Len())
		for i := range s {
			v, err := e.encodeValue(rv.Index(i))
			if err != nil {
				return nil, err
			}
			s[i] = v
		}
		return s, nil
	case reflect.Map:
		if rv.IsNil() {
			return nil, nil
		}
		if rv.Type().Key().Kind() != reflect.String {
			return nil, fmt.Errorf("only string-keyed maps are supported")
		}
		m := map[string]any{}
		iter := rv.MapRange()
		for iter.Next() {
			v, err := e.encodeValue(iter.Value())
			if err != nil {
				return nil, err
			}
			m[iter.Key().String()] = v
		}
		return m, nil
	case reflect.String:
		return rv.String(), nil
	case reflect.Bool:
		return rv.Bool(), nil
	case reflect.Int, reflect.Int8, reflect.Int16, reflect.Int32, reflect.Int64:
		return rv.Int(), nil
	case reflect.Uint, reflect.Uint8, reflect.Uint16, reflect.Uint32, reflect.Uint64:
		return rv.Uint(), nil
	case reflect.Float32, reflect.Float64:
		return rv.Float(), nil
	default:
		return nil, fmt.Errorf("unsupported kind %s", rv.Kind())
	}
}

// Decode parses a save envelope and reconstructs the object graph.
// Only registered types are instantiated; unknown types and dangling
// references are errors.
func (r *Registry) Decode(raw []byte) (any, error) {
	return r.DecodeWithHook(raw, nil)
}

// DecodeWithHook is Decode plus a callback fired as each object is
// registered, before its fields are filled. Branch uses it to
// reconnect stable identities to live pointers after a load.
func (r *Registry) DecodeWithHook(raw []byte, hook func(obj any, id int)) (any, error) {
	var env struct {
		Format    string          `json:"format"`
		FormatVer int             `json:"formatVer"`
		Root      json.RawMessage `json:"root"`
	}
	if err := json.Unmarshal(raw, &env); err != nil {
		return nil, fmt.Errorf("savecore: malformed save: %w", err)
	}
	if env.Format != formatName {
		return nil, fmt.Errorf("savecore: not a savecore file (format=%q)", env.Format)
	}
	if env.FormatVer != formatVersion {
		return nil, fmt.Errorf("savecore: unsupported format version %d (want %d)", env.FormatVer, formatVersion)
	}
	d := &decoder{reg: r, table: map[int]reflect.Value{}, filled: map[int]bool{}, hook: hook}
	// Pass 1: register a zero instance for every node in the
	// document, so pass 2 resolves $ref regardless of the order
	// nodes appear in (merged saves emit nodes in graph order,
	// which is not necessarily struct field order).
	var rootAny any
	if err := json.Unmarshal(env.Root, &rootAny); err != nil {
		return nil, fmt.Errorf("savecore: malformed root: %w", err)
	}
	if err := d.preRegister(rootAny); err != nil {
		return nil, err
	}
	root, err := d.decodeRef(json.RawMessage(env.Root))
	if err != nil {
		return nil, err
	}
	if !root.IsValid() {
		return nil, fmt.Errorf("savecore: save has nil root")
	}
	return root.Interface(), nil
}

type decoder struct {
	reg    *Registry
	table  map[int]reflect.Value
	filled map[int]bool
	hook   func(obj any, id int)
}

// preRegister walks the decoded JSON tree and registers a zero
// instance for every object node, before any field is filled.
func (d *decoder) preRegister(v any) error {
	switch t := v.(type) {
	case map[string]any:
		if _, isRef := t[keyRef]; isRef {
			return nil
		}
		idRaw, isNode := t[keyID]
		if !isNode {
			for _, x := range t {
				if err := d.preRegister(x); err != nil {
					return err
				}
			}
			return nil
		}
		idf, ok := idRaw.(float64)
		if !ok {
			return fmt.Errorf("savecore: node %q is not a number", keyID)
		}
		id := int(idf)
		if _, ok := d.table[id]; !ok {
			typ, _ := t[keyType].(string)
			if typ == "" {
				return fmt.Errorf("savecore: node $id:%d missing %q", id, keyType)
			}
			c, err := d.reg.codec(typ)
			if err != nil {
				return err
			}
			ver, _ := t[keyVer].(float64)
			if ver < 1 {
				return fmt.Errorf("savecore: node %q has invalid version %d", typ, int(ver))
			}
			inst := reflect.ValueOf(c.New())
			d.table[id] = inst
			if d.hook != nil {
				d.hook(inst.Interface(), id)
			}
		}
		if data, ok := t[keyData].(map[string]any); ok {
			for _, x := range data {
				if err := d.preRegister(x); err != nil {
					return err
				}
			}
		}
		return nil
	case []any:
		for _, x := range t {
			if err := d.preRegister(x); err != nil {
				return err
			}
		}
	}
	return nil
}

// decodeRef builds (or returns) the object represented by a raw node
// or reference. It registers a zero instance before filling fields so
// cycles resolve.
func (d *decoder) decodeRef(raw json.RawMessage) (reflect.Value, error) {
	var probe struct {
		ID   *int   `json:"$id"`
		Ref  *int   `json:"$ref"`
		Type string `json:"$type"`
		Ver  int    `json:"$ver"`
		Data any    `json:"data"`
	}
	if err := json.Unmarshal(raw, &probe); err != nil {
		return reflect.Value{}, fmt.Errorf("savecore: malformed node: %w", err)
	}
	if probe.Ref != nil {
		v, ok := d.table[*probe.Ref]
		if !ok {
			return reflect.Value{}, fmt.Errorf("savecore: dangling reference $ref:%d", *probe.Ref)
		}
		return v, nil
	}
	if probe.ID == nil {
		return reflect.Value{}, fmt.Errorf("savecore: node missing %q", keyID)
	}
	id := *probe.ID
	inst, ok := d.table[id]
	if !ok {
		return reflect.Value{}, fmt.Errorf("savecore: node $id:%d was not pre-registered", id)
	}
	if d.filled[id] {
		return inst, nil
	}
	d.filled[id] = true
	c, err := d.reg.codec(probe.Type)
	if err != nil {
		return reflect.Value{}, err
	}

	var rawData map[string]json.RawMessage
	wrapper := struct {
		D map[string]json.RawMessage `json:"data"`
	}{D: rawData}
	if err := json.Unmarshal(raw, &wrapper); err != nil {
		return reflect.Value{}, fmt.Errorf("savecore: node %s: malformed data: %w", probe.Type, err)
	}
	rawData = wrapper.D
	data := map[string]any{}
	for k, v := range rawData {
		var x any
		if err := json.Unmarshal(v, &x); err != nil {
			return reflect.Value{}, fmt.Errorf("savecore: node %s field %s: %w", probe.Type, k, err)
		}
		data[k] = x
	}
	if probe.Ver != c.Version {
		data, err = d.reg.migrateTo(probe.Type, probe.Ver, data)
		if err != nil {
			return reflect.Value{}, err
		}
	}
	if err := d.fillStruct(inst.Elem(), data); err != nil {
		return reflect.Value{}, fmt.Errorf("node %s: %w", probe.Type, err)
	}
	return inst, nil
}

// decodeNode handles a value known to be a node or ref (pointer
// fields). It re-marshals the generic map because decodeRef speaks
// json.RawMessage.
func (d *decoder) decodeNode(v any) (reflect.Value, error) {
	if v == nil {
		return reflect.Value{}, nil
	}
	raw, err := json.Marshal(v)
	if err != nil {
		return reflect.Value{}, err
	}
	return d.decodeRef(raw)
}

func (d *decoder) fillStruct(rv reflect.Value, data map[string]any) error {
	rt := rv.Type()
	for i := 0; i < rt.NumField(); i++ {
		f := rt.Field(i)
		if !f.IsExported() {
			continue
		}
		raw, present := data[f.Name]
		if !present {
			continue
		}
		if err := d.fillValue(rv.Field(i), raw); err != nil {
			return fmt.Errorf("field %s: %w", f.Name, err)
		}
	}
	var extra []string
	for k := range data {
		if _, ok := rt.FieldByName(k); !ok {
			extra = append(extra, k)
		}
	}
	if len(extra) > 0 {
		return fmt.Errorf("unknown fields %s in current struct version (add a migration instead)",
			strings.Join(extra, ","))
	}
	return nil
}

func (d *decoder) fillValue(rv reflect.Value, raw any) error {
	switch rv.Kind() {
	case reflect.Ptr:
		if raw == nil {
			return nil
		}
		if rv.Type().Elem().Kind() != reflect.Struct {
			return fmt.Errorf("pointer to %s is not a registered object", rv.Type().Elem().Kind())
		}
		obj, err := d.decodeNode(raw)
		if err != nil {
			return err
		}
		if obj.IsValid() {
			rv.Set(obj)
		}
		return nil
	case reflect.Interface:
		return fmt.Errorf("interface fields are not supported (prevents hidden code execution)")
	case reflect.Struct:
		m, ok := raw.(map[string]any)
		if !ok {
			return typeMismatch(rv.Type(), raw)
		}
		return d.fillStruct(rv, m)
	case reflect.Slice:
		if raw == nil {
			return nil
		}
		s, ok := raw.([]any)
		if !ok {
			return typeMismatch(rv.Type(), raw)
		}
		out := reflect.MakeSlice(rv.Type(), len(s), len(s))
		for i, item := range s {
			if err := d.fillValue(out.Index(i), item); err != nil {
				return err
			}
		}
		rv.Set(out)
		return nil
	case reflect.Map:
		if raw == nil {
			return nil
		}
		m, ok := raw.(map[string]any)
		if !ok {
			return typeMismatch(rv.Type(), raw)
		}
		if rv.Type().Key().Kind() != reflect.String {
			return fmt.Errorf("only string-keyed maps are supported")
		}
		out := reflect.MakeMap(rv.Type())
		for k, item := range m {
			ev := reflect.New(rv.Type().Elem()).Elem()
			if err := d.fillValue(ev, item); err != nil {
				return err
			}
			out.SetMapIndex(reflect.ValueOf(k), ev)
		}
		rv.Set(out)
		return nil
	case reflect.String, reflect.Bool,
		reflect.Int, reflect.Int8, reflect.Int16, reflect.Int32, reflect.Int64,
		reflect.Uint, reflect.Uint8, reflect.Uint16, reflect.Uint32, reflect.Uint64,
		reflect.Float32, reflect.Float64:
		return setScalar(rv, raw)
	default:
		return fmt.Errorf("unsupported kind %s", rv.Kind())
	}
}

func typeMismatch(t reflect.Type, raw any) error {
	return fmt.Errorf("cannot decode %T into %s", raw, t)
}

func setScalar(rv reflect.Value, raw any) error {
	switch rv.Kind() {
	case reflect.String:
		s, ok := raw.(string)
		if !ok {
			return typeMismatch(rv.Type(), raw)
		}
		rv.SetString(s)
	case reflect.Bool:
		b, ok := raw.(bool)
		if !ok {
			return typeMismatch(rv.Type(), raw)
		}
		rv.SetBool(b)
	case reflect.Int, reflect.Int8, reflect.Int16, reflect.Int32, reflect.Int64:
		n, err := asInt(raw)
		if err != nil {
			return err
		}
		rv.SetInt(n)
	case reflect.Uint, reflect.Uint8, reflect.Uint16, reflect.Uint32, reflect.Uint64:
		n, err := asInt(raw)
		if err != nil {
			return err
		}
		if n < 0 {
			return fmt.Errorf("negative value %d for unsigned field", n)
		}
		rv.SetUint(uint64(n))
	case reflect.Float32, reflect.Float64:
		f, ok := raw.(float64)
		if !ok {
			return typeMismatch(rv.Type(), raw)
		}
		rv.SetFloat(f)
	}
	return nil
}

func asInt(raw any) (int64, error) {
	switch n := raw.(type) {
	case float64:
		return int64(n), nil
	case int64:
		return n, nil
	case int:
		return int64(n), nil
	default:
		return 0, fmt.Errorf("cannot decode %T as integer", raw)
	}
}
