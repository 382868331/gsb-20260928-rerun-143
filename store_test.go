package gamesave_test

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"gamesave"
)

// --- minimal in-package fictional object model (keeps the core independent) ---

type Node struct {
	ID    uint64  `json:"-"`
	Tag   string  `json:"tag"`
	Next  *Node   `json:"next"`
	Peers []*Node `json:"peers"`
	Owner *Node   `json:"owner"`
}

func nodeCounter() func() *Node {
	var n uint64
	return func() *Node { n++; return &Node{ID: n} }
}

func newTestRegistry(t *testing.T) *gamesave.Registry {
	t.Helper()
	reg := gamesave.NewRegistry()
	counter := nodeCounter()
	if err := reg.Register("node", 1, func() gamesave.Object { o := counter(); return o }); err != nil {
		t.Fatal(err)
	}
	return reg
}

func (n *Node) ObjectID() uint64 { return n.ID }

func TestSharedAndCycleIdentity(t *testing.T) {
	reg := newTestRegistry(t)
	store := gamesave.NewStore(reg)

	counter := nodeCounter()
	a := counter()
	a.Tag = "a"
	b := counter()
	b.Tag = "b"
	c := counter()
	c.Tag = "c"
	a.Next = b
	b.Next = a                 // direct cycle
	a.Peers = []*Node{b, c, b} // b shared across fields and repeated in slice
	c.Owner = a

	path := filepath.Join(t.TempDir(), "save.json")
	if err := store.Save(path, map[string]any{"a": a, "c": c}); err != nil {
		t.Fatal(err)
	}

	var ra, rc *Node
	if err := store.Load(path, map[string]any{"a": &ra, "c": &rc}); err != nil {
		t.Fatal(err)
	}
	if ra.Tag != "a" || rc.Tag != "c" {
		t.Fatalf("tags wrong: %q %q", ra.Tag, rc.Tag)
	}
	if ra.Next != ra.Next.Next.Next {
		t.Fatal("direct cycle not preserved: a->b->a must return to same object")
	}
	if ra.Next.Tag != "b" {
		t.Fatalf("expected b, got %q", ra.Next.Tag)
	}
	if ra.Peers[0] != ra.Peers[2] {
		t.Fatal("repeated shared object was duplicated")
	}
	if ra.Peers[0] != ra.Next {
		t.Fatal("shared object across fields was duplicated")
	}
	if rc.Owner != ra {
		t.Fatal("object shared with a root was duplicated")
	}
}

func TestUnknownTypeFails(t *testing.T) {
	reg := newTestRegistry(t)
	store := gamesave.NewStore(reg)
	path := filepath.Join(t.TempDir(), "save.json")

	payload := `{
	  "format":"gamesave","format_version":1,
	  "objects":[{"id":0,"type":"dragon","version":1,"fields":{"tag":"x"}}],
	  "roots":{"r":{"kind":"ref","id":0}}
}`
	if err := os.WriteFile(path, []byte(payload), 0o600); err != nil {
		t.Fatal(err)
	}
	var r *Node
	err := store.Load(path, map[string]any{"r": &r})
	var unknown *gamesave.UnknownTypeError
	if !errors.As(err, &unknown) {
		t.Fatalf("expected *UnknownTypeError, got %v", err)
	}
	if !strings.Contains(err.Error(), "dragon") {
		t.Fatalf("error should name the unknown type: %v", err)
	}
}

func TestUnknownVersionFails(t *testing.T) {
	reg := newTestRegistry(t)
	store := gamesave.NewStore(reg)
	path := filepath.Join(t.TempDir(), "save.json")
	payload := `{
	  "format":"gamesave","format_version":1,
	  "objects":[{"id":0,"type":"node","version":9,"fields":{"tag":"x"}}],
	  "roots":{"r":{"kind":"ref","id":0}}
}`
	if err := os.WriteFile(path, []byte(payload), 0o600); err != nil {
		t.Fatal(err)
	}
	var r *Node
	err := store.Load(path, map[string]any{"r": &r})
	var unknown *gamesave.UnknownTypeError
	if !errors.As(err, &unknown) {
		t.Fatalf("expected *UnknownTypeError for missing version, got %v", err)
	}
}

