// Command save-demo builds a fictional world state with shared objects and a
// reference cycle, saves it atomically, then loads it into fresh variables and
// verifies the relationships survived. It also loads a checked-in v1 save to
// demonstrate cross-version migration. No game UI is involved.
package main

import (
	"fmt"
	"os"
	"path/filepath"

	"gamesave"
	"gamesave/gameworld"
)

func main() {
	if err := run(); err != nil {
		fmt.Fprintln(os.Stderr, "error:", err)
		os.Exit(1)
	}
}

func run() error {
	reg := gameworld.Registry()
	store := gamesave.NewStore(reg)

	f, err := os.CreateTemp("", "save-*.json")
	if err != nil {
		return err
	}
	path := f.Name()
	f.Close()
	defer os.Remove(path)

	ald := gameworld.NewPlayer("Alden", 120)
	mira := gameworld.NewPlayer("Mira", 30)
	chest := gameworld.NewChest("Sea Chest")
	chest.Items["rope"] = 2
	chest.Items["torch"] = 5
	chest.Owner = ald
	ald.Home = chest
	ald.Friends = []*gameworld.Player{mira, ald}
	mira.Friends = []*gameworld.Player{ald}

	if err := store.Save(path, map[string]any{"hero": ald, "companion": mira}); err != nil {
		return err
	}

	var hero, companion *gameworld.Player
	if err := store.Load(path, map[string]any{"hero": &hero, "companion": &companion}); err != nil {
		return err
	}
	if hero.Home == nil || hero.Home.Owner != hero {
		return fmt.Errorf("cycle broken: hero.Home.Owner is not hero")
	}
	if hero.Friends[0] != companion {
		return fmt.Errorf("shared object duplicated: friend should be the same object as companion root")
	}
	if hero.Friends[1] != hero {
		return fmt.Errorf("self reference lost")
	}
	fmt.Printf("round-trip ok: %s owns %q, %d items, one shared friend, cycle intact\n",
		hero.Name, hero.Home.Label, len(hero.Home.Items))

	// Cross-version recovery: read a v1 save with the v2 binary.
	var oldHero *gameworld.Player
	v1Path := filepath.Join("gameworld", "testdata", "save_player_v1.json")
	if err := store.Load(v1Path, map[string]any{"hero": &oldHero}); err != nil {
		return err
	}
	if oldHero.Coins != 250 || oldHero.Banked != 0 || oldHero.Hometown != "Brindlebrook" {
		return fmt.Errorf("migration produced unexpected state: %+v", oldHero)
	}
	if oldHero.Home == nil || oldHero.Home.Owner != oldHero {
		return fmt.Errorf("cycle lost during v1 -> v2 migration")
	}
	if len(oldHero.Friends) != 1 || oldHero.Friends[0].Name != "Mira" {
		return fmt.Errorf("migrated friend graph wrong: %+v", oldHero.Friends)
	}
	fmt.Printf("migration ok: v1 hero %q -> v2 (coins=%d, banked=%d, hometown=%s)\n",
		oldHero.Name, oldHero.Coins, oldHero.Banked, oldHero.Hometown)
	return nil
}
