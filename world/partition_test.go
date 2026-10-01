package world_test

import (
	"encoding/json"
	"errors"
	"hash/crc32"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"savegame/savecore"
	"savegame/world"
)

func partitionFor(typeName string) string {
	switch {
	case strings.HasPrefix(typeName, "world.Player"):
		return "players"
	case strings.HasPrefix(typeName, "world.Guild"):
		return "guilds"
	default:
		return "items"
	}
}

func partitionStore(t *testing.T) *savecore.Store {
	t.Helper()
	return savecore.NewPartitionedStore(world.Registry(), filepath.Join(t.TempDir(), "snapshot"), partitionFor)
}

func TestPartitionedOnDemandLoadPreservesCrossPartitionIdentity(t *testing.T) {
	store := partitionStore(t)
	if err := store.Save(buildWorld()); err != nil {
		t.Fatalf("Save: %v", err)
	}

	loader, err := store.OpenLoader()
	if err != nil {
		t.Fatalf("OpenLoader: %v", err)
	}
	if err := loader.LoadPartition("players"); err != nil {
		t.Fatalf("LoadPartition players: %v", err)
	}
	root := loader.Root().(*world.Player)
	if root.Name != "Ada" || root.Level != 7 {
		t.Fatalf("root fields not loaded: %+v", root)
	}
	guild := root.Guild
	if guild == nil || guild.Name != "" {
		t.Fatalf("guild should be an unloaded zero placeholder, got %+v", guild)
	}
	if len(root.Inventory) != 1 || root.Inventory[0].ID != "" {
		t.Fatalf("item should be an unloaded zero placeholder, got %+v", root.Inventory)
	}
	if root.Inventory[0].FavoriteOwner != nil {
		t.Fatalf("unloaded item field should remain zero, got %+v", root.Inventory[0].FavoriteOwner)
	}

	if err := loader.LoadPartition("guilds"); err != nil {
		t.Fatalf("LoadPartition guilds: %v", err)
	}
	if guild.Name != "Cartographers" || guild.Leader != root {
		t.Fatalf("guild fields/identity not restored: %+v", guild)
	}
	if guild.Members[0] != root || guild.Members[1].Guild != guild {
		t.Fatal("multi-layer cross-partition cycle broken")
	}
	if err := loader.LoadPartition("items"); err != nil {
		t.Fatalf("LoadPartition items: %v", err)
	}
	rival := guild.Members[1]
	if root.Inventory[0] != rival.Inventory[0] {
		t.Fatal("shared item was duplicated across partitions")
	}
	if root.Inventory[0].FavoriteOwner != root {
		t.Fatal("item-to-player cycle broken")
	}
}

func TestPartitionedLoadAllOrderIndependent(t *testing.T) {
	store := partitionStore(t)
	if err := store.Save(buildWorld()); err != nil {
		t.Fatal(err)
	}
	for _, order := range [][]string{
		{"items", "guilds", "players"},
		{"guilds", "items", "players"},
		{"players", "items", "guilds"},
	} {
		loader, err := store.OpenLoader()
		if err != nil {
			t.Fatal(err)
		}
		for _, name := range order {
			if err := loader.LoadPartition(name); err != nil {
				t.Fatalf("order %v: %v", order, err)
			}
		}
		root := loader.Root().(*world.Player)
		guild := root.Guild
		rival := guild.Members[1]
		if guild.Leader != root || rival.Guild != guild || rival.Inventory[0] != root.Inventory[0] || root.Inventory[0].FavoriteOwner != root {
			t.Fatalf("order %v broke identity/cycles", order)
		}
	}
}

func TestPartitionedCorruptionReportsUnrecoverableRange(t *testing.T) {
	store := partitionStore(t)
	if err := store.Save(buildWorld()); err != nil {
		t.Fatal(err)
	}
	pointer, err := os.ReadFile(filepath.Join(store.Path, "current.json"))
	if err != nil {
		t.Fatal(err)
	}
	gen := strings.Trim(strings.Split(string(pointer), "\"")[3], "}")
	if err := os.WriteFile(filepath.Join(store.Path, gen, "partition-guilds.sv"), []byte("broken"), 0o644); err != nil {
		t.Fatal(err)
	}
	loader, err := store.OpenLoader()
	if err != nil {
		t.Fatalf("manifest should still open: %v", err)
	}
	if err := loader.LoadPartition("players"); err != nil {
		t.Fatalf("healthy partition failed: %v", err)
	}
	err = loader.LoadPartition("guilds")
	var pce *savecore.PartitionCorruptionError
	if !errors.As(err, &pce) {
		t.Fatalf("want PartitionCorruptionError, got %v", err)
	}
	if pce.Partition != "guilds" || len(pce.ObjectIDs) != 1 || pce.ObjectTypes[0] != "world.Guild" {
		t.Fatalf("unexpected unrecoverable range: %+v", pce)
	}
	if _, err := loader.LoadAll(); !errors.As(err, &pce) {
		t.Fatalf("LoadAll must reject incomplete graph, got %v", err)
	}
}

