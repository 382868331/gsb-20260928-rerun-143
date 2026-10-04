// Package world defines the fictional game state used with savecore:
// players, guilds and relics. It exists entirely for the save
// service; there is no rendering or gameplay.
package world

import (
	"fmt"

	"savegame/savecore"
)

// Item is a relic that can be carried by several players at once
// (shared object identity across the graph).
type Item struct {
	ID    string
	Power int
}

// Player is the current (version 2) shape.
type Player struct {
	Name      string
	Level     int
	Guild     *Guild
	Inventory []*Item
}

// Guild cycles back to its members; Leader is also in Members,
// producing a guaranteed cycle and shared node.
type Guild struct {
	Name    string
	Leader  *Player
	Members []*Player
}

// Region is a named partition of the world. Objects listed here are
// "in" the region for branching and merging; references between
// regions (a player carrying an item stocked elsewhere, a guild whose
// members live in another region) are ordinary graph references.
type Region struct {
	Name    string
	Players []*Player
	Guilds  []*Guild
	Items   []*Item
}

// World is the save root: a set of regions edited independently.
type World struct {
	Regions []*Region
}

// RegionOf reports which region directly contains obj, for
// savecore.Branch. Shared objects listed under several regions are
// attributed to the first; objects only reachable by reference are
// unregioned ("").
func (w *World) RegionOf(obj any) string {
	// A region container belongs to the region it defines, so that
	// adopting or undoing a region also covers reference decisions
	// stored on the container itself (its Players/Guilds/Items lists).
	if r, ok := obj.(*Region); ok {
		return r.Name
	}
	for _, r := range w.Regions {
		for _, p := range r.Players {
			if p == obj {
				return r.Name
			}
		}
		for _, g := range r.Guilds {
			if g == obj {
				return r.Name
			}
		}
		for _, it := range r.Items {
			if it == obj {
				return r.Name
			}
		}
	}
	return ""
}

// FindRegion returns the named region, or nil.
func (w *World) FindRegion(name string) *Region {
	for _, r := range w.Regions {
		if r.Name == name {
			return r
		}
	}
	return nil
}

// Registry returns a registry containing every type the current game
// knows, plus migrations for reading older saves.
func Registry() *savecore.Registry {
	reg := savecore.NewRegistry()

	reg.Register(savecore.Codec{Name: "world.Item", Version: 1, New: func() any { return &Item{} }})
	reg.Register(savecore.Codec{Name: "world.Player", Version: 2, New: func() any { return &Player{} }})
	reg.Register(savecore.Codec{Name: "world.Guild", Version: 2, New: func() any { return &Guild{} }})
	reg.Register(savecore.Codec{Name: "world.Region", Version: 1, New: func() any { return &Region{} }})
	reg.Register(savecore.Codec{Name: "world.World", Version: 1, New: func() any { return &World{} }})

	// Player v1: {"Nickname": string, "XP": number, "Guild": node,
	//             "Inventory": [node]}
	// v2 splits XP into Level and drops the title casing.
	reg.AddMigration("world.Player", 1, func(from int, data map[string]any) (map[string]any, error) {
		nick, ok := data["Nickname"].(string)
		if !ok {
			return nil, fmt.Errorf("player v1 missing string Nickname")
		}
		xp, ok := data["XP"].(float64)
		if !ok {
			return nil, fmt.Errorf("player v1 missing numeric XP")
		}
		out := map[string]any{
			"Name":      nick,
			"Level":     int(xp/100) + 1,
			"Guild":     data["Guild"],
			"Inventory": data["Inventory"],
		}
		return out, nil
	})

	// Guild v1: {"Title", "Leader", "Members"}; v2 renames Title to Name.
	reg.AddMigration("world.Guild", 1, func(from int, data map[string]any) (map[string]any, error) {
		title, ok := data["Title"].(string)
		if !ok {
			return nil, fmt.Errorf("guild v1 missing string Title")
		}
		return map[string]any{
			"Name":    title,
			"Leader":  data["Leader"],
			"Members": data["Members"],
		}, nil
	})

	return reg
}
