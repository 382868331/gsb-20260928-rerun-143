package world_test

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"savegame/savecore"
	"savegame/world"
)

// buildRegionWorld builds a two-region world with a shared relic
// (carried by players in both regions) and a cross-region reference
// (the north guild counts a south player among its members).
func buildRegionWorld() *world.World {
	relic := &world.Item{ID: "Aegis-7", Power: 42}
	ada := &world.Player{Name: "Ada", Level: 7, Inventory: []*world.Item{relic}}
	bo := &world.Player{Name: "Bo", Level: 3, Inventory: []*world.Item{relic}}
	carto := &world.Guild{Name: "Cartographers", Leader: ada, Members: []*world.Player{ada, bo}}
	compass := &world.Guild{Name: "Compass", Leader: bo, Members: []*world.Player{bo}}
	ada.Guild = carto
	bo.Guild = compass
	return &world.World{Regions: []*world.Region{
		{Name: "north", Players: []*world.Player{ada}, Guilds: []*world.Guild{carto}, Items: []*world.Item{relic}},
		{Name: "south", Players: []*world.Player{bo}, Guilds: []*world.Guild{compass}},
	}}
}

// saveBase writes the common save point both candidates branch from.
func saveBase(t *testing.T, dir string) *savecore.Store {
	t.Helper()
	reg := world.Registry()
	w := buildRegionWorld()
	store := savecore.NewStore(reg, filepath.Join(dir, "base.sv"))
	br, err := savecore.Fork(reg, store, w.RegionOf)
	if err != nil {
		t.Fatalf("Fork base: %v", err)
	}
	if err := br.Save(w); err != nil {
		t.Fatalf("Save base: %v", err)
	}
	return store
}

// editBranch forks the base save into path and applies edit.
func editBranch(t *testing.T, baseStore *savecore.Store, path string, edit func(*world.World)) *savecore.Store {
	t.Helper()
	reg := world.Registry()
	store := savecore.NewStore(reg, path)
	var w *world.World
	br, err := savecore.CloneBranch(reg, baseStore, store, func(obj any) string { return w.RegionOf(obj) })
	if err != nil {
		t.Fatalf("Fork: %v", err)
	}
	v, err := br.Load()
	if err != nil {
		t.Fatalf("branch Load: %v", err)
	}
	w = v.(*world.World)
	edit(w)
	if err := br.Save(w); err != nil {
		t.Fatalf("branch Save: %v", err)
	}
	return store
}

func loadWorld(t *testing.T, store *savecore.Store) *world.World {
	t.Helper()
	v, err := store.Load()
	if err != nil {
		t.Fatalf("Load %s: %v", store.Path, err)
	}
	w, ok := v.(*world.World)
	if !ok {
		t.Fatalf("Load returned %T, want *world.World", v)
	}
	return w
}

func newSession(t *testing.T, base, a, b, out *savecore.Store) *savecore.MergeSession {
	t.Helper()
	m, err := savecore.NewMergeSession(world.Registry(), base, a, b, out)
	if err != nil {
		t.Fatalf("NewMergeSession: %v", err)
	}
	return m
}

func adoptAll(t *testing.T, m *savecore.MergeSession) {
	t.Helper()
	for _, region := range m.CleanRegions() {
		if err := m.AdoptRegion(region); err != nil {
			t.Fatalf("AdoptRegion(%q): %v", region, err)
		}
	}
}

func diffByType(m *savecore.MergeSession, typeName string) []*savecore.ObjectDiff {
	var out []*savecore.ObjectDiff
	for _, od := range m.Diff().Objects {
		if od.Type == typeName {
			out = append(out, od)
		}
	}
	return out
}

