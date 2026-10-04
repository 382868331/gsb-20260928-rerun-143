package savecore

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"reflect"
	"sort"
)

// Stable identity layer. A Branch is a working copy of a save taken
// from one save point; every object gets a stable id that survives
// edits and re-saves, so two branches of the same base can later be
// compared and merged object-by-object. The manifest (stable id ->
// type/region, node id -> stable id) lives next to the save file;
// the save file itself stays a standard savecore file.

// ObjectMeta is the manifest record for one stable identity.
type ObjectMeta struct {
	Type   string `json:"type"`
	Region string `json:"region"`
}

// Manifest maps stable ids to object metadata and the save file's
// local node ids to stable ids. It is the identity bridge between
// two candidates written from the same save point.
type Manifest struct {
	// Nonce makes stable ids minted by this branch unique across
	// sibling branches forked from the same save point; Seq is the
	// branch-local counter. Both persist in the manifest file.
	Nonce   string                 `json:"nonce"`
	Seq     int                    `json:"seq"`
	Objects map[string]*ObjectMeta `json:"objects"`
	Nodes   map[int]string         `json:"nodes"`
}

func newManifest() *Manifest {
	return &Manifest{Objects: map[string]*ObjectMeta{}, Nodes: map[int]string{}}
}

// RegionOfFunc classifies a live object into a region name. Objects
// the game does not place in any region get "".
type RegionOfFunc func(obj any) string

// Branch is one line of descent from a shared save point.
type Branch struct {
	reg      *Registry
	store    *Store
	regionOf RegionOfFunc
	manifest *Manifest
	ids      map[ptrKey]string // live pointer -> stable id
}

// Fork opens (or starts) a branch at store. If a save exists it must
// already carry a manifest: branching from an unversioned file would
// make up identities that the other candidate cannot share.
func Fork(reg *Registry, store *Store, regionOf RegionOfFunc) (*Branch, error) {
	b := &Branch{reg: reg, store: store, regionOf: regionOf, ids: map[ptrKey]string{}}
	exists, err := store.Exists()
	if err != nil {
		return nil, err
	}
	if !exists {
		b.manifest = newManifest()
		return b, nil
	}
	m, err := LoadManifest(ManifestPath(store.Path))
	if err != nil {
		return nil, fmt.Errorf("savecore: cannot branch from %s: %w", store.Path, err)
	}
	b.manifest = m
	return b, nil
}

// CloneBranch starts a new branch of the save at src, stored at dst:
// the save file and its identity manifest are copied, so the new
// branch shares the source's stable identities and the source itself
// stays untouched and recoverable.
func CloneBranch(reg *Registry, src, dst *Store, regionOf RegionOfFunc) (*Branch, error) {
	frame, err := os.ReadFile(src.Path)
	if err != nil {
		return nil, fmt.Errorf("savecore: read source save: %w", err)
	}
	if err := os.WriteFile(dst.Path, frame, 0o644); err != nil {
		return nil, fmt.Errorf("savecore: clone save: %w", err)
	}
	manifest, err := os.ReadFile(ManifestPath(src.Path))
	if err != nil {
		return nil, fmt.Errorf("savecore: source has no identity manifest: %w", err)
	}
	if err := os.WriteFile(ManifestPath(dst.Path), manifest, 0o644); err != nil {
		return nil, fmt.Errorf("savecore: clone manifest: %w", err)
	}
	return Fork(reg, dst, regionOf)
}

// ManifestPath is where the identity manifest of a save file lives.
func ManifestPath(savePath string) string { return savePath + ".manifest.json" }

// LoadManifest reads a manifest file.
func LoadManifest(path string) (*Manifest, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	m := newManifest()
	if err := json.Unmarshal(raw, m); err != nil {
		return nil, fmt.Errorf("savecore: malformed manifest: %w", err)
	}
	if m.Objects == nil || m.Nodes == nil {
		return nil, fmt.Errorf("savecore: manifest missing objects or nodes")
	}
	return m, nil
}

func (m *Manifest) save(path string) error {
	raw, err := json.MarshalIndent(m, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(path, raw, 0o644)
}

// Load restores the branch's world and reconnects stable identities
// to the freshly decoded pointers.
func (b *Branch) Load() (any, error) {
	frame, err := os.ReadFile(b.store.Path)
	if err != nil {
		return nil, fmt.Errorf("savecore: read save: %w", err)
	}
	payload, err := Unpack(frame)
	if err != nil {
		return nil, err
	}
	return b.reg.DecodeWithHook(payload, func(obj any, id int) {
		if sid, ok := b.manifest.Nodes[id]; ok {
			b.remember(obj, sid)
		}
	})
}

// Save publishes the branch's world and its identity manifest. New
// objects receive fresh stable ids; existing ones keep theirs, which
// is what lets a later merge tell "moved" apart from "deleted+added".
func (b *Branch) Save(root any) error {
	nodes := map[int]string{}
	_, err := b.reg.EncodeWithHook(root, func(obj any, id int) {
		sid := b.stableID(obj)
		nodes[id] = sid
		meta := b.manifest.Objects[sid]
		if meta == nil {
			meta = &ObjectMeta{Type: b.typeName(obj)}
			b.manifest.Objects[sid] = meta
		}
		if b.regionOf != nil {
			meta.Region = b.regionOf(obj)
		}
	})
	if err != nil {
		return err
	}
	b.manifest.Nodes = nodes
	if err := b.store.Save(root); err != nil {
		return err
	}
	return b.manifest.save(ManifestPath(b.store.Path))
}

// Manifest exposes the branch's identity table (read-only use).
func (b *Branch) Manifest() *Manifest { return b.manifest }

func (b *Branch) stableID(obj any) string {
	key := ptrKeyOf(obj)
	if sid, ok := b.ids[key]; ok {
		return sid
	}
	if b.manifest.Nonce == "" {
		b.manifest.Nonce = newNonce()
	}
	b.manifest.Seq++
	sid := fmt.Sprintf("obj-%s-%06d", b.manifest.Nonce, b.manifest.Seq)
	b.ids[key] = sid
	return sid
}

func (b *Branch) remember(obj any, sid string) {
	b.ids[ptrKeyOf(obj)] = sid
}

func (b *Branch) typeName(obj any) string {
	t := reflect.TypeOf(obj).Elem()
	for name, c := range b.reg.codecs {
		if reflect.TypeOf(c.New()).Elem() == t {
			return name
		}
	}
	return t.String()
}

func ptrKeyOf(obj any) ptrKey {
	rv := reflect.ValueOf(obj)
	return ptrKey{ptr: rv.Pointer(), typ: rv.Elem().Type()}
}

// newNonce returns a random hex tag so two branches forked from one
// save point never mint the same stable id for different objects.
func newNonce() string {
	var buf [4]byte
	if _, err := rand.Read(buf[:]); err != nil {
		panic(err)
	}
	return hex.EncodeToString(buf[:])
}

// StableIDs returns the sorted stable ids of a manifest, for
// deterministic diffs and reports.
func (m *Manifest) StableIDs() []string {
	out := make([]string, 0, len(m.Objects))
	for sid := range m.Objects {
		out = append(out, sid)
	}
	sort.Strings(out)
	return out
}
