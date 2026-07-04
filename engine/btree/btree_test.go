package btree

import (
	"bytes"
	"fmt"
	"math/rand"
	"path/filepath"
	"testing"

	"github.com/suryansh98/glassdb/pager"
	"github.com/suryansh98/glassdb/record"
	"github.com/suryansh98/glassdb/vfs"
)

func newTestTree(t *testing.T, kind Kind) (*BTree, *pager.Pager) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "t.db")
	p, err := pager.Open(vfs.OS{}, path, nil, pager.Options{})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if p.InTxn() {
			p.Rollback()
		}
		p.Close()
	})
	if err := p.Begin(); err != nil {
		t.Fatal(err)
	}
	tree, err := New(p, nil, kind)
	if err != nil {
		t.Fatal(err)
	}
	return tree, p
}

func TestInsertGetScanLarge(t *testing.T) {
	tree, p := newTestTree(t, Table)
	const n = 5000

	perm := rand.New(rand.NewSource(1)).Perm(n)
	for _, i := range perm {
		key := record.EncodeKeyRowid(uint64(i))
		val := []byte(fmt.Sprintf("value-%d", i))
		if err := tree.Insert(key, val); err != nil {
			t.Fatal(err)
		}
	}
	if err := p.Commit(); err != nil {
		t.Fatal(err)
	}

	for i := 0; i < n; i++ {
		val, ok, err := tree.Get(record.EncodeKeyRowid(uint64(i)))
		if err != nil || !ok {
			t.Fatalf("key %d: ok=%v err=%v", i, ok, err)
		}
		if string(val) != fmt.Sprintf("value-%d", i) {
			t.Fatalf("key %d: wrong value %q", i, val)
		}
	}

	c, err := tree.First()
	if err != nil {
		t.Fatal(err)
	}
	count := 0
	var prev []byte
	for c.Valid() {
		if prev != nil && bytes.Compare(prev, c.Key()) >= 0 {
			t.Fatalf("scan out of order at %d", count)
		}
		prev = append(prev[:0], c.Key()...)
		count++
		if err := c.Next(); err != nil {
			t.Fatal(err)
		}
	}
	if count != n {
		t.Fatalf("scan visited %d keys, want %d", count, n)
	}

	max, ok, err := tree.MaxKey()
	if err != nil || !ok || record.DecodeKeyRowid(max) != n-1 {
		t.Fatalf("MaxKey=%v ok=%v err=%v", max, ok, err)
	}
}

func TestUpsertReplaces(t *testing.T) {
	tree, _ := newTestTree(t, Table)
	key := record.EncodeKeyRowid(7)
	tree.Insert(key, []byte("old"))
	if err := tree.Insert(key, []byte("new")); err != nil {
		t.Fatal(err)
	}
	val, ok, _ := tree.Get(key)
	if !ok || string(val) != "new" {
		t.Fatalf("got %q ok=%v", val, ok)
	}
	c, _ := tree.First()
	count := 0
	for c.Valid() {
		count++
		c.Next()
	}
	if count != 1 {
		t.Fatalf("upsert duplicated the key: %d cells", count)
	}
}

func TestDeleteAgainstMirror(t *testing.T) {
	tree, _ := newTestTree(t, Table)
	r := rand.New(rand.NewSource(2))
	mirror := make(map[uint64]bool)

	for i := 0; i < 2000; i++ {
		k := uint64(r.Intn(3000))
		mirror[k] = true
		if err := tree.Insert(record.EncodeKeyRowid(k), []byte("x")); err != nil {
			t.Fatal(err)
		}
	}
	for k := range mirror {
		if r.Intn(2) == 0 {
			found, err := tree.Delete(record.EncodeKeyRowid(k))
			if err != nil || !found {
				t.Fatalf("delete %d: found=%v err=%v", k, found, err)
			}
			delete(mirror, k)
		}
	}
	if found, _ := tree.Delete(record.EncodeKeyRowid(999999)); found {
		t.Fatal("delete of missing key reported found")
	}

	for k := uint64(0); k < 3000; k++ {
		_, ok, err := tree.Get(record.EncodeKeyRowid(k))
		if err != nil {
			t.Fatal(err)
		}
		if ok != mirror[k] {
			t.Fatalf("key %d: present=%v mirror=%v", k, ok, mirror[k])
		}
	}
	c, _ := tree.First()
	count := 0
	for c.Valid() {
		count++
		c.Next()
	}
	if count != len(mirror) {
		t.Fatalf("scan count %d != mirror %d", count, len(mirror))
	}
}

