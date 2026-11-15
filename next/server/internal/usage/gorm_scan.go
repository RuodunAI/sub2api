package usage

import (
	"bytes"
	"encoding/json"
	"fmt"
)

// nullRaw scans a json, jsonb or text column into a json.RawMessage through
// database/sql, which refuses NULL for a byte slice type other than []byte:
// NULL reads as nil, as it did with pgx.
type nullRaw struct{ dst *json.RawMessage }

// Scan implements sql.Scanner.
func (n nullRaw) Scan(src any) error {
	switch v := src.(type) {
	case nil:
		*n.dst = nil
	case []byte:
		*n.dst = bytes.Clone(v)
	case string:
		*n.dst = json.RawMessage(v)
	default:
		return fmt.Errorf("usage: cannot scan %T into json.RawMessage", src)
	}
	return nil
}
