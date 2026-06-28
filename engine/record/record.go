// Package record defines glassdb's value model and its two on-disk
// encodings: row encoding (how a table row is stored as a B+tree value) and
// order-preserving key encoding (how index keys are built so that plain
// bytes.Compare sorts them correctly).
package record

import (
	"encoding/binary"
	"fmt"
	"math"
	"strings"
)

// Type is a column/value type.
type Type byte

const (
	TNull Type = 0
	TInt  Type = 1
	TReal Type = 2
	TText Type = 3
)

func (t Type) String() string {
	switch t {
	case TNull:
		return "NULL"
	case TInt:
		return "INTEGER"
	case TReal:
		return "REAL"
	case TText:
		return "TEXT"
	}
	return fmt.Sprintf("Type(%d)", byte(t))
}

// Value is a single SQL value.
type Value struct {
	Type Type
	Int  int64
	Real float64
	Text string
}

func NullV() Value          { return Value{Type: TNull} }
func IntV(v int64) Value    { return Value{Type: TInt, Int: v} }
func RealV(v float64) Value { return Value{Type: TReal, Real: v} }
func TextV(v string) Value  { return Value{Type: TText, Text: v} }

// IsNull reports whether the value is NULL.
func (v Value) IsNull() bool { return v.Type == TNull }

// Num returns the value as float64 for numeric comparison/arithmetic.
func (v Value) Num() float64 {
	if v.Type == TInt {
		return float64(v.Int)
	}
	return v.Real
}

// Compare orders values: NULL < numbers (INTEGER and REAL compare
// numerically against each other) < TEXT. Comparing int64 to float64 goes
// through float64, which loses precision above 2^53 — acceptable at this
// engine's scale and documented here on purpose.
func (v Value) Compare(o Value) int {
	ca, cb := v.class(), o.class()
	if ca != cb {
		if ca < cb {
			return -1
		}
		return 1
	}
	switch ca {
	case 0: // both NULL
		return 0
	case 1: // numeric
		if v.Type == TInt && o.Type == TInt {
			switch {
			case v.Int < o.Int:
				return -1
			case v.Int > o.Int:
				return 1
			}
			return 0
		}
		a, b := v.Num(), o.Num()
		switch {
		case a < b:
			return -1
		case a > b:
			return 1
		}
		return 0
	default: // text
		return strings.Compare(v.Text, o.Text)
	}
}

// class: 0 = NULL, 1 = numeric, 2 = text.
func (v Value) class() int {
	switch v.Type {
	case TNull:
		return 0
	case TInt, TReal:
		return 1
	default:
		return 2
	}
}

func (v Value) String() string {
	switch v.Type {
	case TNull:
		return "NULL"
	case TInt:
		return fmt.Sprintf("%d", v.Int)
	case TReal:
		return fmt.Sprintf("%g", v.Real)
	default:
		return v.Text
	}
}

// EncodeRow serializes values as a table row: uvarint count, then per value
// a type tag followed by a type-specific payload (int: zigzag varint,
// real: 8-byte big-endian IEEE bits, text: uvarint length + bytes).
func EncodeRow(vals []Value) []byte {
	buf := binary.AppendUvarint(nil, uint64(len(vals)))
	for _, v := range vals {
		buf = append(buf, byte(v.Type))
		switch v.Type {
		case TNull:
		case TInt:
			buf = binary.AppendVarint(buf, v.Int)
		case TReal:
			buf = binary.BigEndian.AppendUint64(buf, math.Float64bits(v.Real))
		case TText:
			buf = binary.AppendUvarint(buf, uint64(len(v.Text)))
			buf = append(buf, v.Text...)
		}
	}
	return buf
}

// DecodeRow parses a row produced by EncodeRow.
func DecodeRow(buf []byte) ([]Value, error) {
	n, sz := binary.Uvarint(buf)
	if sz <= 0 {
		return nil, fmt.Errorf("record: corrupt row header")
	}
	buf = buf[sz:]
	vals := make([]Value, 0, n)
	for i := uint64(0); i < n; i++ {
		if len(buf) == 0 {
			return nil, fmt.Errorf("record: truncated row at value %d", i)
		}
		t := Type(buf[0])
		buf = buf[1:]
		switch t {
		case TNull:
			vals = append(vals, NullV())
		case TInt:
			v, sz := binary.Varint(buf)
			if sz <= 0 {
				return nil, fmt.Errorf("record: corrupt int at value %d", i)
			}
			buf = buf[sz:]
			vals = append(vals, IntV(v))
		case TReal:
			if len(buf) < 8 {
				return nil, fmt.Errorf("record: corrupt real at value %d", i)
			}
			vals = append(vals, RealV(math.Float64frombits(binary.BigEndian.Uint64(buf))))
			buf = buf[8:]
		case TText:
			l, sz := binary.Uvarint(buf)
			if sz <= 0 || uint64(len(buf)-sz) < l {
				return nil, fmt.Errorf("record: corrupt text at value %d", i)
			}
			buf = buf[sz:]
			vals = append(vals, TextV(string(buf[:l])))
			buf = buf[l:]
		default:
			return nil, fmt.Errorf("record: unknown type tag %d", t)
		}
	}
	return vals, nil
}
