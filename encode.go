package gamesave

import (
	"fmt"
	"reflect"
)

type encoder struct {
	reg     *Registry
	objects []wireObject
	seen    map[uintptr]int
	queue   []reflect.Value
}

func newEncoder(reg *Registry) *encoder {
	return &encoder{reg: reg, seen: map[uintptr]int{}}
}

// intern registers an object pointer and returns its table id, queuing it for
// field encoding if this is the first sighting. Pointer identity is the only
// dedup key, so shared objects and cycles survive a round trip.
func (e *encoder) intern(v reflect.Value) int {
	key := v.Pointer()
	if id, ok := e.seen[key]; ok {
		return id
	}
	id := len(e.objects)
	e.seen[key] = id
	e.objects = append(e.objects, wireObject{ID: id})
	e.queue = append(e.queue, v)
	return id
}

func (e *encoder) encodeRoot(v reflect.Value) (any, error) {
	return e.encodeValue(v)
}

// encodeValue turns a Go value into JSON-safe data. Registered object pointers
// become refs; other pointers are followed (nil -> null).
func (e *encoder) encodeValue(v reflect.Value) (any, error) {
	switch v.Kind() {
	case reflect.Ptr:
		if v.IsNil() {
			return nil, nil
		}
		if _, ok := asObject(v); ok {
			if _, _, known := e.reg.nameFor(v.Type()); !known {
				return nil, fmt.Errorf("gamesave: cannot save %s: type %s is not registered", pathOf(v.Interface().(Object)), v.Type())
			}
			return refValue(e.intern(v)), nil
		}
		return e.encodeValue(v.Elem())
	case reflect.Interface:
		if v.IsNil() {
			return nil, nil
		}
		return e.encodeValue(v.Elem())
	case reflect.Struct:
		out := wireField{}
		if err := e.encodeStruct(v, out); err != nil {
			return nil, err
		}
		return out, nil
	case reflect.Slice, reflect.Array:
		if v.Kind() == reflect.Slice && v.IsNil() {
			return nil, nil
		}
		out := make([]any, v.Len())
		for i := 0; i < v.Len(); i++ {
			val, err := e.encodeValue(v.Index(i))
			if err != nil {
				return nil, err
			}
			out[i] = val
		}
		return out, nil
	case reflect.Map:
		if v.IsNil() {
			return nil, nil
		}
		if v.Type().Key().Kind() != reflect.String {
			return nil, fmt.Errorf("gamesave: map keys must be string, got %s", v.Type().Key())
		}
		out := wireField{}
		iter := v.MapRange()
		for iter.Next() {
			val, err := e.encodeValue(iter.Value())
			if err != nil {
				return nil, err
			}
			out[iter.Key().String()] = val
		}
		return out, nil
	case reflect.String:
		return v.String(), nil
	case reflect.Bool:
		return v.Bool(), nil
	case reflect.Int, reflect.Int8, reflect.Int16, reflect.Int32, reflect.Int64:
		return v.Int(), nil
	case reflect.Uint, reflect.Uint8, reflect.Uint16, reflect.Uint32, reflect.Uint64, reflect.Uintptr:
		return v.Uint(), nil
	case reflect.Float32, reflect.Float64:
		return v.Float(), nil
	default:
		return nil, fmt.Errorf("gamesave: unsupported field type %s", v.Type())
	}
}

func (e *encoder) encodeStruct(v reflect.Value, out wireField) error {
	t := v.Type()
	for i := 0; i < t.NumField(); i++ {
		f := t.Field(i)
		if !f.IsExported() {
			continue
		}
		name := jsonFieldName(f)
		if name == "-" {
			continue
		}
		fv := v.Field(i)
		if f.Anonymous {
			if fv.Kind() == reflect.Ptr {
				if fv.IsNil() {
					continue
				}
				if _, ok := asObject(fv); ok {
					val, err := e.encodeValue(fv)
					if err != nil {
						return err
					}
					out[name] = val
					continue
				}
				fv = fv.Elem()
			}
			if fv.Kind() == reflect.Struct {
				if err := e.encodeStruct(fv, out); err != nil {
					return err
				}
				continue
			}
		}
		val, err := e.encodeValue(fv)
		if err != nil {
			return fmt.Errorf("field %s.%s: %w", t.Name(), name, err)
		}
		out[name] = val
	}
	return nil
}

// flush fills every queued object's fields. Queue processing tolerates cycles
// because intern records identity first; field encoding happens afterwards.
func (e *encoder) flush() error {
	for len(e.queue) > 0 {
		v := e.queue[0]
		e.queue = e.queue[1:]
		id := e.seen[v.Pointer()]
		name, version, ok := e.reg.nameFor(v.Type())
		if !ok {
			return fmt.Errorf("gamesave: cannot save %s: type %s is not registered", pathOf(v.Interface().(Object)), v.Type())
		}
		fields := wireField{}
		if err := e.encodeStruct(v.Elem(), fields); err != nil {
			return err
		}
		e.objects[id].Type = name
		e.objects[id].Version = version
		e.objects[id].Fields = fields
	}
	return nil
}

func asObject(v reflect.Value) (Object, bool) {
	if v.Kind() != reflect.Ptr {
		return nil, false
	}
	obj, ok := v.Interface().(Object)
	return obj, ok
}

func pathOf(obj Object) string {
	return fmt.Sprintf("object(id=%d)", obj.ObjectID())
}
