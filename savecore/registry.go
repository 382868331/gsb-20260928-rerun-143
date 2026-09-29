// Package savecore implements the core of a game save service:
// identity-preserving object graph encoding, per-type version
// migration, and atomic checksummed file writes. The three concerns
// are deliberately separated so new game object types can be added
// (as new codecs) without touching the write path or old saves.
package savecore

import (
	"fmt"
	"reflect"
	"sort"
)

// MigrateFunc upgrades a raw saved data payload from version "from"
// to version "from+1" of the same type. It operates on the decoded
// JSON-level map, never on live game objects.
type MigrateFunc func(from int, data map[string]any) (map[string]any, error)

// Codec describes one saveable object type. New returns a pointer to
// a zero value of the type; it is the only way instances are created
// during load, so loading a save can never construct a type the
// program did not explicitly register.
type Codec struct {
	Name    string
	Version int
	New     func() any
}

// Registry maps save-format type names to codecs and migrations.
// It is the single source of truth for object identity semantics.
type Registry struct {
	codecs     map[string]*Codec
	migrations map[string]map[int]MigrateFunc
}

func NewRegistry() *Registry {
	return &Registry{
		codecs:     map[string]*Codec{},
		migrations: map[string]map[int]MigrateFunc{},
	}
}

// Register adds a codec. It panics on duplicate names or a New func
// that does not return a pointer, since those are programmer errors.
func (r *Registry) Register(c Codec) {
	if c.Name == "" || c.New == nil || c.Version < 1 {
		panic(fmt.Sprintf("savecore: invalid codec %+v", c))
	}
	if _, dup := r.codecs[c.Name]; dup {
		panic("savecore: duplicate codec " + c.Name)
	}
	t := reflect.TypeOf(c.New())
	if t.Kind() != reflect.Ptr || t.Elem().Kind() != reflect.Struct {
		panic("savecore: codec New must return pointer to struct: " + c.Name)
	}
	r.codecs[c.Name] = &c
}

// AddMigration registers a step migration from version "from" to
// "from+1" for the named type.
func (r *Registry) AddMigration(typeName string, from int, f MigrateFunc) {
	if _, ok := r.codecs[typeName]; !ok {
		panic("savecore: migration for unregistered type " + typeName)
	}
	m := r.migrations[typeName]
	if m == nil {
		m = map[int]MigrateFunc{}
		r.migrations[typeName] = m
	}
	m[from] = f
}

// UnknownTypeError is returned when a save references a type the
// program never registered. Loading fails loudly instead of silently
// dropping or duplicating objects.
type UnknownTypeError struct{ TypeName string }

func (e *UnknownTypeError) Error() string {
	return fmt.Sprintf("savecore: unknown object type %q", e.TypeName)
}

// MigrationError is returned when a saved version cannot be brought
// up to the current version (too new, or a missing step).
type MigrationError struct {
	TypeName string
	From, To int
	Reason   string
}

func (e *MigrationError) Error() string {
	return fmt.Sprintf("savecore: cannot migrate %q from version %d to %d: %s",
		e.TypeName, e.From, e.To, e.Reason)
}

func (r *Registry) codec(name string) (*Codec, error) {
	c, ok := r.codecs[name]
	if !ok {
		return nil, &UnknownTypeError{TypeName: name}
	}
	return c, nil
}

// migrateTo upgrades raw data of type "name" from "ver" to the
// codec's current version, applying registered steps in order.
func (r *Registry) migrateTo(name string, ver int, data map[string]any) (map[string]any, error) {
	c, err := r.codec(name)
	if err != nil {
		return nil, err
	}
	if ver > c.Version {
		return nil, &MigrationError{TypeName: name, From: ver, To: c.Version,
			Reason: "save was written by a newer version of the game"}
	}
	for ver < c.Version {
		step, ok := r.migrations[name][ver]
		if !ok {
			return nil, &MigrationError{TypeName: name, From: ver, To: c.Version,
				Reason: "no migration path registered"}
		}
		data, err = step(ver, data)
		if err != nil {
			return nil, &MigrationError{TypeName: name, From: ver, To: c.Version,
				Reason: err.Error()}
		}
		ver++
	}
	return data, nil
}

// TypeNames returns registered type names, for diagnostics.
func (r *Registry) TypeNames() []string {
	names := make([]string, 0, len(r.codecs))
	for n := range r.codecs {
		names = append(names, n)
	}
	sort.Strings(names)
	return names
}
