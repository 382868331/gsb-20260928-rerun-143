package world_test

import (
	"errors"
	"os"
	"path/filepath"
	"testing"

	"savegame/savecore"
	"savegame/world"
)

func mustLoad(t *testing.T, reg *savecore.Registry, store *savecore.Store) *world.Player {
	t.Helper()
	v, err := store.Load()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	p, ok := v.(*world.Player)
	if !ok {
		t.Fatalf("Load returned %T, want *world.Player", v)
	}
	return p
}

// buildWorld constructs a graph with every required shape:
//   - a cycle: Player.Guild -> Guild -> Guild.Members -> Player
//   - a shared object: Leader and Rival both hold the same *Item
func buildWorld() *world.Player {
	relic := &world.Item{ID: "Aegis-7", Power: 42}
	leader := &world.Player{Name: "Ada", Level: 7, Inventory: []*world.Item{relic}}
	rival := &world.Player{Name: "Bo", Level: 3, Inventory: []*world.Item{relic}}
	guild := &world.Guild{Name: "Cartographers", Leader: leader, Members: []*world.Player{leader, rival}}
	leader.Guild = guild
	rival.Guild = guild
	relic.FavoriteOwner = leader
	return leader
}

func TestRoundTripPreservesIdentityAndCycles(t *testing.T) {
	store := savecore.NewStore(world.Registry(), filepath.Join(t.TempDir(), "save.sv"))
	if err := store.Save(buildWorld()); err != nil {
		t.Fatalf("Save: %v", err)
	}

	got := mustLoad(t, world.Registry(), store)

	if got.Name != "Ada" || got.Level != 7 {
		t.Fatalf("scalar fields lost: %+v", got)
	}
	g := got.Guild
	if g == nil {
		t.Fatal("Guild pointer lost")
	}
	if g.Name != "Cartographers" {
		t.Fatalf("guild name = %q", g.Name)
	}

	// Cycle: the leader as seen via guild membership is the same object.
	if g.Leader != got {
		t.Fatal("cycle broken: Guild.Leader is a copy of the root Player")
	}
	if len(g.Members) != 2 || g.Members[0] != got {
		t.Fatalf("membership identity lost: %+v", g.Members)
	}
	rival := g.Members[1]
	if rival.Guild != g {
		t.Fatal("second player points at a duplicate Guild")
	}

	// Shared object: the relic must be one object, not two copies.
	if len(got.Inventory) != 1 || len(rival.Inventory) != 1 {
		t.Fatal("inventories lost")
	}
	if got.Inventory[0] != rival.Inventory[0] {
		t.Fatal("shared Item was duplicated on restore")
	}
	if got.Inventory[0].ID != "Aegis-7" || got.Inventory[0].Power != 42 {
		t.Fatalf("item fields lost: %+v", got.Inventory[0])
	}
}

func TestUnknownTypeFailsExplicitly(t *testing.T) {
	reg := world.Registry()
	raw := []byte(`{"format":"savecore","formatVer":1,"root":{
		"$id":1,"$type":"world.Dragon","$ver":1,"data":{"Name":"Tiamat"}}}`)
	frame, err := savecore.Pack(raw)
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(t.TempDir(), "save.sv")
	if err := os.WriteFile(path, frame, 0o644); err != nil {
		t.Fatal(err)
	}
	_, err = savecore.NewStore(reg, path).Load()
	var ue *savecore.UnknownTypeError
	if !errors.As(err, &ue) {
		t.Fatalf("want *UnknownTypeError, got %v", err)
	}
	if ue.TypeName != "world.Dragon" {
		t.Fatalf("error type name = %q", ue.TypeName)
	}
}

// TestLoadsV1SaveAcrossVersions hand-writes a version-1 world (old
// field names, old levels) and checks that the current game restores
// it after per-type migrations, cycles included.
func TestLoadsV1SaveAcrossVersions(t *testing.T) {
	raw := []byte(`{
	  "format":"savecore","formatVer":1,
	  "root":{"$id":1,"$type":"world.Player","$ver":1,"data":{
	    "Nickname":"Veteran","XP":250,
	    "Guild":{"$id":2,"$type":"world.Guild","$ver":1,"data":{
	      "Title":"Old Guard",
	      "Leader":{"$ref":1},
	      "Members":[{"$ref":1}]
	    }},
	    "Inventory":[{"$id":3,"$type":"world.Item","$ver":1,"data":{"ID":"Horn","Power":9}}]
	  }}
	}`)
	frame, err := savecore.Pack(raw)
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(t.TempDir(), "save.sv")
	if err := os.WriteFile(path, frame, 0o644); err != nil {
		t.Fatal(err)
	}

	p := mustLoad(t, world.Registry(), savecore.NewStore(world.Registry(), path))
	if p.Name != "Veteran" {
		t.Fatalf("Nickname not migrated to Name: %q", p.Name)
	}
	if p.Level != 3 {
		t.Fatalf("XP 250 should migrate to Level 3, got %d", p.Level)
	}
	if p.Guild == nil || p.Guild.Name != "Old Guard" {
		t.Fatalf("guild v1 not migrated: %+v", p.Guild)
	}
	if p.Guild.Leader != p {
		t.Fatal("v1 cycle not preserved after migration")
	}
	if inv := p.Inventory; len(inv) != 1 || inv[0].ID != "Horn" || inv[0].Power != 9 {
		t.Fatalf("v1 item not restored: %+v", inv)
	}
}