func TestDeepSplitCascade(t *testing.T) {
	tree, p := newTestTree(t, Table)
	val := bytes.Repeat([]byte("v"), 500) // ~7 cells per leaf
	const n = 5000
	for i := 0; i < n; i++ {
		if err := tree.Insert(record.EncodeKeyRowid(uint64(i)), val); err != nil {
			t.Fatal(err)
		}
	}
	if err := p.Commit(); err != nil {
		t.Fatal(err)
	}

	dump, err := tree.Structure(0)
	if err != nil {
		t.Fatal(err)
	}
	depth := 0
	for d := &dump; ; d = &d.Children[0] {
		depth++
		if len(d.Children) == 0 {
			break
		}
	}
	if depth < 3 {
		t.Fatalf("tree depth %d, want >= 3 (interior splits untested)", depth)
	}

	c, _ := tree.First()
	count := 0
	for c.Valid() {
		count++
		if err := c.Next(); err != nil {
			t.Fatal(err)
		}
	}
	if count != n {
		t.Fatalf("scan after deep splits: %d, want %d", count, n)
	}
}

func TestSeek(t *testing.T) {
	tree, _ := newTestTree(t, Table)
	for i := 0; i < 100; i += 2 { // even keys only
		tree.Insert(record.EncodeKeyRowid(uint64(i)), []byte("x"))
	}
	c, err := tree.Seek(record.EncodeKeyRowid(51))
	if err != nil {
		t.Fatal(err)
	}
	if !c.Valid() || record.DecodeKeyRowid(c.Key()) != 52 {
		t.Fatalf("seek(51) landed on %v", c.Key())
	}
	c, _ = tree.Seek(record.EncodeKeyRowid(9999))
	if c.Valid() {
		t.Fatal("seek past end should be invalid")
	}
}

func TestPayloadTooLarge(t *testing.T) {
	tree, _ := newTestTree(t, Table)
	err := tree.Insert(record.EncodeKeyRowid(1), bytes.Repeat([]byte("x"), MaxPayload))
	if err != ErrPayloadTooLarge {
		t.Fatalf("want ErrPayloadTooLarge, got %v", err)
	}
}

func TestMaxKeyWithEmptiedRightmostLeaf(t *testing.T) {
	tree, _ := newTestTree(t, Table)
	val := bytes.Repeat([]byte("v"), 300)
	for i := 0; i < 60; i++ {
		tree.Insert(record.EncodeKeyRowid(uint64(i)), val)
	}
	// Empty out the top of the tree so the rightmost leaf has no cells.
	for i := 40; i < 60; i++ {
		tree.Delete(record.EncodeKeyRowid(uint64(i)))
	}
	max, ok, err := tree.MaxKey()
	if err != nil || !ok {
		t.Fatalf("ok=%v err=%v", ok, err)
	}
	if got := record.DecodeKeyRowid(max); got != 39 {
		t.Fatalf("MaxKey=%d, want 39", got)
	}
}

func TestPersistsAcrossReopen(t *testing.T) {
	path := filepath.Join(t.TempDir(), "t.db")
	p, err := pager.Open(vfs.OS{}, path, nil, pager.Options{})
	if err != nil {
		t.Fatal(err)
	}
	p.Begin()
	tree, err := New(p, nil, Index)
	if err != nil {
		t.Fatal(err)
	}
	root := tree.Root()
	for i := 0; i < 500; i++ {
		key := record.EncodeIndexKey(record.TextV(fmt.Sprintf("user-%03d", i)), uint64(i))
		if err := tree.Insert(key, nil); err != nil {
			t.Fatal(err)
		}
	}
	if err := p.Commit(); err != nil {
		t.Fatal(err)
	}
	if err := p.Close(); err != nil {
		t.Fatal(err)
	}

	p, err = pager.Open(vfs.OS{}, path, nil, pager.Options{})
	if err != nil {
		t.Fatal(err)
	}
	defer p.Close()
	tree = Load(p, nil, root, Index)
	count := 0
	c, err := tree.First()
	if err != nil {
		t.Fatal(err)
	}
	for c.Valid() {
		count++
		c.Next()
	}
	if count != 500 {
		t.Fatalf("reopened scan count %d, want 500", count)
	}
}
