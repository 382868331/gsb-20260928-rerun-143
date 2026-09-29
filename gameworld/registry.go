package gameworld

import "gamesave"

// Registry returns the game's current type registry. Only types listed here
// can ever come back to life from a save file, and every historical version
// needed to read old saves stays listed.
func Registry() *gamesave.Registry {
	reg := gamesave.NewRegistry()
	// Player v1 is intentionally not registered as a constructor target; the
	// current v2 entry supplies the migrator for v1 records. To make v1
	// readable we still register its layout name/version so lookup succeeds.
	reg.MustRegister("player", 2, func() gamesave.Object { return &Player{} })
	reg.MustRegisterLegacy("player", 1)
	reg.MustRegister("chest", 1, func() gamesave.Object { return &Chest{} })
	return reg
}