func TestPartitionedInterruptedMigrationKeepsOldSave(t *testing.T) {
	store := partitionStore(t)
	if err := store.Save(buildWorld()); err != nil {
		t.Fatal(err)
	}
	firstGen := currentGeneration(t, store)
	store.BeforePublish = func(string) error { return errors.New("simulated crash before publication") }
	if err := store.Upgrade(); err == nil || err.Error() != "simulated crash before publication" {
		t.Fatalf("want simulated interruption, got %v", err)
	}
	if got := currentGeneration(t, store); got != firstGen {
		t.Fatalf("current generation changed from %s to %s", firstGen, got)
	}
	loader, err := store.OpenLoader()
	if err != nil {
		t.Fatalf("old save no longer opens: %v", err)
	}
	root, err := loader.LoadAll()
	if err != nil {
		t.Fatalf("old complete save no longer loads: %v", err)
	}
	player := root.(*world.Player)
	if player.Name != "Ada" || player.Guild.Name != "Cartographers" {
		t.Fatalf("old save content lost: %+v", player)
	}

	store.BeforePublish = nil
	if err := store.Upgrade(); err != nil {
		t.Fatalf("post-interruption upgrade failed: %v", err)
	}
	loader, err = store.OpenLoader()
	if err != nil {
		t.Fatal(err)
	}
	if _, err := loader.LoadAll(); err != nil {
		t.Fatal(err)
	}
	player = loader.Root().(*world.Player)
	if player.Guild.Leader != player || player.Inventory[0].FavoriteOwner != player {
		t.Fatal("migration broke cycles")
	}
}

func TestPartitionedMigrationChecksCrossPartitionRefsBeforePublish(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "snapshot")
	gen := "gen-old"
	genDir := filepath.Join(dir, gen)
	if err := os.MkdirAll(genDir, 0o755); err != nil {
		t.Fatal(err)
	}
	partitions := []struct {
		name string
		node int
		doc  json.RawMessage
	}{
		{name: "players", node: 1, doc: json.RawMessage(`{
			"format":"savecore","formatVer":2,"partition":"players","nodes":[{
				"$id":1,"$type":"world.Player","$ver":1,"data":{
					"Nickname":"Veteran","XP":250,
					"Guild":{"$ref":2},
					"Inventory":[{"$ref":3}]
				}}
			]}`)},
		{name: "guilds", node: 2, doc: json.RawMessage(`{
			"format":"savecore","formatVer":2,"partition":"guilds","nodes":[{
				"$id":2,"$type":"world.Guild","$ver":1,"data":{
					"Title":"Old Guard",
					"Leader":{"$ref":1},
					"Members":[{"$ref":1}]
				}}
			]}`)},
		{name: "items", node: 3, doc: json.RawMessage(`{
			"format":"savecore","formatVer":2,"partition":"items","nodes":[{
				"$id":3,"$type":"world.Item","$ver":1,"data":{"ID":"Horn","Power":9}
			}]}`)},
	}
	infos := make([]map[string]any, 0, len(partitions))
	for _, p := range partitions {
		raw := p.doc
		frame, err := savecore.Pack(raw)
		if err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(genDir, "partition-"+p.name+".sv"), frame, 0o644); err != nil {
			t.Fatal(err)
		}
		infos = append(infos, map[string]any{"name": p.name, "nodes": []int{p.node}, "crc32": crc32.ChecksumIEEE(raw)})
	}
	manifest := map[string]any{
		"format": "savecore", "formatVer": 2,
		"root":       map[string]any{"$ref": 1},
		"partitions": infos,
		"nodes": []map[string]any{
			{"$id": 1, "$type": "world.Player", "$ver": 1, "partition": "players"},
			{"$id": 2, "$type": "world.Guild", "$ver": 1, "partition": "guilds"},
			{"$id": 3, "$type": "world.Item", "$ver": 1, "partition": "items"},
		},
	}
	manifestRaw, err := json.Marshal(manifest)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(genDir, "manifest.json"), manifestRaw, 0o644); err != nil {
		t.Fatal(err)
	}
	pointer, _ := json.Marshal(map[string]string{"generation": gen})
	if err := os.WriteFile(filepath.Join(dir, "current.json"), pointer, 0o644); err != nil {
		t.Fatal(err)
	}

	store := savecore.NewPartitionedStore(world.Registry(), dir, partitionFor)
	loader, err := store.OpenLoader()
	if err != nil {
		t.Fatalf("open old version: %v", err)
	}
	if err := loader.LoadPartition("guilds"); err != nil {
		t.Fatalf("migrating guild without players should use global refs: %v", err)
	}
	if err := store.Upgrade(); err != nil {
		t.Fatalf("Upgrade: %v", err)
	}
	loader, err = store.OpenLoader()
	if err != nil {
		t.Fatal(err)
	}
	root, err := loader.LoadAll()
	if err != nil {
		t.Fatalf("full load after migration: %v", err)
	}
	player := root.(*world.Player)
	if player.Name != "Veteran" || player.Level != 3 || player.Guild.Name != "Old Guard" {
		t.Fatalf("old fields not migrated: %+v", player)
	}
	if player.Guild.Leader != player || player.Inventory[0].ID != "Horn" {
		t.Fatalf("cross-partition migration broke references: %+v", player)
	}
}

func currentGeneration(t *testing.T, store *savecore.Store) string {
	t.Helper()
	b, err := os.ReadFile(filepath.Join(store.Path, "current.json"))
	if err != nil {
		t.Fatal(err)
	}
	s := string(b)
	parts := strings.Split(s, "\"")
	if len(parts) < 4 {
		t.Fatalf("bad pointer %s", s)
	}
	return parts[3]
}
