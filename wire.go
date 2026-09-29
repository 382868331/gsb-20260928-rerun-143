package gamesave

// Wire format version 1. A save is one JSON document:
//
//	{
//	  "format": "gamesave",
//	  "format_version": 1,
//	  "objects": [
//	    {"id": 0, "type": "player", "version": 2, "fields": {...}}
//	  ],
//	  "roots": {"main": {"kind": "ref", "id": 0}}
//
// Objects are deduplicated by Go pointer identity: the same pointer appears in
// the table once no matter how many places reference it, which also makes
// cycles representable. Field values are plain JSON data; pointers to
// registered objects encode as {"kind":"ref","id":N}.

import "encoding/json"

type wireField map[string]any

type wireObject struct {
	ID      int       `json:"id"`
	Type    string    `json:"type"`
	Version int       `json:"version"`
	Fields  wireField `json:"fields"`
}

type wireDoc struct {
	Format        string                     `json:"format"`
	FormatVersion int                        `json:"format_version"`
	Objects       []wireObject               `json:"objects"`
	Roots         map[string]json.RawMessage `json:"roots"`
}

func refValue(id int) map[string]any {
	return map[string]any{"kind": "ref", "id": id}
}