func TestSaveFailureKeepsPreviousVersion(t *testing.T) {
	reg := newTestRegistry(t)
	store := gamesave.NewStore(reg)
	path := filepath.Join(t.TempDir(), "save.json")

	counter := nodeCounter()
	first := counter()
	first.Tag = "v1-good"
	if err := store.Save(path, map[string]any{"r": first}); err != nil {
		t.Fatal(err)
	}
	original, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}

	// Attempt a replacement that dies halfway through the write.
	second := counter()
	second.Tag = "v2-should-not-land"
	err = store.Save(path, map[string]any{"r": second}, gamesave.WithFaultyWriter(7))
	if err == nil {
		t.Fatal("expected simulated write failure")
	}

	after, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("previous save vanished after failed write: %v", err)
	}
	if string(after) != string(original) {
		t.Fatal("previous complete save was modified by the failed write")
	}
	// And the old file still loads.
	var loaded *Node
	if err := store.Load(path, map[string]any{"r": &loaded}); err != nil {
		t.Fatalf("previous save no longer loadable: %v", err)
	}
	if loaded.Tag != "v1-good" {
		t.Fatalf("wrong version restored: %q", loaded.Tag)
	}
	// No temp leftovers in the directory.
	entries, _ := os.ReadDir(filepath.Dir(path))
	for _, e := range entries {
		if strings.Contains(e.Name(), ".tmp-") {
			t.Fatalf("temporary file left behind: %s", e.Name())
		}
	}
}

func TestTruncatedFileIsCorruptAndUnchanged(t *testing.T) {
	reg := newTestRegistry(t)
	store := gamesave.NewStore(reg)
	path := filepath.Join(t.TempDir(), "save.json")
	counter := nodeCounter()
	good := counter()
	good.Tag = "complete"
	if err := store.Save(path, map[string]any{"r": good}); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	// Simulate a half-written file (e.g. power loss with a non-atomic writer).
	if err := os.WriteFile(path, data[:len(data)/2], 0o600); err != nil {
		t.Fatal(err)
	}
	var loaded *Node
	err = store.Load(path, map[string]any{"r": &loaded})
	if err == nil {
		t.Fatal("expected error on truncated save")
	}
	if !gamesave.IsCorrupt(err) {
		t.Fatalf("expected corrupt-save error, got %T: %v", err, err)
	}
}

func TestNoCodeExecutionFromSave(t *testing.T) {
	// Interface-typed fields can only receive plain JSON data. A payload that
	// tries to smuggle a "type/function" description must be rejected rather
	// than instantiating anything the game did not register.
	reg := newTestRegistry(t)
	store := gamesave.NewStore(reg)
	path := filepath.Join(t.TempDir(), "save.json")
	payload := `{
	  "format":"gamesave","format_version":1,
	  "objects":[{"id":0,"type":"node","version":1,"fields":{"tag":{"kind":"ref","id":1}}}],
	  "roots":{"r":{"kind":"ref","id":0}}
}`
	if err := os.WriteFile(path, []byte(payload), 0o600); err != nil {
		t.Fatal(err)
	}
	var r *Node
	if err := store.Load(path, map[string]any{"r": &r}); err == nil {
		t.Fatal("expected type error: ref marker cannot populate string field")
	}
}

func TestDanglingAndDuplicateRefsRejected(t *testing.T) {
	reg := newTestRegistry(t)
	store := gamesave.NewStore(reg)
	path := filepath.Join(t.TempDir(), "save.json")
	payload := `{
	  "format":"gamesave","format_version":1,
	  "objects":[
	    {"id":0,"type":"node","version":1,"fields":{"tag":"a","next":{"kind":"ref","id":5}}}
	  ],
	  "roots":{"r":{"kind":"ref","id":0}}
}`
	if err := os.WriteFile(path, []byte(payload), 0o600); err != nil {
		t.Fatal(err)
	}
	var r *Node
	if err := store.Load(path, map[string]any{"r": &r}); err == nil {
		t.Fatal("expected out-of-range reference to be rejected")
	}
}