func TestMergeCleanRegionsKeepsSharedAndCrossRegionIdentity(t *testing.T) {
	dir := t.TempDir()
	base := saveBase(t, dir)
	a := editBranch(t, base, filepath.Join(dir, "a.sv"), func(w *world.World) {
		w.FindRegion("north").Players[0].Level = 99
	})
	b := editBranch(t, base, filepath.Join(dir, "b.sv"), func(w *world.World) {
		w.FindRegion("south").Guilds[0].Name = "New Compass"
	})
	out := savecore.NewStore(world.Registry(), filepath.Join(dir, "merged.sv"))

	m := newSession(t, base, a, b, out)

	// The relic is one shared object carried across regions.
	var relic *savecore.ObjectDiff
	for _, od := range diffByType(m, "world.Item") {
		relic = od
	}
	if relic == nil || !relic.Shared {
		t.Fatal("diff must flag the shared relic as a shared object")
	}

	clean := m.CleanRegions()
	if len(clean) != 3 { // "", north, south
		t.Fatalf("CleanRegions = %v, want 3 clean regions", clean)
	}
	adoptAll(t, m)

	report, err := m.Finalize()
	if err != nil {
		t.Fatalf("Finalize: %v", err)
	}
	if !report.Complete || len(report.PendingObjects) != 0 || len(report.PendingReferences) != 0 {
		t.Fatalf("clean merge reported incomplete: %+v", report)
	}

	merged := loadWorld(t, out)
	north, south := merged.FindRegion("north"), merged.FindRegion("south")
	ada, bo := north.Players[0], south.Players[0]
	if ada.Level != 99 {
		t.Fatalf("north edit lost: ada.Level = %d", ada.Level)
	}
	if south.Guilds[0].Name != "New Compass" {
		t.Fatalf("south edit lost: %q", south.Guilds[0].Name)
	}
	// Shared relic: still exactly one object across regions.
	if ada.Inventory[0] != bo.Inventory[0] {
		t.Fatal("merge duplicated the shared relic")
	}
	// Cross-region reference: the north guild still points at the
	// identical south player, not a copy or a dangling target.
	carto := north.Guilds[0]
	if carto.Members[1] != bo {
		t.Fatal("cross-region guild membership broke during merge")
	}

	// Original candidates are untouched and still loadable.
	wa := loadWorld(t, a)
	if wa.FindRegion("north").Players[0].Level != 99 ||
		wa.FindRegion("south").Guilds[0].Name != "Compass" {
		t.Fatal("candidate A was modified by the merge")
	}
	wb := loadWorld(t, b)
	if wb.FindRegion("south").Guilds[0].Name != "New Compass" ||
		wb.FindRegion("north").Players[0].Level != 7 {
		t.Fatal("candidate B was modified by the merge")
	}
}

func TestCrossRegionMoveIsNotDeletePlusAdd(t *testing.T) {
	dir := t.TempDir()
	base := saveBase(t, dir)
	a := editBranch(t, base, filepath.Join(dir, "a.sv"), func(w *world.World) {
		north, south := w.FindRegion("north"), w.FindRegion("south")
		bo := south.Players[0]
		south.Players = nil
		north.Players = append(north.Players, bo)
	})
	b := editBranch(t, base, filepath.Join(dir, "b.sv"), func(w *world.World) {})
	out := savecore.NewStore(world.Registry(), filepath.Join(dir, "merged.sv"))

	m := newSession(t, base, a, b, out)
	var boRow *savecore.ObjectDiff
	for _, od := range diffByType(m, "world.Player") {
		if od.KindA != savecore.Unchanged || od.KindB != savecore.Unchanged {
			boRow = od
		}
	}
	if boRow == nil {
		t.Fatal("diff shows no changed player")
	}
	if boRow.KindA != savecore.Moved {
		t.Fatalf("cross-region move reported as %q, want %q", boRow.KindA, savecore.Moved)
	}
	if boRow.RegionA != "north" || boRow.RegionB != "south" {
		t.Fatalf("move endpoints wrong: A=%q B=%q", boRow.RegionA, boRow.RegionB)
	}

	adoptAll(t, m)
	report, err := m.Finalize()
	if err != nil {
		t.Fatalf("Finalize: %v", err)
	}
	if !report.Complete {
		t.Fatalf("move merge incomplete: %+v", report)
	}
	merged := loadWorld(t, out)
	north := merged.FindRegion("north")
	if len(north.Players) != 2 {
		t.Fatalf("moved player not in north: %d players", len(north.Players))
	}
	bo := north.Players[1]
	if bo.Name != "Bo" {
		t.Fatalf("moved player = %q", bo.Name)
	}
	// The guild reference followed the object; no duplicate, no dangling.
	if north.Guilds[0].Members[1] != bo {
		t.Fatal("guild points at a duplicate of the moved player")
	}
	if len(merged.FindRegion("south").Players) != 0 {
		t.Fatal("moved player still listed in south")
	}
}

