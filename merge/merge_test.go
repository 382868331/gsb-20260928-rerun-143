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

func testLayout() merge.Layout {
	return merge.Layout{
		RootType:     "world.World",
		RegionType:   "world.Region",
		RegionsField: "Regions",
		NameField:    "Name",
	}
}

// baseWorld builds the shared savepoint:
//   - north: Ada (carrying the shared relic)
//   - south: Bo (carrying the same relic) and the guild, whose
//     Leader lives in the north — a cross-region reference.
//
// The relic is listed in the north but carried in both regions:
// one shared object, referenced across region borders.
func baseWorld() *world.World {
	relic := &world.Item{ID: "Relic", Power: 10}
	ada := &world.Player{UID: "p-ada", Name: "Ada", Level: 5, Inventory: []*world.Item{relic}}
	bo := &world.Player{UID: "p-bo", Name: "Bo", Level: 2, Inventory: []*world.Item{relic}}
	guild := &world.Guild{UID: "g-1", Name: "Cartographers", Leader: ada, Members: []*world.Player{ada, bo}}
	bo.Guild = guild
	return &world.World{
		Name: "realm",
		Regions: []*world.Region{
			{Name: "north", Players: []*world.Player{ada}, Items: []*world.Item{relic}},
			{Name: "south", Players: []*world.Player{bo}, Guilds: []*world.Guild{guild}},
		},
	}
}

func saveWorld(t *testing.T, dir, name string, w *world.World) string {
	t.Helper()
	path := filepath.Join(dir, name)
	if err := savecore.NewStore(world.Registry(), path).Save(w); err != nil {
		t.Fatalf("save %s: %v", name, err)
	}
	return path
}

// branchEdits produces the two candidates:
//   - left edits only the north: Ada levels up and finds a new item.
//   - right edits only the south (guild renamed) and moves Bo from
//     the south to the north.
func branchEdits(t *testing.T, dir string) (base, left, right string) {
	t.Helper()
	base = saveWorld(t, dir, "base.sv", baseWorld())

	lw := baseWorld()
	ada := lw.Regions[0].Players[0]
	ada.Level = 9
	newItem := &world.Item{ID: "i-new", Power: 3}
	ada.Inventory = append(ada.Inventory, newItem)
	lw.Regions[0].Items = append(lw.Regions[0].Items, newItem)
	left = saveWorld(t, dir, "left.sv", lw)

	rw := baseWorld()
	rw.Regions[1].Guilds[0].Name = "Navigators"
	bo := rw.Regions[1].Players[0]
	rw.Regions[1].Players = nil
	rw.Regions[0].Players = append(rw.Regions[0].Players, bo)
	right = saveWorld(t, dir, "right.sv", rw)
	return base, left, right
}

func newSession(t *testing.T, base, left, right string) *merge.Session {
	t.Helper()
	s, err := merge.NewSession(world.Registry(), testLayout(), base, left, right)
	if err != nil {
		t.Fatalf("NewSession: %v", err)
	}
	return s
}

func regionStatus(t *testing.T, s *merge.Session, name string) merge.RegionStatus {
	t.Helper()
	for _, st := range s.Regions() {
		if st.Name == name {
			return st
		}
	}
	t.Fatalf("region %q not found", name)
	return merge.RegionStatus{}
}

func loadWorld(t *testing.T, path string) *world.World {
	t.Helper()
	v, err := savecore.NewStore(world.Registry(), path).Load()
	if err != nil {
		t.Fatalf("load merged: %v", err)
	}
	w, ok := v.(*world.World)
	if !ok {
		t.Fatalf("merged root is %T, want *world.World", v)
	}
	return w
}

func findPlayer(w *world.World, uid string) *world.Player {
	seen := map[*world.Player]bool{}
	var found *world.Player
	var walk func(p *world.Player)
	walk = func(p *world.Player) {
		if p == nil || seen[p] {
			return
		}
		seen[p] = true
		if p.UID == uid {
			found = p
		}
		if p.Guild != nil {
			for _, m := range p.Guild.Members {
				walk(m)
			}
			walk(p.Guild.Leader)
		}
	}
	for _, r := range w.Regions {
		for _, p := range r.Players {
			walk(p)
		}
		for _, g := range r.Guilds {
			walk(g.Leader)
			for _, m := range g.Members {
				walk(m)
			}
		}
	}
	return found
}

