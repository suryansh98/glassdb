package record

import (
	"bytes"
	"math"
	"math/rand"
	"testing"
)

func TestRowRoundTrip(t *testing.T) {
	rows := [][]Value{
		{},
		{NullV()},
		{IntV(0), IntV(-1), IntV(math.MaxInt64), IntV(math.MinInt64)},
		{RealV(0), RealV(-3.14), RealV(math.MaxFloat64)},
		{TextV(""), TextV("hello"), TextV("umlaut äöü and \x00 nul")},
		{IntV(42), NullV(), TextV("mixed"), RealV(2.5)},
	}
	for _, row := range rows {
		enc := EncodeRow(row)
		dec, err := DecodeRow(enc)
		if err != nil {
			t.Fatalf("decode %v: %v", row, err)
		}
		if len(dec) != len(row) {
			t.Fatalf("len %d != %d", len(dec), len(row))
		}
		for i := range row {
			if row[i].Compare(dec[i]) != 0 || row[i].Type != dec[i].Type {
				t.Fatalf("value %d: %v != %v", i, row[i], dec[i])
			}
		}
	}
}

func TestDecodeCorrupt(t *testing.T) {
	for _, buf := range [][]byte{{}, {5}, {1, 3, 200}, {1, 99}} {
		if _, err := DecodeRow(buf); err == nil {
			t.Fatalf("expected error for %v", buf)
		}
	}
}

func randomValue(r *rand.Rand) Value {
	switch r.Intn(4) {
	case 0:
		return NullV()
	case 1:
		return IntV(r.Int63() - r.Int63())
	case 2:
		return RealV((r.Float64() - 0.5) * math.Pow(10, float64(r.Intn(12))))
	default:
		n := r.Intn(12)
		b := make([]byte, n)
		for i := range b {
			b[i] = byte(r.Intn(128)) // includes 0x00 to exercise escaping
		}
		return TextV(string(b))
	}
}

// The core index-key property: bytes.Compare over encoded keys must match
// (value, rowid) comparison.
func TestIndexKeyOrderPreserving(t *testing.T) {
	r := rand.New(rand.NewSource(7))
	for i := 0; i < 2000; i++ {
		va, vb := randomValue(r), randomValue(r)
		ra, rb := uint64(r.Intn(1000)), uint64(r.Intn(1000))

		if va.Type != vb.Type && !va.IsNull() && !vb.IsNull() {
			// Columns are typed, so a single index never mixes INTEGER,
			// REAL, and TEXT payloads: cross-type byte order (beyond the
			// NULL tag) is deliberately unspecified.
			continue
		}
		want := va.Compare(vb)
		if want == 0 {
			switch {
			case ra < rb:
				want = -1
			case ra > rb:
				want = 1
			}
		}
		got := bytes.Compare(EncodeIndexKey(va, ra), EncodeIndexKey(vb, rb))
		if got != want {
			t.Fatalf("order mismatch: %v/%d vs %v/%d: got %d want %d",
				va, ra, vb, rb, got, want)
		}
	}
}

func TestRowidKeyOrder(t *testing.T) {
	prev := EncodeKeyRowid(0)
	for i := uint64(1); i < 10000; i += 37 {
		cur := EncodeKeyRowid(i)
		if bytes.Compare(prev, cur) >= 0 {
			t.Fatalf("rowid keys not increasing at %d", i)
		}
		if DecodeKeyRowid(cur) != i {
			t.Fatalf("round trip failed at %d", i)
		}
		prev = cur
	}
}

func TestIndexKeyPrefixBounds(t *testing.T) {
	v := TextV("ada")
	lo := IndexKeyPrefix(v)
	for _, rowid := range []uint64{0, 1, math.MaxUint64} {
		k := EncodeIndexKey(v, rowid)
		if !bytes.HasPrefix(k, lo) {
			t.Fatalf("key for rowid %d does not start with prefix", rowid)
		}
	}
	if bytes.HasPrefix(EncodeIndexKey(TextV("adb"), 0), lo) {
		t.Fatal("different value must not share prefix")
	}
}
