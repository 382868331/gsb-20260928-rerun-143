package gamesave

import "reflect"

// jsonFieldName mirrors encoding/json for the simple cases used here: an
// explicit name wins, otherwise the Go field name is used verbatim.
func jsonFieldName(f reflect.StructField) string {
	tag := f.Tag.Get("json")
	name := f.Name
	if tag != "" {
		for i := 0; i < len(tag); i++ {
			if tag[i] == ',' {
				if i > 0 {
					name = tag[:i]
				}
				if name == "" {
					name = f.Name
				}
				return name
			}
		}
		if tag == "-" {
			return "-"
		}
		name = tag
	}
	return name
}
