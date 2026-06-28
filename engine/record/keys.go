package record

import (
	"encoding/binary"
	"math"
)

// EncodeKeyRowid encodes a rowid as a table B+tree key. Big-endian, so
// bytes.Compare == numeric order.
func EncodeKeyRowid(rowid uint64) []byte {
	return binary.BigEndian.AppendUint64(nil, rowid)
}

// DecodeKeyRowid reverses EncodeKeyRowid.
func DecodeKeyRowid(key []byte) uint64 {
	return binary.BigEndian.Uint64(key)
}

// EncodeIndexKey builds an index B+tree key for (value, rowid) such that
// bytes.Compare sorts by value first (NULLs first), then rowid. The rowid
// suffix makes every index key unique, so duplicate column values are fine.
//
// Layout: tag byte (0x00 NULL, 0x01 present) + type payload + 8-byte rowid.
//   - INTEGER: uint64(v) with the sign bit flipped, big-endian.
//   - REAL: IEEE-754 bits; negative values are fully inverted, positive get
//     the sign bit set — the classic order-preserving float trick.
//   - TEXT: raw bytes with 0x00 escaped as 0x00 0x01, terminated by
//     0x00 0x00 (so a shorter string sorts before its extensions).
func EncodeIndexKey(v Value, rowid uint64) []byte {
	buf := make([]byte, 0, 16+len(v.Text))
	switch v.Type {
	case TNull:
		buf = append(buf, 0x00)
	case TInt:
		buf = append(buf, 0x01)
		buf = binary.BigEndian.AppendUint64(buf, uint64(v.Int)^(1<<63))
	case TReal:
		buf = append(buf, 0x01)
		bits := math.Float64bits(v.Real)
		if bits&(1<<63) != 0 {
			bits = ^bits
		} else {
			bits |= 1 << 63
		}
		buf = binary.BigEndian.AppendUint64(buf, bits)
	case TText:
		buf = append(buf, 0x01)
		for i := 0; i < len(v.Text); i++ {
			if v.Text[i] == 0x00 {
				buf = append(buf, 0x00, 0x01)
			} else {
				buf = append(buf, v.Text[i])
			}
		}
		buf = append(buf, 0x00, 0x00)
	}
	return binary.BigEndian.AppendUint64(buf, rowid)
}

// DecodeIndexKeyRowid extracts the rowid suffix of an index key.
func DecodeIndexKeyRowid(key []byte) uint64 {
	return binary.BigEndian.Uint64(key[len(key)-8:])
}

// IndexKeyPrefix encodes just the value part (no rowid), for use as a range
// bound: every key for value v satisfies prefix(v) <= key < prefix(v)+0xFF..
func IndexKeyPrefix(v Value) []byte {
	k := EncodeIndexKey(v, 0)
	return k[:len(k)-8]
}
