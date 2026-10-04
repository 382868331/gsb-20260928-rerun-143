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
	UID       string
	Name      string
	Level     int
	Guild     *Guild
	Inventory []*Item
}

// Guild cycles back to its members; Leader is also in Members,
// producing a guaranteed cycle and shared node.
type Guild struct {
	UID     string
	Name    string
	Leader  *Player
	Members []*Player
}

// Region groups the objects that are edited together. Objects in
// one region may freely reference objects in another (a player can
// carry an item home to another region, or belong to a guild
// headquartered elsewhere); those cross-region references are what
// a region-level merge must keep intact.
type Region struct {
	Name    string
	Players []*Player
	Guilds  []*Guild
	Items   []*Item
}

// World is the save root: a fixed list of named regions.
type World struct {
	Name    string
	Regions []*Region
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