func TestRegionDiffClassifiesChanges(t *testing.T) {
	dir := t.TempDir()
	base, left, right := branchEdits(t, dir)
	s := newSession(t, base, left, right)

	north := regionStatus(t, s, "north")
	if !north.Conflict {
		t.Fatalf("north should be conflicted: %+v", north)
	}
	// Left: Ada modified + item added. Right: Bo moved in.
	var kinds []string
	for _, c := range north.Left {
		kinds = append(kinds, string(c.Kind))
	}
	if got := strings.Join(kinds, ","); got != "added,modified" {
		t.Fatalf("left north changes = %s", got)
	}
	if len(north.Right) != 1 || north.Right[0].Kind != merge.Moved {
		t.Fatalf("right north changes = %+v", north.Right)
	}

	south := regionStatus(t, s, "south")
	if south.Conflict {
		t.Fatalf("south should be clean (only right changed it)")
	}
	// The move out of the south is a Moved change, not delete+add.
	var sawMove, sawDelete bool
	for _, c := range south.Right {
		if c.Kind == merge.Moved {
			sawMove = true
		}
		if c.Kind == merge.Deleted {
			sawDelete = true
		}
	}
	if !sawMove || sawDelete {
		t.Fatalf("Bo's departure must be a move, not a delete: %+v", south.Right)
	}
}

func TestMergeAdoptAndFinalize(t *testing.T) {
	dir := t.TempDir()
	base, left, right := branchEdits(t, dir)
	s := newSession(t, base, left, right)

	// Operator adopts clean regions first, then resolves the rest.
	if adopted := s.AutoAdopt(); len(adopted) != 1 || adopted[0] != "south" {
		t.Fatalf("AutoAdopt = %v, want [south]", adopted)
	}
	if p := s.Pending(); len(p) != 1 || p[0].Region != "north" || !p[0].Conflict {
		t.Fatalf("Pending = %+v, want conflicted north", p)
	}
	if err := s.Adopt("north", merge.SideLeft); err != nil {
		t.Fatal(err)
	}

	out := filepath.Join(dir, "merged.sv")
	report, err := s.Finalize(out)
	if err != nil {
		t.Fatalf("Finalize: %v", err)
	}
	if !report.Complete {
		t.Fatalf("report should be complete: %+v", report.Pending)
	}
	// Bo moved out of the adopted south but his arrival in the north
	// was not adopted; he must be retained, explicitly.
	if len(report.Retained) != 1 || report.Retained[0].Key != "world.Player|p-bo" {
		t.Fatalf("Retained = %+v, want p-bo", report.Retained)
	}
	if _, err := os.Stat(report.ReportPath()); err != nil {
		t.Fatalf("report file missing: %v", err)
	}

	w := loadWorld(t, out)
	ada := findPlayer(w, "p-ada")
	bo := findPlayer(w, "p-bo")
	if ada == nil || bo == nil {
		t.Fatal("players lost in merge")
	}
	if ada.Level != 9 {
		t.Fatalf("adopted north change lost: Ada level %d", ada.Level)
	}
	// The south was adopted from the right: guild renamed.
	var guild *world.Guild
	for _, r := range w.Regions {
		for _, g := range r.Guilds {
			guild = g
		}
	}
	if guild == nil || guild.Name != "Navigators" {
		t.Fatalf("adopted south change lost: %+v", guild)
	}
	// Cross-region references survive as identity, not copies:
	// the guild's leader IS the Ada in the north.
	if guild.Leader != ada {
		t.Fatal("cross-region reference duplicated Ada")
	}
	// The shared relic is still one object across regions.
	if len(ada.Inventory) != 2 || len(bo.Inventory) != 1 {
		t.Fatalf("inventories wrong: ada=%d bo=%d", len(ada.Inventory), len(bo.Inventory))
	}
	if ada.Inventory[0] != bo.Inventory[0] {
		t.Fatal("shared relic was duplicated by the merge")
	}
	if ada.Inventory[1].ID != "i-new" {
		t.Fatal("new item from the left candidate missing")
	}
}

