// SPDX-License-Identifier: MIT

package server

import (
	"bytes"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"math"
	"math/big"
	"strconv"
	"strings"
	"time"

	duckdb "github.com/duckdb/duckdb-go/v2"
)

// base64Key marks a blob in both directions: {"$base64": "<standard base64>"}.
const base64Key = "$base64"

// decodeParams turns the JSON params array into driver arguments. The decoder ran with UseNumber,
// so numbers arrive as json.Number: an integer literal binds as BIGINT and anything with a fraction
// or exponent as DOUBLE. Booleans bind as BOOLEAN. A blob is an object with the single key
// "$base64". Any other array or object is refused.
func decodeParams(raw []interface{}) ([]interface{}, error) {
	args := make([]interface{}, len(raw))
	for i, value := range raw {
		arg, err := decodeParam(value)
		if err != nil {
			return nil, fmt.Errorf("params[%d]: %v", i, err)
		}
		args[i] = arg
	}
	return args, nil
}

func decodeParam(value interface{}) (interface{}, error) {
	switch v := value.(type) {
	case nil:
		return nil, nil
	case string, bool:
		return v, nil
	case json.Number:
		text := v.String()
		if !strings.ContainsAny(text, ".eE") {
			if n, err := strconv.ParseInt(text, 10, 64); err == nil {
				return n, nil
			}
			return nil, fmt.Errorf("integer %s does not fit in 64 bits", text)
		}
		f, err := strconv.ParseFloat(text, 64)
		if err != nil {
			return nil, fmt.Errorf("number %s is not a 64-bit float", text)
		}
		return f, nil
	case map[string]interface{}:
		encoded, ok := v[base64Key].(string)
		if len(v) != 1 || !ok {
			return nil, fmt.Errorf(`an object parameter must be {"%s": "<base64>"}`, base64Key)
		}
		blobValue, err := base64.StdEncoding.DecodeString(encoded)
		if err != nil {
			return nil, fmt.Errorf("invalid base64: %v", err)
		}
		return blobValue, nil
	default:
		return nil, fmt.Errorf("unsupported parameter type %T", value)
	}
}

// sqlFloat keeps a FLOAT or DOUBLE a float in JSON: 1.0 is written 1.0, not 1, so a client can
// tell it from an integer. JSON has no infinity or NaN; they are the strings "Infinity",
// "-Infinity" and "NaN". bits is 32 for FLOAT, so 0.1 is written 0.1 and not its float64 widening.
type sqlFloat struct {
	value float64
	bits  int
}

func (f sqlFloat) MarshalJSON() ([]byte, error) {
	switch {
	case math.IsInf(f.value, 1):
		return []byte(`"Infinity"`), nil
	case math.IsInf(f.value, -1):
		return []byte(`"-Infinity"`), nil
	case math.IsNaN(f.value):
		return []byte(`"NaN"`), nil
	}
	text := strconv.FormatFloat(f.value, 'g', -1, f.bits)
	if !strings.ContainsAny(text, ".eE") {
		text += ".0"
	}
	return []byte(text), nil
}

// blob is a BLOB value: {"$base64": "..."}.
type blob []byte

func (b blob) MarshalJSON() ([]byte, error) {
	var buf bytes.Buffer
	buf.WriteString(`{"` + base64Key + `":"`)
	buf.WriteString(base64.StdEncoding.EncodeToString(b))
	buf.WriteString(`"}`)
	return buf.Bytes(), nil
}