func TestDeleteVsModifyConflictAndExplicitPending(t *testing.T) {
	dir := t.TempDir()
	base := saveBase(t, dir)
	// A deletes the shared relic (removing every reference to it).
	a := editBranch(t, base, filepath.Join(dir, "a.sv"), func(w *world.World) {
		north := w.FindRegion("north")
		north.Items = nil
		north.Players[0].Inventory = nil
		w.FindRegion("south").Players[0].Inventory = nil
	})
	// B powers the relic up and levels its south carrier.
	b := editBranch(t, base, filepath.Join(dir, "b.sv"), func(w *world.World) {
		w.FindRegion("north").Items[0].Power = 100
		w.FindRegion("south").Players[0].Level = 8
	})
	out := savecore.NewStore(world.Registry(), filepath.Join(dir, "merged.sv"))

	m := newSession(t, base, a, b, out)
	relic := diffByType(m, "world.Item")[0]
	if relic.Conflict != "delete-modify" {
		t.Fatalf("conflict = %q, want delete-modify", relic.Conflict)
	}
	if err := m.AdoptRegion("north"); err == nil {
		t.Fatal("adopting a conflicted region must fail")
	}

	// Finalizing with open conflicts: the official save must say so.
	report, err := m.Finalize()
	if err != nil {
		t.Fatalf("Finalize: %v", err)
	}
	if report.Complete {
		t.Fatal("merge with open conflict claims to be complete")
	}
	var relicPending *savecore.PendingObject
	for i := range report.PendingObjects {
		if report.PendingObjects[i].StableID == relic.StableID {
			relicPending = &report.PendingObjects[i]
		}
	}
	if relicPending == nil || relicPending.Reason != "delete-modify" || relicPending.Kept != "base" {
		t.Fatalf("relic pending entry wrong: %+v", relicPending)
	}
	merged := loadWorld(t, out)
	if got := merged.FindRegion("north").Items[0].Power; got != 42 {
		t.Fatalf("unadopted conflict should keep base value 42, got %d", got)
	}

	// Operator keeps B's relic and B's carrier; the merge completes.
	if err := m.Resolve(relic.StableID, savecore.SideB); err != nil {
		t.Fatalf("Resolve relic: %v", err)
	}
	var boRow *savecore.ObjectDiff
	for _, od := range diffByType(m, "world.Player") {
		if od.Conflict == "modify-modify" {
			boRow = od
		}
	}
	if boRow == nil {
		t.Fatal("south player should be a modify-modify conflict")
	}
	if err := m.Resolve(boRow.StableID, savecore.SideB); err != nil {
		t.Fatalf("Resolve bo: %v", err)
	}
	adoptAll(t, m)
	report, err = m.Finalize()
	if err != nil {
		t.Fatalf("Finalize after resolve: %v", err)
	}
	if !report.Complete {
		t.Fatalf("resolved merge still incomplete: %+v", report)
	}
	merged = loadWorld(t, out)
	bo := merged.FindRegion("south").Players[0]
	if bo.Level != 8 {
		t.Fatalf("resolved bo level = %d, want 8", bo.Level)
	}
	if len(bo.Inventory) != 1 || bo.Inventory[0].Power != 100 {
		t.Fatalf("resolved relic = %+v, want power 100", bo.Inventory)
	}
}

