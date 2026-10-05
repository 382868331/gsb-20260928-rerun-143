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
	if _, err := m.UndoRegion("north"); err != nil {
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

// TestBranchNewObjectsGetDistinctIdentities: objects newly added in
// two branches must never share a stable id just because they are the
// first object each branch minted. The scenario also covers a shared
// object moved in one branch, a delete-vs-modify conflict, rescue of
// a branch-new reference target with no base version, resume after a
// save/reopen, and undoing one region retracting only its references.
func TestBranchNewObjectsGetDistinctIdentities(t *testing.T) {
	dir := t.TempDir()
	base := saveBase(t, dir)

	// Branch A: adds a new northern relic (carried by Ada), removes
	// the shared relic completely (both carriers and the region list),
	// and levels Ada.
	a := editBranch(t, base, filepath.Join(dir, "a.sv"), func(w *world.World) {
		north := w.FindRegion("north")
		echo := &world.Item{ID: "Echo", Power: 1}
		ada := north.Players[0]
		north.Items = []*world.Item{echo}
		ada.Inventory = []*world.Item{echo}
		w.FindRegion("south").Players[0].Inventory = nil
		ada.Level = 10
	})

	// Branch B: adds a distinct southern relic with the SAME game id
	// "Echo", carried by Bo, and powers the shared relic up.
	b := editBranch(t, base, filepath.Join(dir, "b.sv"), func(w *world.World) {
		south := w.FindRegion("south")
		echo := &world.Item{ID: "Echo", Power: 2}
		south.Items = append(south.Items, echo)
		w.FindRegion("north").Items[0].Power = 100 // modify shared relic
		// Move a shared/cross-region object: Bo joins the north region
		// while keeping his guild and southern cross-references.
		bo := south.Players[0]
		south.Players = nil
		w.FindRegion("north").Players = append(w.FindRegion("north").Players, bo)
	})

	// The two "Echo" items must have different stable identities.
	ma, err := savecore.LoadManifest(savecore.ManifestPath(a.Path))
	if err != nil {
		t.Fatalf("load A manifest: %v", err)
	}
	mb, err := savecore.LoadManifest(savecore.ManifestPath(b.Path))
	if err != nil {
		t.Fatalf("load B manifest: %v", err)
	}
	itemIDs := func(m *savecore.Manifest) map[string]bool {
		out := map[string]bool{}
		for sid, meta := range m.Objects {
			if meta.Type == "world.Item" {
				out[sid] = true
			}
		}
		return out
	}
	mbase, err := savecore.LoadManifest(savecore.ManifestPath(base.Path))
	if err != nil {
		t.Fatalf("load base manifest: %v", err)
	}
	newInA, newInB := itemIDs(ma), itemIDs(mb)
	// Ids shared with the base are inherited identities (the relic);
	// every genuinely new id must be branch-unique.
	for sid := range newInA {
		if newInB[sid] && mbase.Objects[sid] == nil {
			t.Fatalf("branch-new objects collided on stable id %s", sid)
		}
	}

	out := savecore.NewStore(world.Registry(), filepath.Join(dir, "merged.sv"))
	m := newSession(t, base, a, b, out)

	// Locate diff rows for the relic (delete-modify) and both Echo items.
	var relicRow, echoARow, echoB *savecore.ObjectDiff
	for _, od := range m.Diff().Objects {
		if od.Type != "world.Item" {
			continue
		}
		switch {
		case od.KindA == savecore.Deleted || od.KindB == savecore.Deleted:
			relicRow = od
		case od.KindA == savecore.Added:
			echoARow = od
		case od.KindB == savecore.Added:
			echoB = od
		}
	}
	if relicRow == nil || relicRow.Conflict != "delete-modify" {
		t.Fatalf("shared relic should be a delete-modify row: %+v", relicRow)
	}
	if echoARow == nil || echoB == nil || echoARow.StableID == echoB.StableID {
		t.Fatalf("two Echo items must be separate additions: %+v %+v", echoARow, echoB)
	}
	if echoARow.Region != "north" || echoB.Region != "south" {
		t.Fatalf("echo regions wrong: %q %q", echoARow.Region, echoB.Region)
	}

	// South is conflict-free and adopts first; its brand-new Echo has
	// no base version and must survive Finalize without an error.
	clean := m.CleanRegions()
	var southClean bool
	for _, r := range clean {
		if r == "south" {
			southClean = true
		}
	}
	if !southClean {
		t.Fatalf("south must be clean: %v", clean)
	}
	if err := m.AdoptRegion("south"); err != nil {
		t.Fatalf("AdoptRegion(south): %v", err)
	}

	report, err := m.Finalize()
	if err != nil {
		t.Fatalf("Finalize with branch-new target: %v", err)
	}
	if report.Complete {
		t.Fatal("partial merge must not claim completeness")
	}
	// The unadopted north must stay explicit: nothing is silently lost.
	var sawNorthPending bool
	for _, po := range report.PendingObjects {
		if po.Region == "north" {
			sawNorthPending = true
		}
	}
	if !sawNorthPending {
		t.Fatalf("unadopted north objects missing from report: %+v", report.PendingObjects)
	}

	// The save written at this checkpoint must load and contain B's
	// brand-new Echo under its own identity (simulates save/reopen).
	checkpoint := loadWorld(t, out)
	south := checkpoint.FindRegion("south")
	if len(south.Items) != 1 || south.Items[0].ID != "Echo" || south.Items[0].Power != 2 {
		t.Fatalf("branch B Echo lost at checkpoint: %+v", south.Items)
	}

	// Reopen the merge from its persisted state and finish it.
	resumed, err := savecore.ResumeMergeSession(world.Registry(), base, a, b, out)
	if err != nil {
		t.Fatalf("ResumeMergeSession: %v", err)
	}
	// Resolve the relic conflict in B's favour and adopt the rest.
	var relicID string
	for _, od := range resumed.Diff().Objects {
		if od.Type == "world.Item" && (od.KindA == savecore.Deleted || od.KindB == savecore.Deleted) {
			relicID = od.StableID
		}
	}
	if err := resumed.Resolve(relicID, savecore.SideB); err != nil {
		t.Fatalf("Resolve relic: %v", err)
	}
	// Ada is modified on A while B left her at base -> clean via A.
	for _, region := range resumed.CleanRegions() {
		if err := resumed.AdoptRegion(region); err != nil {
			t.Fatalf("AdoptRegion(%q) after resume: %v", region, err)
		}
	}
	// Any remaining object-level conflict (e.g. bo modify/modify via relic).
	for _, od := range resumed.Pending() {
		if od.Conflict != "" {
			side := savecore.SideA
			if od.KindB != savecore.Unchanged {
				side = savecore.SideB
			}
			if err := resumed.Resolve(od.StableID, side); err != nil {
				t.Fatalf("Resolve %s: %v", od.StableID, err)
			}
		}
	}
	for _, region := range resumed.CleanRegions() {
		if err := resumed.AdoptRegion(region); err != nil {
			t.Fatalf("final AdoptRegion(%q): %v", region, err)
		}
	}
	report, err = resumed.Finalize()
	if err != nil {
		t.Fatalf("Finalize after resume: %v", err)
	}
	if !report.Complete {
		t.Fatalf("merge should complete after resolution: %+v", report)
	}

	merged := loadWorld(t, out)
	northW, southW := merged.FindRegion("north"), merged.FindRegion("south")
	// Two genuinely different Echo objects, one per region. A's Echo
	// is carried by Ada; B's sits in the south region list.
	if len(northW.Items) != 1 || northW.Items[0].ID != "Aegis-7" {
		t.Fatalf("relic list wrong: %+v", northW.Items)
	}
	if len(southW.Items) != 1 || southW.Items[0].ID != "Echo" || southW.Items[0].Power != 2 {
		t.Fatalf("B Echo wrong: %+v", southW.Items)
	}
	var echoA *world.Item
	for _, it := range northW.Players[0].Inventory {
		if it.ID == "Echo" {
			echoA = it
		}
	}
	if echoA == nil || echoA.Power != 1 {
		t.Fatalf("A Echo wrong: %+v", northW.Players[0].Inventory)
	}
	if echoA == southW.Items[0] {
		t.Fatal("distinct branch-new objects collapsed into one pointer")
	}
	// The shared relic survived as one object and carries B's
	// modification; Bo moved into the north region and still carries it.
	ada := northW.Players[0]
	if len(southW.Players) != 0 {
		t.Fatalf("Bo did not move out of south: %+v", southW.Players)
	}
	var bo *world.Player
	for _, p := range northW.Players {
		if p.Name == "Bo" {
			bo = p
		}
	}
	if bo == nil {
		t.Fatal("moved Bo missing from north")
	}
	relic := northW.Items[0]
	if relic.Power != 100 {
		t.Fatalf("shared relic should carry B's power: %d", relic.Power)
	}
	var boHasRelic bool
	for _, it := range bo.Inventory {
		if it == relic {
			boHasRelic = true
		}
	}
	if !boHasRelic {
		t.Fatal("relic duplicated instead of shared after merge")
	}
	if ada.Level != 10 {
		t.Fatalf("A's ada edit lost: level %d", ada.Level)
	}

	// The merged manifest keeps the two branch-new objects under the
	// distinct stable ids their branches minted.
	mm, err := savecore.LoadManifest(savecore.ManifestPath(out.Path))
	if err != nil {
		t.Fatalf("merged manifest: %v", err)
	}
	itemIDCount := map[string]int{}
	for _, sid := range mm.Nodes {
		if mm.Objects[sid] != nil && mm.Objects[sid].Type == "world.Item" {
			itemIDCount[sid]++
		}
	}
	if len(itemIDCount) < 3 { // shared relic + two Echoes
		t.Fatalf("merged manifest lost branch-new identities: %v", itemIDCount)
	}
	if !containsSID(itemIDCount, echoARow.StableID) || !containsSID(itemIDCount, echoB.StableID) {
		t.Fatal("merged manifest dropped a branch-new stable id")
	}

	// Undo south: only south adoption and its reference consequences
	// retract; the north content stays adopted.
	if _, err := resumed.UndoRegion("south"); err != nil {
		t.Fatalf("UndoRegion(south): %v", err)
	}
	report, err = resumed.Finalize()
	if err != nil {
		t.Fatalf("Finalize after undo: %v", err)
	}
	if report.Complete {
		t.Fatal("undo must leave the merge incomplete")
	}
	var echoBUndone bool
	for _, po := range report.PendingObjects {
		if po.StableID == echoB.StableID {
			echoBUndone = true
		}
	}
	if !echoBUndone {
		t.Fatalf("B's Echo missing from pending after undo: %+v", report.PendingObjects)
	}
	merged = loadWorld(t, out)
	north2 := merged.FindRegion("north")
	if len(north2.Items) != 1 || north2.Items[0].ID != "Aegis-7" {
		t.Fatalf("north relic list wrong after undo: %+v", north2.Items)
	}
	var echoAStillThere bool
	for _, it := range north2.Players[0].Inventory {
		if it.ID == "Echo" && it.Power == 1 {
			echoAStillThere = true
		}
	}
	if !echoAStillThere {
		t.Fatalf("undo south retracted north content: %+v", north2.Players[0].Inventory)
	}
	south2 := merged.FindRegion("south")
	if len(south2.Items) != 0 {
		t.Fatalf("undone south Echo survived in output: %+v", south2.Items)
	}

	// Original candidates were never rewritten and remain recoverable.
	wa := loadWorld(t, a)
	if wa.FindRegion("north").Players[0].Level != 10 {
		t.Fatal("candidate A changed by the merge")
	}
	wb := loadWorld(t, b)
	var bRelic *world.Item
	for _, p := range wb.FindRegion("north").Players {
		if p.Name == "Bo" {
			for _, it := range p.Inventory {
				if it.ID == "Aegis-7" {
					bRelic = it
				}
			}
		}
	}
	if bRelic == nil || bRelic.Power != 100 {
		t.Fatal("candidate B changed by the merge")
	}
}

func containsSID(m map[string]int, sid string) bool { _, ok := m[sid]; return ok }

// TestMergeCheckpoints: named checkpoints snapshot adopted regions,
// pending objects and reference decisions; a later session can diff
// two checkpoints; restoring a checkpoint never rewrites the
// candidates; undoing a region between checkpoints names the
// reference decisions it invalidates while other regions stay adopted.
func TestMergeCheckpoints(t *testing.T) {
	dir := t.TempDir()
	base := saveBase(t, dir)
	// Branch A drops Bo (south) entirely; branch B only renames the
	// northern guild, whose base/B member list still references Bo —
	// so adopting south and taking B's guild produces a reference
	// rescue decision for Bo.
	a := editBranch(t, base, filepath.Join(dir, "a.sv"), func(w *world.World) {
		south := w.FindRegion("south")
		south.Players = nil
		south.Guilds = nil
		carto := w.FindRegion("north").Guilds[0]
		carto.Members = carto.Members[:1]
	})
	b := editBranch(t, base, filepath.Join(dir, "b.sv"), func(w *world.World) {
		w.FindRegion("north").Guilds[0].Name = "Cartographers II"
	})
	out := savecore.NewStore(world.Registry(), filepath.Join(dir, "merged.sv"))

	candidateBytes := func() map[string][]byte {
		raw := map[string][]byte{}
		for _, s := range []*savecore.Store{base, a, b} {
			data, err := os.ReadFile(s.Path)
			if err != nil {
				t.Fatalf("read candidate %s: %v", s.Path, err)
			}
			raw[s.Path] = data
		}
		return raw
	}
	before := candidateBytes()

	m := newSession(t, base, a, b, out)
	var boRow *savecore.ObjectDiff
	for _, od := range m.Diff().Objects {
		if od.Type == "world.Player" && od.KindA == savecore.Deleted {
			boRow = od
		}
	}
	if boRow == nil || boRow.Region != "south" {
		t.Fatalf("expected Bo as a south deletion row: %+v", boRow)
	}

	// South is clean and adopts first; the guild is a modify-modify
	// conflict resolved in B's favour, so the chosen guild still
	// references the deleted Bo.
	if err := m.AdoptRegion("south"); err != nil {
		t.Fatalf("AdoptRegion(south): %v", err)
	}
	var cartoRow *savecore.ObjectDiff
	for _, od := range m.Diff().Objects {
		if od.Type == "world.Guild" && od.Conflict == "modify-modify" {
			cartoRow = od
		}
	}
	if cartoRow == nil {
		t.Fatal("expected a modify-modify conflict on the northern guild")
	}
	if err := m.Resolve(cartoRow.StableID, savecore.SideB); err != nil {
		t.Fatalf("Resolve guild: %v", err)
	}
	if err := m.AdoptRegion("north"); err != nil {
		t.Fatalf("AdoptRegion(north): %v", err)
	}
	cp1, err := m.SaveCheckpoint("both-adopted")
	if err != nil {
		t.Fatalf("SaveCheckpoint: %v", err)
	}
	if len(cp1.AdoptedRegions) != 2 {
		t.Fatalf("checkpoint regions = %v, want north+south", cp1.AdoptedRegions)
	}
	var boRescued bool
	for _, pr := range cp1.ReferenceDecisions {
		if pr.To == boRow.StableID && pr.Rescued == "base" {
			boRescued = true
		}
	}
	if !boRescued {
		t.Fatalf("checkpoint must record the Bo rescue decision: %+v", cp1.ReferenceDecisions)
	}

	// Withdraw south: the Bo rescue decision lapses, north stays adopted.
	invalidated, err := m.UndoRegion("south")
	if err != nil {
		t.Fatalf("UndoRegion(south): %v", err)
	}
	var boInvalidated bool
	for _, pr := range invalidated {
		if pr.To == boRow.StableID {
			boInvalidated = true
		}
	}
	if !boInvalidated || len(invalidated) == 0 {
		t.Fatalf("undo must invalidate the Bo rescue: %+v", invalidated)
	}
	cp2, err := m.SaveCheckpoint("south-withdrawn")
	if err != nil {
		t.Fatalf("SaveCheckpoint 2: %v", err)
	}
	if len(cp2.AdoptedRegions) != 1 || cp2.AdoptedRegions[0] != "north" {
		t.Fatalf("other regions must stay adopted: %v", cp2.AdoptedRegions)
	}
	var boPending bool
	for _, po := range cp2.PendingObjects {
		if po.StableID == boRow.StableID {
			boPending = true
		}
	}
	if !boPending {
		t.Fatalf("withdrawn Bo must be pending in checkpoint: %+v", cp2.PendingObjects)
	}

	// A later session diffs the two checkpoints from disk.
	resumed, err := savecore.ResumeMergeSession(world.Registry(), base, a, b, out)
	if err != nil {
		t.Fatalf("ResumeMergeSession: %v", err)
	}
	names, err := resumed.Checkpoints()
	if err != nil {
		t.Fatalf("Checkpoints: %v", err)
	}
	if len(names) != 2 || names[0] != "both-adopted" || names[1] != "south-withdrawn" {
		t.Fatalf("checkpoint names = %v", names)
	}
	diff, err := resumed.DiffCheckpoints("both-adopted", "south-withdrawn")
	if err != nil {
		t.Fatalf("DiffCheckpoints: %v", err)
	}
	if len(diff.RegionsOnlyInA) != 1 || diff.RegionsOnlyInA[0] != "south" {
		t.Fatalf("diff regions = %+v", diff.RegionsOnlyInA)
	}
	var diffShowsBoRescue bool
	for _, pr := range diff.ReferencesOnlyInA {
		if pr.To == boRow.StableID {
			diffShowsBoRescue = true
		}
	}
	if !diffShowsBoRescue {
		t.Fatalf("diff must show the lapsed Bo rescue: %+v", diff.ReferencesOnlyInA)
	}
	var diffShowsBoPending bool
	for _, po := range diff.PendingOnlyInB {
		if po.StableID == boRow.StableID {
			diffShowsBoPending = true
		}
	}
	if !diffShowsBoPending {
		t.Fatalf("diff must show Bo newly pending: %+v", diff.PendingOnlyInB)
	}

	// Restore the first checkpoint in the later session: the review
	// state rolls back, and the candidate files are byte-identical.
	if err := resumed.RestoreCheckpoint("both-adopted"); err != nil {
		t.Fatalf("RestoreCheckpoint: %v", err)
	}
	report, err := resumed.Finalize()
	if err != nil {
		t.Fatalf("Finalize after restore: %v", err)
	}
	var boRescuedAfterRestore bool
	for _, pr := range report.PendingReferences {
		if pr.To == boRow.StableID {
			boRescuedAfterRestore = true
		}
	}
	if !boRescuedAfterRestore {
		t.Fatalf("restored checkpoint lost the Bo rescue: %+v", report.PendingReferences)
	}
	merged := loadWorld(t, out)
	if len(merged.FindRegion("south").Players) != 0 {
		t.Fatal("restored adoption did not re-apply the south deletion")
	}
	for path, data := range before {
		after, err := os.ReadFile(path)
		if err != nil {
			t.Fatalf("read candidate %s: %v", path, err)
		}
		if string(after) != string(data) {
			t.Fatalf("candidate %s was rewritten by checkpoint restore", path)
		}
	}
}
