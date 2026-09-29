package gamesave

import (
	"fmt"
	"reflect"
	"sync"
)

// Object is implemented by every registered game object type. ObjectID is
// only meaningful in memory: it is never serialized and never used to decide
// persistence identity. Identity in a save file is assigned per graph traversal.
type Object interface {
	ObjectID() uint64
}

// Migrator is implemented by object types that can upgrade data loaded from an
// older save. The argument is the field map exactly as found in the old save;
// references still appear as {"kind":"ref","id":N} markers. A migrator returns
// a field map shaped like the current struct version, after which the engine
// instantiates the current type and rewires every reference. Migration is thus
// pure data rewriting supplied by the game - the file itself carries no code.
type Migrator interface {
	Migrate(from int, fields map[string]any) (map[string]any, error)
}

type typeEntry struct {
	name     string
	version  int
	new      func() Object
	legacy   bool
	migrator Migrator
}

// Registry maps the wire name/version of a game object type to Go constructors.
// Reading a save never runs init code discovered from the file: only types that
// were explicitly registered by the game can be instantiated.
type Registry struct {
	mu      sync.RWMutex
	current map[string]*typeEntry
	byname  map[string]map[int]*typeEntry
}

func NewRegistry() *Registry {
	return &Registry{
		current: map[string]*typeEntry{},
		byname:  map[string]map[int]*typeEntry{},
	}
}

// Register adds a type version. The newest registered version for a name is
// the current version used when saving. Older versions must stay registered so
// ancient saves remain readable; supply a migrator on the current entry to
// upgrade them. The prototype carries the migrator (typically a pointer to the
// current struct), and may itself be the zero value returned by newFunc.
func (r *Registry) Register(name string, version int, newFunc func() Object) error {
	if name == "" {
		return fmt.Errorf("gamesave: register: empty type name")
	}
	if version < 1 {
		return fmt.Errorf("gamesave: register %s: version must be >= 1, got %d", name, version)
	}
	if newFunc == nil {
		return fmt.Errorf("gamesave: register %s v%d: nil constructor", name, version)
	}
	proto := newFunc()
	if proto == nil {
		return fmt.Errorf("gamesave: register %s v%d: constructor returned nil", name, version)
	}
	if _, ok := typeOf(proto); !ok {
		return fmt.Errorf("gamesave: register %s v%d: %T must be a pointer to a struct", name, version, proto)
	}

	r.mu.Lock()
	defer r.mu.Unlock()
	if r.byname[name] == nil {
		r.byname[name] = map[int]*typeEntry{}
	}
	if _, exists := r.byname[name][version]; exists {
		return fmt.Errorf("gamesave: register %s v%d: duplicate registration", name, version)
	}
	e := &typeEntry{name: name, version: version, new: newFunc}
	if m, ok := proto.(Migrator); ok {
		e.migrator = m
	}
	r.byname[name][version] = e
	cur := r.current[name]
	if cur == nil || version > cur.version {
		r.current[name] = e
	}
	return nil
}

func (r *Registry) lookup(name string, version int) (*typeEntry, error) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	vs, ok := r.byname[name]
	if !ok {
		return nil, &UnknownTypeError{Name: name, Version: version}
	}
	e, ok := vs[version]
	if !ok {
		return nil, &UnknownTypeError{Name: name, Version: version, KnownVersions: versionsOf(vs)}
	}
	return e, nil
}

// RegisterLegacy records that an old version of a type may appear in ancient
// saves. No constructor is needed: the current version's migrator upgrades the
// raw record before anything is instantiated.
func (r *Registry) RegisterLegacy(name string, version int) error {
	if version < 1 {
		return fmt.Errorf("gamesave: legacy %s: version must be >= 1", name)
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.byname[name] == nil {
		r.byname[name] = map[int]*typeEntry{}
	}
	if _, exists := r.byname[name][version]; exists {
		return fmt.Errorf("gamesave: legacy %s v%d: duplicate registration", name, version)
	}
	r.byname[name][version] = &typeEntry{name: name, version: version, legacy: true}
	return nil
}

// MustRegister / MustRegisterLegacy panic on registration errors; intended for
// package-level game setup where a failure is a programming error.
func (r *Registry) MustRegister(name string, version int, newFunc func() Object) {
	if err := r.Register(name, version, newFunc); err != nil {
		panic(err)
	}
}

func (r *Registry) MustRegisterLegacy(name string, version int) {
	if err := r.RegisterLegacy(name, version); err != nil {
		panic(err)
	}
}

func (r *Registry) currentEntry(name string) (*typeEntry, error) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	e, ok := r.current[name]
	if !ok {
		return nil, &UnknownTypeError{Name: name}
	}
	return e, nil
}

func versionsOf(vs map[int]*typeEntry) []int {
	out := make([]int, 0, len(vs))
	for v := range vs {
		out = append(out, v)
	}
	return out
}

// typeOf requires Objects to be pointers to structs, which gives reflection a
// settable target and makes pointer identity well defined.
func typeOf(obj Object) (reflect.Type, bool) {
	t := reflect.TypeOf(obj)
	if t == nil || t.Kind() != reflect.Ptr || t.Elem().Kind() != reflect.Struct {
		return nil, false
	}
	return t, true
}

// registryKey maps a concrete Go pointer-to-struct type to its wire name. Type
// identity (not field structure) decides what a save record is.
func (r *Registry) nameFor(t reflect.Type) (string, int, bool) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	for _, e := range r.current {
		et, _ := typeOf(e.new())
		if et == t {
			return e.name, e.version, true
		}
	}
	return "", 0, false
}

// UnknownTypeError is returned when a save contains a type name/version that
// was never registered. Loading fails outright instead of guessing or skipping.
type UnknownTypeError struct {
	Name          string
	Version       int
	KnownVersions []int
}

func (e *UnknownTypeError) Error() string {
	if e.Version <= 0 {
		return fmt.Sprintf("gamesave: unknown object type %q", e.Name)
	}
	if len(e.KnownVersions) > 0 {
		return fmt.Sprintf("gamesave: unknown object type %q version %d (known versions: %v)", e.Name, e.Version, e.KnownVersions)
	}
	return fmt.Sprintf("gamesave: unknown object type %q version %d", e.Name, e.Version)
}
