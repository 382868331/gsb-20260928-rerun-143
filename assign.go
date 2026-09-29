package gamesave

import (
	"encoding/json"
	"fmt"
	"reflect"
)

// assignStruct fills a struct from a field map. Unknown JSON fields are
// ignored so newer saves with extra data do not break older readers that opt
// into forward handling; type mismatches are errors.
func assignStruct(dst reflect.Value, data wireField, d *decoder) error {
	t := dst.Type()
	for i := 0; i < t.NumField(); i++ {
		f := t.Field(i)
		if !f.IsExported() {
			continue
		}
		name := jsonFieldName(f)
		if name == "-" {
			continue
		}
		fv := dst.Field(i)
		if f.Anonymous {
			if fv.Kind() == reflect.Ptr {
				// Embedded object pointer: treat as a normal ref field.
				if raw, ok := data[name]; ok {
					if err := assignValue(fv, raw, d, t.Name()+"."+name); err != nil {
						return err
					}
				}
				continue
			}
			if fv.Kind() == reflect.Struct {
				// Flattened embedded struct shares the same field map.
				if err := assignStruct(fv, data, d); err != nil {
					return err
				}
				continue
			}
		}
		raw, ok := data[name]
		if !ok {
			continue
		}
		if err := assignValue(fv, raw, d, t.Name()+"."+name); err != nil {
			return err
		}
	}
	return nil
}

func assignValue(dst reflect.Value, raw any, d *decoder, path string) error {
	if !dst.CanSet() {
		// Pointers require allocation; handled in the Ptr branch.
		return fmt.Errorf("internal: unsettable %s at %s", dst.Type(), path)
	}

	// Reference marker: whatever the destination pointer type, the target must
	// be assignment-compatible. Resolved in a second pass so cycles work.
	if m, ok := isRefMarker(raw); ok {
		id, ok := refIDOf(m)
		if !ok {
			return fmt.Errorf("corrupt reference at %s", path)
		}
		dstType := dst.Type()
		if dstType.Kind() != reflect.Ptr {
			return fmt.Errorf("reference marker used for non-pointer field %s (%s)", path, dstType)
		}
		if id < 0 || id >= len(d.objects) {
			return fmt.Errorf("reference id %d out of range at %s", id, path)
		}
		d.refs = append(d.refs, pendingRef{
			id: id,
			set: func(obj reflect.Value) {
				if !obj.Type().AssignableTo(dstType) {
					panic(&refTypeError{objType: obj.Type(), dstType: dstType, id: id})
				}
				dst.Set(obj)
			},
		})
		return nil
	}

	if raw == nil {
		setZero(dst)
		return nil
	}

	switch dst.Kind() {
	case reflect.Ptr:
		if raw == nil {
			setZero(dst)
			return nil
		}
		// Registered object pointers were handled by the ref-marker branch;
		// remaining pointers are wrappers around plain values.
		if dst.IsNil() {
			dst.Set(reflect.New(dst.Type().Elem()))
		}
		return assignValue(dst.Elem(), raw, d, path)
	case reflect.Interface:
		return assignInterface(dst, raw, d, path)
	case reflect.Struct:
		m, ok := raw.(map[string]any)
		if !ok {
			return typeMismatch(path, dst.Type(), raw)
		}
		return assignStruct(dst, wireField(m), d)
	case reflect.Slice:
		arr, ok := raw.([]any)
		if !ok {
			return typeMismatch(path, dst.Type(), raw)
		}
		s := reflect.MakeSlice(dst.Type(), len(arr), len(arr))
		for i, item := range arr {
			if err := assignValue(s.Index(i), item, d, fmt.Sprintf("%s[%d]", path, i)); err != nil {
				return err
			}
		}
		dst.Set(s)
		return nil
	case reflect.Array:
		arr, ok := raw.([]any)
		if !ok {
			return typeMismatch(path, dst.Type(), raw)
		}
		if len(arr) > dst.Len() {
			return fmt.Errorf("%s: array data has %d elements, target holds %d", path, len(arr), dst.Len())
		}
		for i, item := range arr {
			if err := assignValue(dst.Index(i), item, d, fmt.Sprintf("%s[%d]", path, i)); err != nil {
				return err
			}
		}
		return nil
	case reflect.Map:
		m, ok := raw.(map[string]any)
		if !ok {
			return typeMismatch(path, dst.Type(), raw)
		}
		if dst.Type().Key().Kind() != reflect.String {
			return fmt.Errorf("%s: only string-keyed maps supported, got %s", path, dst.Type())
		}
		mp := reflect.MakeMapWithSize(dst.Type(), len(m))
		for k, item := range m {
			val := reflect.New(dst.Type().Elem()).Elem()
			if err := assignValue(val, item, d, path+"."+k); err != nil {
				return err
			}
			mp.SetMapIndex(reflect.ValueOf(k), val)
		}
		dst.Set(mp)
		return nil
	case reflect.String:
		v, ok := raw.(string)
		if !ok {
			return typeMismatch(path, dst.Type(), raw)
		}
		dst.SetString(v)
		return nil
	case reflect.Bool:
		v, ok := raw.(bool)
		if !ok {
			return typeMismatch(path, dst.Type(), raw)
		}
		dst.SetBool(v)
		return nil
	case reflect.Int, reflect.Int8, reflect.Int16, reflect.Int32, reflect.Int64:
		n, err := integer(raw)
		if err != nil {
			return fmt.Errorf("%s: %w", path, err)
		}
		dst.SetInt(n)
		return nil
	case reflect.Uint, reflect.Uint8, reflect.Uint16, reflect.Uint32, reflect.Uint64, reflect.Uintptr:
		n, err := unsigned(raw)
		if err != nil {
			return fmt.Errorf("%s: %w", path, err)
		}
		dst.SetUint(n)
		return nil
	case reflect.Float32, reflect.Float64:
		n, err := number(raw)
		if err != nil {
			return fmt.Errorf("%s: %w", path, err)
		}
		dst.SetFloat(n)
		return nil
	default:
		return fmt.Errorf("%s: unsupported target type %s", path, dst.Type())
	}
}