func TestDeletedCrossRegionObjectStaysDeleted(t *testing.T) {
	dir := t.TempDir()
	base := saveBase(t, dir)
	// A deletes the north guild; B edits an unrelated south player.
	a := editBranch(t, base, filepath.Join(dir, "a.sv"), func(w *world.World) {
		north := w.FindRegion("north")
		north.Guilds = nil
		north.Players[0].Guild = nil
	})
	b := editBranch(t, base, filepath.Join(dir, "b.sv"), func(w *world.World) {
		w.FindRegion("south").Players[0].Level = 8
	})
	out := savecore.NewStore(world.Registry(), filepath.Join(dir, "merged.sv"))

	m := newSession(t, base, a, b, out)
	adoptAll(t, m)
	report, err := m.Finalize()
	if err != nil {
		t.Fatalf("Finalize: %v", err)
	}
	if !report.Complete {
		t.Fatalf("clean delete should complete: %+v", report)
	}
	merged := loadWorld(t, out)
	if len(merged.FindRegion("north").Guilds) != 0 {
		t.Fatal("deleted guild survived the merge")
	}
	if got := merged.FindRegion("south").Players[0].Level; got != 8 {
		t.Fatalf("B's edit lost: level = %d", got)
	}
}

func TestKeptReferenceToDeletedObjectIsRescued(t *testing.T) {
	dir := t.TempDir()
	base := saveBase(t, dir)
	// A deletes the shared relic (removing every reference to it).
	a := editBranch(t, base, filepath.Join(dir, "a.sv"), func(w *world.World) {
		north := w.FindRegion("north")
		north.Items = nil
		north.Players[0].Inventory = nil
		w.FindRegion("south").Players[0].Inventory = nil
	})
	// B only levels the south player; B's player still carries the relic.
	b := editBranch(t, base, filepath.Join(dir, "b.sv"), func(w *world.World) {
		w.FindRegion("south").Players[0].Level = 8
	})
	out := savecore.NewStore(world.Registry(), filepath.Join(dir, "merged.sv"))

	m := newSession(t, base, a, b, out)
	adoptAll(t, m)
	report, err := m.Finalize()
	if err != nil {
		t.Fatalf("Finalize: %v", err)
	}
	// The relic deletion won (B never touched it), but the south
	// player is a modify-modify conflict kept at its base version,
	// whose inventory still references the deleted relic: the base
	// copy is rescued and the decision is reported, not hidden.
	if report.Complete {
		t.Fatal("merge with a rescued reference must not claim completeness")
	}
	var relicRescued bool
	for _, pr := range report.PendingReferences {
		if pr.ToType == "world.Item" && pr.Rescued == "base" {
			relicRescued = true
		}
	}
	if !relicRescued {
		t.Fatalf("relic rescue not reported: %+v", report.PendingReferences)
	}
	// The merged save loads with no dangling reference.
	merged := loadWorld(t, out)
	bo := merged.FindRegion("south").Players[0]
	if bo.Level != 3 {
		t.Fatalf("pending player should keep base level 3, got %d", bo.Level)
	}
	if len(bo.Inventory) != 1 || bo.Inventory[0].ID != "Aegis-7" {
		t.Fatalf("dangling inventory after merge: %+v", bo.Inventory)
	}
}

// findNode walks a nested save payload and returns the first object
// node matching pred.
func findNode(v any, pred func(map[string]any) bool) map[string]any {
	switch t := v.(type) {
	case map[string]any:
		if _, isNode := t["$id"]; isNode {
			if pred(t) {
				return t
			}
		}
		for _, x := range t {
			if n := findNode(x, pred); n != nil {
				return n
			}
		}
	case []any:
		for _, x := range t {
			if n := findNode(x, pred); n != nil {
				return n
			}
		}
	}
	return nil
}

