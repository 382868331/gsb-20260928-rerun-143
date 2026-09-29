package gameworld_test

import (
	"path/filepath"
	"testing"

	"gamesave"
	"gamesave/gameworld"
)

func TestLoadsV1SaveIntoV2(t *testing.T) {
	reg := gameworld.Registry()
	store := gamesave.NewStore(reg)

	var hero *gameworld.Player
	path := filepath.Join("testdata", "save_player_v1.json")
	if err := store.Load(path, map[string]any{"hero": &hero}); err != nil {
		t.Fatalf("loading v1 save: %v", err)
	}
	if hero.Name != "Alden" {
		t.Fatalf("name lost: %q", hero.Name)
	}
	if hero.Coins != 250 {
		t.Fatalf("gold should migrate into coins=250, got %d", hero.Coins)
	}
	if hero.Banked != 0 {
		t.Fatalf("new banked field should default to 0, got %d", hero.Banked)
	}
	if hero.Hometown != "Brindlebrook" {
		t.Fatalf("hometown default wrong: %q", hero.Hometown)
	}
	// The old chest and its back-reference survive migration as one pair.
	if hero.Home == nil {
		t.Fatal("home chest lost during migration")
	}
	if hero.Home.Label != "Old Sea Chest" {
		t.Fatalf("chest label: %q", hero.Home.Label)
	}
	if hero.Home.Owner != hero {
		t.Fatal("player<->chest cycle broken by migration")
	}
	if got := hero.Home.Items["torch"]; got != 5 {
		t.Fatalf("chest items wrong: torch=%d", got)
	}
	if len(hero.Friends) != 1 || hero.Friends[0].Name != "Mira" || hero.Friends[0].Coins != 90 {
		t.Fatalf("migrated friend graph wrong: %+v", hero.Friends)
	}
}

func TestResavedV1BecomesV2(t *testing.T) {
	reg := gameworld.Registry()
	store := gamesave.NewStore(reg)

	var hero *gameworld.Player
	v1 := filepath.Join("testdata", "save_player_v1.json")
	if err := store.Load(v1, map[string]any{"hero": &hero}); err != nil {
		t.Fatal(err)
	}
	out := filepath.Join(t.TempDir(), "v2.json")
	if err := store.Save(out, map[string]any{"hero": hero}); err != nil {
		t.Fatal(err)
	}
	// A fresh load must still work and every object is now current-version.
	var hero2 *gameworld.Player
	if err := store.Load(out, map[string]any{"hero": &hero2}); err != nil {
		t.Fatal(err)
	}
	if hero2.Hometown != "Brindlebrook" || hero2.Coins != 250 {
		t.Fatalf("resave changed migrated data: %+v", hero2)
	}
	if hero2.Home.Owner != hero2 {
		t.Fatal("cycle broken on resave")
	}
}