// encodeValue maps a value the driver read to its JSON form. dbType is the column's DuckDB type
// name (empty inside a STRUCT or MAP, whose member types the driver does not name); it tells a
// UUID from a BLOB and a DATE from a TIMESTAMP. The mapping is documented in docs/api.md.
func encodeValue(value interface{}, dbType string) interface{} {
	switch v := value.(type) {
	case nil:
		return nil
	case bool, string, int8, int16, int32, int64, int, uint8, uint16, uint32, uint64, uint:
		return v
	case float32:
		return sqlFloat{float64(v), 32}
	case float64:
		return sqlFloat{v, 64}
	case *big.Int:
		// HUGEINT, UHUGEINT and BIGNUM: an exact JSON integer.
		return json.Number(v.String())
	case duckdb.Decimal:
		// Exact, as a string: a JSON number would round through a double in most clients.
		return v.String()
	case []byte:
		if dbType == "UUID" && len(v) == 16 {
			return formatUUID(v)
		}
		return blob(v)
	case duckdb.UUID:
		return formatUUID(v[:])
	case time.Time:
		return formatTime(v, dbType)
	case duckdb.Interval:
		return map[string]interface{}{"months": v.Months, "days": v.Days, "micros": v.Micros}
	case []interface{}:
		elementType := listElementType(dbType)
		out := make([]interface{}, len(v))
		for i, element := range v {
			out[i] = encodeValue(element, elementType)
		}
		return out
	case map[string]interface{}:
		out := make(map[string]interface{}, len(v))
		for key, member := range v {
			out[key] = encodeValue(member, "")
		}
		return out
	case duckdb.OrderedMap:
		keys, values := v.Keys(), v.Values()
		out := make([]interface{}, len(keys))
		for i := range keys {
			out[i] = map[string]interface{}{"key": encodeValue(keys[i], ""), "value": encodeValue(values[i], "")}
		}
		return out
	case duckdb.Union:
		return map[string]interface{}{"tag": v.Tag, "value": encodeValue(v.Value, "")}
	default:
		return fmt.Sprint(v)
	}
}

// listElementType: "INTEGER[]" and "INTEGER[3]" hold INTEGER.
func listElementType(dbType string) string {
	if !strings.HasSuffix(dbType, "]") {
		return ""
	}
	open := strings.LastIndex(dbType, "[")
	if open < 0 {
		return ""
	}
	return dbType[:open]
}

func formatUUID(b []byte) string {
	text := hex.EncodeToString(b)
	return text[0:8] + "-" + text[8:12] + "-" + text[12:16] + "-" + text[16:20] + "-" + text[20:32]
}

// formatTime writes a DATE as 2026-09-26, a TIME as 10:11:12[.fraction], a TIMETZ with its offset,
// a TIMESTAMP (any precision) as 2026-09-26 10:11:12[.fraction] and a TIMESTAMPTZ, or a time inside
// a nested value, as RFC 3339 in UTC.
func formatTime(t time.Time, dbType string) string {
	switch dbType {
	case "DATE":
		return t.Format("2006-01-02")
	case "TIME":
		return t.Format("15:04:05.999999")
	case "TIMETZ":
		return t.Format("15:04:05.999999-07:00")
	case "TIMESTAMP", "TIMESTAMP_S", "TIMESTAMP_MS", "TIMESTAMP_NS":
		return t.UTC().Format("2006-01-02 15:04:05.999999999")
	}
	return t.UTC().Format(time.RFC3339Nano)
}

// valueSize estimates the bytes a value the driver read takes in the answer: the length of a string
// or blob, the sum of a nested value's members, and 8 for anything else. It bounds the response,
// so it only needs to grow with the real size, not match it.
func valueSize(value interface{}) int64 {
	switch v := value.(type) {
	case string:
		return int64(len(v)) + 8
	case []byte:
		return int64(len(v)) + 8
	case []interface{}:
		size := int64(8)
		for _, element := range v {
			size += valueSize(element)
		}
		return size
	case map[string]interface{}:
		size := int64(8)
		for key, member := range v {
			size += int64(len(key)) + valueSize(member)
		}
		return size
	case duckdb.OrderedMap:
		size := int64(8)
		for _, key := range v.Keys() {
			size += valueSize(key)
		}
		for _, member := range v.Values() {
			size += valueSize(member)
		}
		return size
	case duckdb.Union:
		return int64(len(v.Tag)) + valueSize(v.Value)
	default:
		return 8
	}
}