func readPayload(t *testing.T, path string) map[string]any {
	t.Helper()
	frame, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	payload, err := savecore.Unpack(frame)
	if err != nil {
		t.Fatal(err)
	}
	var env map[string]any
	if err := json.Unmarshal(payload, &env); err != nil {
		t.Fatal(err)
	}
	return env
}

// TestVersionedCandidatesCompareByMeaning: a candidate written with
// an older data version is compared by what its objects mean (after
// migration), and a node with no migration path keeps its original
// data through the merge.
func TestVersionedCandidatesCompareByMeaning(t *testing.T) {
	dir := t.TempDir()
	base := saveBase(t, dir)

	// Candidate A: a plain current-version edit.
	a := editBranch(t, base, filepath.Join(dir, "a.sv"), func(w *world.World) {
		w.FindRegion("south").Guilds[0].Name = "New Compass"
	})

	// Candidate B: hand-rewritten from the base file. Ada's player
	// node is downgraded to the v1 shape (Nickname/XP) that migrates
	// to exactly her current meaning, and the relic is stamped with a
	// future version no migration path exists for.
	bPath := filepath.Join(dir, "b.sv")
	env := readPayload(t, base.Path)
	ada := findNode(env["root"], func(n map[string]any) bool {
		d, _ := n["data"].(map[string]any)
		return n["$type"] == "world.Player" && d["Name"] == "Ada"
	})
	if ada == nil {
		t.Fatal("ada node not found in base payload")
	}
	data := ada["data"].(map[string]any)
	ada["$ver"] = float64(1)
	ada["data"] = map[string]any{
		"Nickname":  "Ada",
		"XP":        float64(600), // migrates to Level 7, her base value
		"Guild":     data["Guild"],
		"Inventory": data["Inventory"],
	}
	relic := findNode(env["root"], func(n map[string]any) bool {
		return n["$type"] == "world.Item"
	})
	if relic == nil {
		t.Fatal("relic node not found in base payload")
	}
	relic["$ver"] = float64(99) // future version: cannot be converted
	payload, err := json.Marshal(env)
	if err != nil {
		t.Fatal(err)
	}
	frame, err := savecore.Pack(payload)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(bPath, frame, 0o644); err != nil {
		t.Fatal(err)
	}
	manifest, err := os.ReadFile(savecore.ManifestPath(base.Path))
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(savecore.ManifestPath(bPath), manifest, 0o644); err != nil {
		t.Fatal(err)
	}
	b := savecore.NewStore(world.Registry(), bPath)
	out := savecore.NewStore(world.Registry(), filepath.Join(dir, "merged.sv"))

	m := newSession(t, base, a, b, out)

	// Ada compares unchanged: her v1 form means exactly the base.
	var adaRow *savecore.ObjectDiff
	for _, od := range diffByType(m, "world.Player") {
		if od.KindA == savecore.Unchanged {
			adaRow = od
		}
	}
	if adaRow == nil {
		t.Fatal("no unchanged player row; v1 form should compare by meaning")
	}
	if adaRow.KindB != savecore.Unchanged {
		t.Fatalf("ada changed on B? KindB = %q", adaRow.KindB)
	}

	adoptAll(t, m)
	report, err := m.Finalize()
	if err != nil {
		t.Fatalf("Finalize: %v", err)
	}
	if !report.Complete {
		t.Fatalf("merge incomplete: %+v", report)
	}

	// The unconvertible relic kept its original version and data.
	merged := readPayload(t, out.Path)
	got := findNode(merged["root"], func(n map[string]any) bool {
		return n["$type"] == "world.Item"
	})
	if got == nil {
		t.Fatal("relic missing from merged save")
	}
	if got["$ver"] != float64(99) {
		t.Fatalf("unconvertible node version rewritten: %v", got["$ver"])
	}
	d := got["data"].(map[string]any)
	if d["ID"] != "Aegis-7" || d["Power"] != float64(42) {
		t.Fatalf("unconvertible node data not preserved: %v", d)
	}
}