func TestDeleteVsModifyConflict(t *testing.T) {
	dir := t.TempDir()
	base := saveWorld(t, dir, "base.sv", baseWorld())

	// Left deletes the relic entirely.
	lw := baseWorld()
	lw.Regions[0].Items = nil
	lw.Regions[0].Players[0].Inventory = nil
	lw.Regions[1].Players[0].Inventory = nil
	left := saveWorld(t, dir, "left.sv", lw)

	// Right modifies the relic.
	rw := baseWorld()
	rw.Regions[0].Items[0].Power = 99
	right := saveWorld(t, dir, "right.sv", rw)

	s := newSession(t, base, left, right)
	north := regionStatus(t, s, "north")
	if !north.Conflict {
		t.Fatal("delete vs modify must conflict")
	}
	found := false
	for _, c := range north.Conflicts {
		if c.Key == "world.Item|Relic" && c.Kind == "delete-vs-modify" {
			found = true
		}
	}
	if !found {
		t.Fatalf("conflicts = %+v, want delete-vs-modify on Relic", north.Conflicts)
	}
}

func TestFinalizeWithUndecidedRegionIsExplicitlyIncomplete(t *testing.T) {
	dir := t.TempDir()
	base, left, right := branchEdits(t, dir)
	s := newSession(t, base, left, right)
	s.AutoAdopt() // south only; north stays undecided

	out := filepath.Join(dir, "merged.sv")
	report, err := s.Finalize(out)
	if err != nil {
		t.Fatal(err)
	}
	if report.Complete {
		t.Fatal("merge with an undecided conflict must not claim completeness")
	}
	if len(report.Pending) != 1 || report.Pending[0].Region != "north" {
		t.Fatalf("Pending = %+v", report.Pending)
	}
	// The undecided region keeps its base content.
	w := loadWorld(t, out)
	if ada := findPlayer(w, "p-ada"); ada == nil || ada.Level != 5 {
		t.Fatalf("undecided region should keep base content: %+v", ada)
	}
	// The report on disk says the same.
	raw, err := os.ReadFile(report.ReportPath())
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(raw), `"north"`) {
		t.Fatal("report file does not name the pending region")
	}
}

func TestUndoRetractsOnlyThatRegion(t *testing.T) {
	dir := t.TempDir()
	base, left, right := branchEdits(t, dir)
	s := newSession(t, base, left, right)
	s.AutoAdopt()
	if err := s.Adopt("north", merge.SideLeft); err != nil {
		t.Fatal(err)
	}
	s.Undo("north")
	if st := regionStatus(t, s, "north"); st.Decision != "" {
		t.Fatalf("undo left decision %q", st.Decision)
	}
	if st := regionStatus(t, s, "south"); st.Decision != merge.SideRight {
		t.Fatalf("undo of north must not touch south: %q", st.Decision)
	}
	if p := s.Pending(); len(p) != 1 || p[0].Region != "north" {
		t.Fatalf("Pending after undo = %+v", p)
	}
}

func TestResumeFromStateFileKeepsOriginals(t *testing.T) {
	dir := t.TempDir()
	base, left, right := branchEdits(t, dir)
	s := newSession(t, base, left, right)
	s.AutoAdopt()
	if err := s.Adopt("north", merge.SideLeft); err != nil {
		t.Fatal(err)
	}
	statePath := filepath.Join(dir, "merge.state.json")
	if err := s.SaveState(statePath); err != nil {
		t.Fatal(err)
	}

	before := map[string][]byte{}
	for _, p := range []string{base, left, right} {
		raw, err := os.ReadFile(p)
		if err != nil {
			t.Fatal(err)
		}
		before[p] = raw
	}

	// "Interrupted" session resumed from disk.
	s2, err := merge.LoadSession(world.Registry(), testLayout(), statePath)
	if err != nil {
		t.Fatal(err)
	}
	if st := regionStatus(t, s2, "north"); st.Decision != merge.SideLeft {
		t.Fatalf("decision lost across resume: %q", st.Decision)
	}
	out := filepath.Join(dir, "merged.sv")
	report, err := s2.Finalize(out)
	if err != nil {
		t.Fatal(err)
	}
	if !report.Complete {
		t.Fatalf("resumed merge should complete: %+v", report.Pending)
	}
	if ada := findPlayer(loadWorld(t, out), "p-ada"); ada == nil || ada.Level != 9 {
		t.Fatal("resumed merge lost the adopted change")
	}

	// The original candidates are byte-for-byte untouched.
	for p, want := range before {
		got, err := os.ReadFile(p)
		if err != nil {
			t.Fatal(err)
		}
		if string(got) != string(want) {
			t.Fatalf("candidate %s was modified by the merge", p)
		}
	}
}
