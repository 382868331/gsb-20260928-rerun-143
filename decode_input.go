package gamesave

import "encoding/json"

// wireObjectIn keeps fields raw so UseNumber decoding preserves integers.
type wireObjectIn struct {
	ID      int             `json:"id"`
	Type    string          `json:"type"`
	Version int             `json:"version"`
	Fields  json.RawMessage `json:"fields"`
}

type wireDocIn struct {
	Format        string                     `json:"format"`
	FormatVersion int                        `json:"format_version"`
	Objects       []wireObjectIn             `json:"objects"`
	Roots         map[string]json.RawMessage `json:"roots"`
}
