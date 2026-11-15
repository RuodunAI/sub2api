package store

import (
	"database/sql/driver"
	"fmt"
	"io"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
)

// bridgeRows hands pgx rows to database/sql. Each column is decoded by the
// resultType of its PostgreSQL type, planned once per query.
type bridgeRows struct {
	r    pgx.Rows
	cols []string
	dec  []columnDecoder
}

// columnDecoder turns one non-NULL raw value into what database/sql gets.
type columnDecoder func(src []byte) (driver.Value, error)

// resultType is how the bridge reads one PostgreSQL type: binary for types
// whose wire form is cheaper to decode, text where the text is the value
// (numeric keeps every digit; uuid, inet, interval read as PostgreSQL
// prints them).
type resultType struct {
	binary bool
	plan   func(m *pgtype.Map, oid uint32, format int16) columnDecoder
}

// resultTypes are the types the bridge reads: every column type of the
// migrated schema (TestGORMReadsEverySchemaColumn) and what expressions
// commonly yield. Scalars follow pgx's database/sql driver, so scanners
// written for it work unchanged; arrays come as Go slices (Int64s and
// Strings read them). Anything else is an error, never a guess.
var resultTypes = map[uint32]resultType{
	pgtype.BoolOID:        {true, decodeAs(func(v bool) (driver.Value, error) { return v, nil })},
	pgtype.Int2OID:        {true, decodeAs(func(v int16) (driver.Value, error) { return int64(v), nil })},
	pgtype.Int4OID:        {true, decodeAs(func(v int32) (driver.Value, error) { return int64(v), nil })},
	pgtype.Int8OID:        {true, decodeAs(func(v int64) (driver.Value, error) { return v, nil })},
	pgtype.Float4OID:      {true, decodeAs(func(v float32) (driver.Value, error) { return float64(v), nil })},
	pgtype.Float8OID:      {true, decodeAs(func(v float64) (driver.Value, error) { return v, nil })},
	pgtype.NumericOID:     {false, decodeText},
	pgtype.TextOID:        {false, decodeText},
	pgtype.VarcharOID:     {false, decodeText},
	pgtype.BPCharOID:      {false, decodeText},
	pgtype.NameOID:        {false, decodeText},
	pgtype.ByteaOID:       {true, decodeBytes},
	pgtype.JSONOID:        {false, decodeBytes},
	pgtype.JSONBOID:       {false, decodeBytes},
	pgtype.DateOID:        {true, decodeAs(pgtype.Date.Value)},
	pgtype.TimestampOID:   {true, decodeAs(pgtype.Timestamp.Value)},
	pgtype.TimestamptzOID: {true, decodeAs(pgtype.Timestamptz.Value)},
	pgtype.IntervalOID:    {false, decodeText},
	pgtype.UUIDOID:        {false, decodeText},
	pgtype.InetOID:        {false, decodeText},
	pgtype.CIDROID:        {false, decodeText},
	pgtype.BitOID:         {false, decodeText},
	pgtype.VarbitOID:      {false, decodeText},

	pgtype.Int2ArrayOID:        {true, decodeSlice[int64]},
	pgtype.Int4ArrayOID:        {true, decodeSlice[int64]},
	pgtype.Int8ArrayOID:        {true, decodeSlice[int64]},
	pgtype.Float4ArrayOID:      {true, decodeSlice[float64]},
	pgtype.Float8ArrayOID:      {true, decodeSlice[float64]},
	pgtype.BoolArrayOID:        {true, decodeSlice[bool]},
	pgtype.TextArrayOID:        {true, decodeSlice[string]},
	pgtype.VarcharArrayOID:     {true, decodeSlice[string]},
	pgtype.BPCharArrayOID:      {true, decodeSlice[string]},
	pgtype.NumericArrayOID:     {false, decodeSlice[string]},
	pgtype.UUIDArrayOID:        {false, decodeSlice[string]},
	pgtype.JSONArrayOID:        {false, decodeSlice[[]byte]},
	pgtype.JSONBArrayOID:       {false, decodeSlice[[]byte]},
	pgtype.TimestamptzArrayOID: {true, decodeSlice[time.Time]},
}

// resultFormats asks the server for the binary form of the resultTypes that
// want it; every other column comes as text.
var resultFormats = func() pgx.QueryResultFormatsByOID {
	f := pgx.QueryResultFormatsByOID{}
	for oid, t := range resultTypes {
		if t.binary {
			f[oid] = pgx.BinaryFormatCode
		}
	}
	return f
}()

// decodeAs scans into a T, reset before every row so that no value handed
// out shares memory with the next one, and converts it with out.
func decodeAs[T any](out func(T) (driver.Value, error)) func(*pgtype.Map, uint32, int16) columnDecoder {
	return func(m *pgtype.Map, oid uint32, format int16) columnDecoder {
		var d T
		plan := m.PlanScan(oid, format, &d)
		return func(src []byte) (driver.Value, error) {
			var zero T
			d = zero
			if err := plan.Scan(src, &d); err != nil {
				return nil, err
			}
			return out(d)
		}
	}
}

var (
	decodeText  = decodeAs(func(v string) (driver.Value, error) { return v, nil })
	decodeBytes = decodeAs(func(v []byte) (driver.Value, error) { return v, nil })
)

func decodeSlice[E any](m *pgtype.Map, oid uint32, format int16) columnDecoder {
	return decodeAs(func(v []E) (driver.Value, error) { return v, nil })(m, oid, format)
}

func newBridgeRows(r pgx.Rows) (*bridgeRows, error) {
	fds := r.FieldDescriptions()
	br := &bridgeRows{r: r, cols: make([]string, len(fds)), dec: make([]columnDecoder, len(fds))}
	m := typeMap(r)
	for i, fd := range fds {
		br.cols[i] = fd.Name
		t, ok := resultTypes[fd.DataTypeOID]
		if !ok {
			r.Close()
			return nil, fmt.Errorf("store: column %q has type %s, which the GORM bridge does not read; cast it in SQL (e.g. ::text)",
				fd.Name, typeName(m, fd.DataTypeOID))
		}
		br.dec[i] = t.plan(m, fd.DataTypeOID, fd.Format)
	}
	return br, nil
}

// typeMap is the type map of the connection behind r, as pgx's own
// database/sql driver uses; a Querier without one gets the defaults.
func typeMap(r pgx.Rows) *pgtype.Map {
	if c := r.Conn(); c != nil {
		return c.TypeMap()
	}
	return pgtype.NewMap()
}

func typeName(m *pgtype.Map, oid uint32) string {
	if t, ok := m.TypeForOID(oid); ok {
		return fmt.Sprintf("%s (oid %d)", t.Name, oid)
	}
	return fmt.Sprintf("oid %d", oid)
}

func (br *bridgeRows) Columns() []string { return br.cols }

func (br *bridgeRows) Close() error {
	br.r.Close()
	return br.r.Err()
}

func (br *bridgeRows) Next(dest []driver.Value) error {
	if !br.r.Next() {
		if err := br.r.Err(); err != nil {
			return err
		}
		return io.EOF
	}
	for i, src := range br.r.RawValues() {
		if src == nil {
			dest[i] = nil
			continue
		}
		v, err := br.dec[i](src)
		if err != nil {
			return fmt.Errorf("store: column %q: %w", br.cols[i], err)
		}
		dest[i] = v
	}
	return nil
}