// TestMergeResumesAfterInterruption: decisions survive a crash; the
// merge is resumed from the state file and completes.
func TestMergeResumesAfterInterruption(t *testing.T) {
	dir := t.TempDir()
	base := saveBase(t, dir)
	a := editBranch(t, base, filepath.Join(dir, "a.sv"), func(w *world.World) {
		w.FindRegion("north").Players[0].Level = 99
	})
	b := editBranch(t, base, filepath.Join(dir, "b.sv"), func(w *world.World) {
		w.FindRegion("south").Guilds[0].Name = "New Compass"
	})
	out := savecore.NewStore(world.Registry(), filepath.Join(dir, "merged.sv"))

	m := newSession(t, base, a, b, out)
	if err := m.AdoptRegion("north"); err != nil {
		t.Fatalf("AdoptRegion: %v", err)
	}
	// "Crash": the session object is dropped before Finalize.

	resumed, err := savecore.ResumeMergeSession(world.Registry(), base, a, b, out)
	if err != nil {
		t.Fatalf("ResumeMergeSession: %v", err)
	}
	adoptAll(t, resumed)
	report, err := resumed.Finalize()
	if err != nil {
		t.Fatalf("Finalize: %v", err)
	}
	if !report.Complete {
		t.Fatalf("resumed merge incomplete: %+v", report)
	}
	var northAdopted bool
	for _, r := range report.AdoptedRegions {
		if r == "north" {
			northAdopted = true
		}
	}
	if !northAdopted {
		t.Fatalf("pre-crash adoption lost: %v", report.AdoptedRegions)
	}
	merged := loadWorld(t, out)
	if got := merged.FindRegion("north").Players[0].Level; got != 99 {
		t.Fatalf("north edit lost across resume: %d", got)
	}
	if got := merged.FindRegion("south").Guilds[0].Name; got != "New Compass" {
		t.Fatalf("south edit lost across resume: %q", got)
	}
}

// TestUndoRegionAdoption: undoing an adoption retracts exactly that
// region's objects; the finalized save keeps their base versions and
// says so.
func TestUndoRegionAdoption(t *testing.T) {
	dir := t.TempDir()
	base := saveBase(t, dir)
	a := editBranch(t, base, filepath.Join(dir, "a.sv"), func(w *world.World) {
		w.FindRegion("north").Players[0].Level = 99
	})
	b := editBranch(t, base, filepath.Join(dir, "b.sv"), func(w *world.World) {})
	out := savecore.NewStore(world.Registry(), filepath.Join(dir, "merged.sv"))

	m := newSession(t, base, a, b, out)
	if err := m.AdoptRegion("north"); err != nil {
		t.Fatalf("AdoptRegion: %v", err)
	}
	if err := m.UndoRegion("north"); err != nil {
		t.Fatalf("UndoRegion: %v", err)
	}
	pending := m.Pending()
	if len(pending) == 0 {
		t.Fatal("undone region should be pending again")
	}
	report, err := m.Finalize()
	if err != nil {
		t.Fatalf("Finalize: %v", err)
	}
	if report.Complete {
		t.Fatal("merge with an un-adopted region claims completeness")
	}
	var adaPending bool
	for _, po := range report.PendingObjects {
		if po.Region == "north" && po.Reason == "not adopted" && po.Kept == "base" {
			adaPending = true
		}
	}
	if !adaPending {
		t.Fatalf("undone object missing from report: %+v", report.PendingObjects)
	}
	merged := loadWorld(t, out)
	if got := merged.FindRegion("north").Players[0].Level; got != 7 {
		t.Fatalf("undone adoption leaked into save: level = %d", got)
	}
}