type refTypeError struct {
	objType, dstType reflect.Type
	id               int
}

func (e *refTypeError) Error() string {
	return fmt.Sprintf("gamesave: corrupt save: object %d (%s) is not assignable to %s", e.id, e.objType, e.dstType)
}

// assignInterface supports plain data interfaces and slices/structs behind
// them. Object pointer interfaces are reached through concrete pointer fields
// (the ref branch), so arbitrary interface -> object reconstruction is not
// needed and is intentionally unsupported to keep loading data-only.
func assignInterface(dst reflect.Value, raw any, d *decoder, path string) error {
	switch v := raw.(type) {
	case nil:
		dst.Set(reflect.Zero(dst.Type()))
	case string:
		dst.Set(reflect.ValueOf(v))
	case bool:
		dst.Set(reflect.ValueOf(v))
	case json.Number:
		if i, err := v.Int64(); err == nil {
			dst.Set(reflect.ValueOf(i))
		} else {
			f, err := v.Float64()
			if err != nil {
				return fmt.Errorf("%s: bad number %q", path, v)
			}
			dst.Set(reflect.ValueOf(f))
		}
	default:
		return fmt.Errorf("%s: cannot decode %T into interface %s (only plain data supported, never code)", path, raw, dst.Type())
	}
	return nil
}

func setZero(dst reflect.Value) { dst.Set(reflect.Zero(dst.Type())) }

func typeMismatch(path string, t reflect.Type, raw any) error {
	return fmt.Errorf("%s: cannot decode %T into %s", path, raw, t)
}

func integer(raw any) (int64, error) {
	switch n := raw.(type) {
	case int:
		return int64(n), nil
	case int64:
		return n, nil
	}
	n, ok := raw.(json.Number)
	if !ok {
		return 0, fmt.Errorf("expected integer, got %T", raw)
	}
	i, err := n.Int64()
	if err != nil {
		return 0, fmt.Errorf("expected integer, got %q", n)
	}
	return i, nil
}

func unsigned(raw any) (uint64, error) {
	switch n := raw.(type) {
	case int:
		if n < 0 {
			return 0, fmt.Errorf("expected non-negative integer, got %d", n)
		}
		return uint64(n), nil
	case int64:
		if n < 0 {
			return 0, fmt.Errorf("expected non-negative integer, got %d", n)
		}
		return uint64(n), nil
	}
	n, ok := raw.(json.Number)
	if !ok {
		return 0, fmt.Errorf("expected unsigned integer, got %T", raw)
	}
	s := n.String()
	i, err := n.Int64()
	if err != nil || i < 0 {
		return 0, fmt.Errorf("expected non-negative integer, got %q", s)
	}
	return uint64(i), nil
}

func number(raw any) (float64, error) {
	n, ok := raw.(json.Number)
	if !ok {
		return 0, fmt.Errorf("expected number, got %T", raw)
	}
	return n.Float64()
}

func refIDOf(m map[string]any) (int, bool) {
	v, ok := m["id"].(json.Number)
	if !ok {
		return 0, false
	}
	i, err := v.Int64()
	if err != nil {
		return 0, false
	}
	return int(i), true
}
