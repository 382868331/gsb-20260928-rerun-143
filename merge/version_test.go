package merge_test

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"savegame/merge"
	"savegame/savecore"
	"savegame/world"
)

// smallBaseWorld is a one-region world without uid fields, so the
// version-mixed tests match objects by their meaningful names.
func smallBaseWorld() *world.World {
	relic := &world.Item{ID: "Relic", Power: 10}
	ada := &world.Player{Name: "Ada", Level: 5, Inventory: []*world.Item{relic}}
	return &world.World{
		Name: "realm",
		Regions: []*world.Region{
			{Name: "north", Players: []*world.Player{ada}, Items: []*world.Item{relic}},
		},
	}
}

func writeRawSave(t *testing.T, path, payload string) {
	t.Helper()
	frame, err := savecore.Pack([]byte(payload))
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, frame, 0o644); err != nil {
		t.Fatal(err)
	}
}

// The left candidate was written by an older game version: players
// are v1 (Nickname/XP), everything else identical in meaning to the
// base except Ada's XP, which maps to level 9 after migration.
const leftV1 = `{"format":"savecore","formatVer":1,"root":
{"$id":1,"$type":"world.World","$ver":1,"data":{"Name":"realm","Regions":[
 {"$id":2,"$type":"world.Region","$ver":1,"data":{"Name":"north",
  "Players":[{"$id":3,"$type":"world.Player","$ver":1,"data":{
    "Nickname":"Ada","XP":850,"Guild":null,
    "Inventory":[{"$id":4,"$type":"world.Item","$ver":1,"data":{"ID":"Relic","Power":10}}]}}],
  "Guilds":null,
  "Items":[{"$ref":4}]}}]}}}`

func TestCompareAcrossDataVersions(t *testing.T) {
	dir := t.TempDir()
	base := saveWorld(t, dir, "base.sv", smallBaseWorld())
	left := filepath.Join(dir, "left.sv")
	writeRawSave(t, left, leftV1)
	// The right candidate is the base unchanged.
	right := saveWorld(t, dir, "right.sv", smallBaseWorld())

	s := newSession(t, base, left, right)
	north := regionStatus(t, s, "north")
	if north.Conflict {
		t.Fatalf("only the left changed anything: %+v", north.Conflicts)
	}
	// Exactly one real change: Ada's level. The version gap (v1
	// Nickname/XP vs v2 Name/Level) must not show up as noise.
	if len(north.Left) != 1 || north.Left[0].Kind != merge.Modified ||
		north.Left[0].Key != "world.Player|Ada" {
		t.Fatalf("left changes = %+v, want one modified Ada", north.Left)
	}

	if adopted := s.AutoAdopt(); len(adopted) != 1 || adopted[0] != "north" {
		t.Fatalf("AutoAdopt = %v", adopted)
	}
	out := filepath.Join(dir, "merged.sv")
	report, err := s.Finalize(out)
	if err != nil {
		t.Fatal(err)
	}
	if !report.Complete {
		t.Fatalf("pending: %+v", report.Pending)
	}
	w := loadWorld(t, out)
	ada := w.Regions[0].Players[0]
	if ada.Name != "Ada" || ada.Level != 9 {
		t.Fatalf("merged Ada = %+v, want name Ada level 9", ada)
	}
	if len(ada.Inventory) != 1 || ada.Inventory[0].ID != "Relic" {
		t.Fatalf("inventory lost across versions: %+v", ada.Inventory)
	}
}

// The left candidate carries an item at a data version this program
// cannot migrate. The comparison falls back to the raw payload, and
// the merged save keeps the original bytes untouched.
const leftFutureItem = `{"format":"savecore","formatVer":1,"root":
{"$id":1,"$type":"world.World","$ver":1,"data":{"Name":"realm","Regions":[
 {"$id":2,"$type":"world.Region","$ver":1,"data":{"Name":"north",
  "Players":[{"$id":3,"$type":"world.Player","$ver":2,"data":{
    "UID":"","Name":"Ada","Level":5,"Guild":null,
    "Inventory":[{"$id":4,"$type":"world.Item","$ver":99,"data":{"ID":"Relic","Power":11}}]}}],
  "Guilds":null,
  "Items":[{"$ref":4}]}}]}}}`

func TestUnconvertibleNodeKeepsRawData(t *testing.T) {
	dir := t.TempDir()
	base := saveWorld(t, dir, "base.sv", smallBaseWorld())
	left := filepath.Join(dir, "left.sv")
	writeRawSave(t, left, leftFutureItem)
	right := saveWorld(t, dir, "right.sv", smallBaseWorld())

	s := newSession(t, base, left, right)
	north := regionStatus(t, s, "north")
	// The raw comparison still sees the one real change: Power 11.
	if len(north.Left) != 1 || north.Left[0].Kind != merge.Modified ||
		north.Left[0].Key != "world.Item|Relic" {
		t.Fatalf("left changes = %+v, want one modified Relic", north.Left)
	}
	if err := s.Adopt("north", merge.SideLeft); err != nil {
		t.Fatal(err)
	}
	out := filepath.Join(dir, "merged.sv")
	if _, err := s.Finalize(out); err != nil {
		t.Fatal(err)
	}
	frame, err := os.ReadFile(out)
	if err != nil {
		t.Fatal(err)
	}
	payload, err := savecore.Unpack(frame)
	if err != nil {
		t.Fatal(err)
	}
	// The unconvertible node survives the merge with its original
	// version number and data, not a lossy conversion.
	if !strings.Contains(string(payload), "\"$ver\":99") {
		t.Fatal("future-version node was rewritten")
	}
	if !strings.Contains(string(payload), "\"Power\":11") {
		t.Fatal("raw payload of unconvertible node was not preserved")
	}
}
