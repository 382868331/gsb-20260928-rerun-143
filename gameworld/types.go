// Package gameworld is a fictional game state used to exercise the save core:
// a Player and treasure Chests. It deliberately has no rendering - it exists
// only to prove identity, cycles and version migration round-trip.
package gameworld

import "fmt"

var nextID uint64 = 1

func newID() uint64 {
	id := nextID
	nextID++
	return id
}

// Player version 2. Version 1 had a single numeric "gold" field; v2 splits it
// into Coins and Banked and remembers the player's hometown. See Migrate.
type Player struct {
	ID       uint64 `json:"-"`
	Name     string `json:"name"`
	Coins    int64  `json:"coins"`
	Banked   int64  `json:"banked"`
	Hometown string `json:"hometown"`
	// Home is a chest the player keeps their things in. Chest.Owner points
	// back, forming a cycle that must survive save/load.
	Home    *Chest    `json:"home"`
	Friends []*Player `json:"friends"`
}

func NewPlayer(name string, coins int64) *Player {
	return &Player{ID: newID(), Name: name, Coins: coins, Hometown: "Unknown"}
}

func (p *Player) ObjectID() uint64 { return p.ID }

// Migrate upgrades old player records. v1 -> v2: all old gold becomes carried
// coins, nothing was banked, and hometown defaults to the starter town. The
// input/output are plain field maps; reference markers pass through untouched.
func (p *Player) Migrate(from int, fields map[string]any) (map[string]any, error) {
	if from != 1 {
		return nil, fmt.Errorf("player: no migration from version %d", from)
	}
	return map[string]any{
		"name":     fields["name"],
		"coins":    fields["gold"],
		"banked":   int64(0),
		"hometown": "Brindlebrook",
		"home":     fields["home"],
		"friends":  fields["friends"],
	}, nil
}

// Chest is a treasure container owned by exactly one player.
type Chest struct {
	ID    uint64         `json:"-"`
	Label string         `json:"label"`
	Items map[string]int `json:"items"`
	Owner *Player        `json:"owner"`
}

func NewChest(label string) *Chest {
	return &Chest{ID: newID(), Label: label, Items: map[string]int{}}
}

func (c *Chest) ObjectID() uint64 { return c.ID }