func TestFutureVersionFails(t *testing.T) {
	raw := []byte(`{"format":"savecore","formatVer":1,"root":{
		"$id":1,"$type":"world.Player","$ver":99,"data":{"Name":"Z"}}}`)
	frame, _ := savecore.Pack(raw)
	path := filepath.Join(t.TempDir(), "save.sv")
	os.WriteFile(path, frame, 0o644)

	_, err := savecore.NewStore(world.Registry(), path).Load()
	var me *savecore.MigrationError
	if !errors.As(err, &me) {
		t.Fatalf("want *MigrationError for future version, got %v", err)
	}
}

// TestInterruptedSaveKeepsPreviousVersion: save A, then simulate a
// crash while writing B (temp file left behind, never renamed). The
// previous complete save must still load intact.
func TestInterruptedSaveKeepsPreviousVersion(t *testing.T) {
	dir := t.TempDir()
	store := savecore.NewStore(world.Registry(), filepath.Join(dir, "save.sv"))

	first := buildWorld()
	if err := store.Save(first); err != nil {
		t.Fatalf("Save A: %v", err)
	}

	// Simulate the new save dying halfway: create the temp path the
	// store would use with partial, garbage contents, and never rename.
	if err := os.WriteFile(filepath.Join(dir, "save.sv.tmp"),
		[]byte("SVCR\x00\x00"), 0o644); err != nil {
		t.Fatal(err)
	}

	got := mustLoad(t, world.Registry(), store)
	if got.Name != "Ada" {
		t.Fatalf("previous version lost after interrupted write: %q", got.Name)
	}
	if got.Guild.Leader != got || got.Inventory[0] != got.Guild.Members[1].Inventory[0] {
		t.Fatal("identity relationships lost in surviving version")
	}

	// After a later successful save, stale temp files are irrelevant.
	second := &world.Player{Name: "Cleo", Level: 1}
	if err := store.Save(second); err != nil {
		t.Fatalf("Save B: %v", err)
	}
	got = mustLoad(t, world.Registry(), store)
	if got.Name != "Cleo" {
		t.Fatalf("new version not published: %q", got.Name)
	}
}

// TestHalfFileAndCorruptionDetected: truncation, byte flips and bad
// magic must all surface as CorruptionError, never as partial data.
func TestHalfFileAndCorruptionDetected(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "save.sv")
	store := savecore.NewStore(world.Registry(), path)
	if err := store.Save(buildWorld()); err != nil {
		t.Fatal(err)
	}
	good, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}

	cases := map[string][]byte{
		"truncated halfway": good[:len(good)/2],
		"truncated header":  good[:6],
		"flipped payload":   flipByte(good, len(good)-3),
		"flipped length":    flipByte(good, 5),
		"bad magic":         append([]byte("XXXX"), good[4:]...),
	}
	for name, frame := range cases {
		t.Run(name, func(t *testing.T) {
			p := filepath.Join(t.TempDir(), "save.sv")
			if err := os.WriteFile(p, frame, 0o644); err != nil {
				t.Fatal(err)
			}
			_, err := savecore.NewStore(world.Registry(), p).Load()
			var ce *savecore.CorruptionError
			if !errors.As(err, &ce) {
				t.Fatalf("want CorruptionError, got %v", err)
			}
		})
	}
}

// TestMalformedJSONDoesNotExecuteAnything: unknown fields on a
// current-version node fail rather than being silently ignored, and
// interface-based gadgetry is rejected outright.
func TestMissingMigrationStepFails(t *testing.T) {
	reg := savecore.NewRegistry()
	type Widget struct{ Label string }
	reg.Register(savecore.Codec{Name: "test.Widget", Version: 3, New: func() any { return &Widget{} }})
	raw := []byte(`{"format":"savecore","formatVer":1,"root":{
		"$id":1,"$type":"test.Widget","$ver":1,"data":{"Label":"x"}}}`)
	frame, _ := savecore.Pack(raw)
	path := filepath.Join(t.TempDir(), "save.sv")
	os.WriteFile(path, frame, 0o644)
	_, err := savecore.NewStore(reg, path).Load()
	var me *savecore.MigrationError
	if !errors.As(err, &me) {
		t.Fatalf("want MigrationError for missing steps, got %v", err)
	}
}

func flipByte(b []byte, i int) []byte {
	out := append([]byte(nil), b...)
	out[i] ^= 0xFF
	return out
}
