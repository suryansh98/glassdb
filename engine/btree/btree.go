// Package btree implements the B+tree that stores every table and index in
// glassdb. Keys and values are opaque byte strings ordered by bytes.Compare;
// package record produces encodings that make that ordering correct.
//
// Interior nodes hold separator cells (key, child) where child covers keys
// <= key, plus a rightmost child for everything greater. Leaves hold the
// actual values and are chained left-to-right for range scans. Roots never
// move: when a root fills up, its content moves into two children and the
// root becomes interior — so the catalog can store root page numbers
// permanently.
//
// Deletes are lazy (no merging or rebalancing); pages reclaim internally on
// rewrite but empty leaves stay in the chain. This is a documented v1
// trade-off, matching what several production engines shipped for years.
package btree

import (
	"bytes"
	"encoding/binary"
	"fmt"
	"sort"
	"strconv"

	"github.com/suryansh98/glassdb/events"
	"github.com/suryansh98/glassdb/pager"
)

// Kind selects table or index page kinds (they render differently in the
// X-ray and dump keys differently).
type Kind int

const (
	Table Kind = iota
	Index
)

// MaxPayload is the largest key+value size storable in one cell. Larger
// rows would need overflow pages, which v1 does not implement.
const MaxPayload = 1000

// ErrPayloadTooLarge is returned when key+value exceeds MaxPayload.
var ErrPayloadTooLarge = fmt.Errorf("btree: payload exceeds %d bytes (overflow pages not supported)", MaxPayload)

// BTree operates on one tree rooted at a fixed page.
type BTree struct {
	p    *pager.Pager
	bus  *events.Bus
	root uint32
	kind Kind
}

func (k Kind) leafKind() byte {
	if k == Table {
		return pager.KindTableLeaf
	}
	return pager.KindIndexLeaf
}

func (k Kind) interiorKind() byte {
	if k == Table {
		return pager.KindTableInterior
	}
	return pager.KindIndexInterior
}

// New allocates an empty tree (a single leaf root). Requires an active
// transaction.
func New(p *pager.Pager, bus *events.Bus, kind Kind) (*BTree, error) {
	pg, err := p.Allocate(kind.leafKind())
	if err != nil {
		return nil, err
	}
	root := pg.ID
	(&node{kind: kind.leafKind()}).write(pg)
	pg.Release()
	return &BTree{p: p, bus: bus, root: root, kind: kind}, nil
}

// Load attaches to an existing tree.
func Load(p *pager.Pager, bus *events.Bus, root uint32, kind Kind) *BTree {
	return &BTree{p: p, bus: bus, root: root, kind: kind}
}

// Root returns the root page number (stable for the tree's lifetime).
func (t *BTree) Root() uint32 { return t.root }

func (t *BTree) readNode(pgno uint32) (*node, error) {
	pg, err := t.p.Get(pgno)
	if err != nil {
		return nil, err
	}
	defer pg.Release()
	return parseNode(pg.Data)
}

func (t *BTree) writeNode(pgno uint32, n *node) error {
	pg, err := t.p.Get(pgno)
	if err != nil {
		return err
	}
	defer pg.Release()
	n.write(pg)
	return nil
}

// findCell returns the index of the first cell with key >= target, and
// whether it is an exact match.
func findCell(n *node, key []byte) (int, bool) {
	idx := sort.Search(len(n.cells), func(i int) bool {
		return bytes.Compare(n.cells[i].key, key) >= 0
	})
	found := idx < len(n.cells) && bytes.Equal(n.cells[idx].key, key)
	return idx, found
}

// childFor picks the child covering key in an interior node.
func childFor(n *node, key []byte) uint32 {
	idx, _ := findCell(n, key)
	if idx < len(n.cells) {
		return n.cells[idx].child
	}
	return n.next
}

type split struct {
	sep   []byte
	right uint32
}

// Insert upserts key -> val. Requires an active transaction.
func (t *BTree) Insert(key, val []byte) error {
	if len(key)+len(val) > MaxPayload {
		return ErrPayloadTooLarge
	}
	sp, err := t.insert(t.root, key, val, 0)
	if err != nil {
		return err
	}
	if sp == nil {
		return nil
	}
	// Root split: move the root's (already halved) content into a fresh
	// left child, then turn the root into an interior node over both
	// halves. The root page number never changes.
	rootNode, err := t.readNode(t.root)
	if err != nil {
		return err
	}
	leftPg, err := t.p.Allocate(rootNode.kind)
	if err != nil {
		return err
	}
	leftID := leftPg.ID
	rootNode.write(leftPg)
	leftPg.Release()

	newRoot := &node{
		kind:  t.kind.interiorKind(),
		next:  sp.right,
		cells: []cell{{key: sp.sep, child: leftID}},
	}
	if err := t.writeNode(t.root, newRoot); err != nil {
		return err
	}
	t.bus.Emit(events.EvBtreeSplit, events.F{
		"page": t.root, "newPage": leftID, "root": true,
	})
	return nil
}

func (t *BTree) insert(pgno uint32, key, val []byte, depth int) (*split, error) {
	n, err := t.readNode(pgno)
	if err != nil {
		return nil, err
	}
	if n.leaf() {
		idx, found := findCell(n, key)
		if found {
			n.cells[idx].val = val
		} else {
			n.cells = append(n.cells, cell{})
			copy(n.cells[idx+1:], n.cells[idx:])
			n.cells[idx] = cell{key: append([]byte(nil), key...), val: append([]byte(nil), val...)}
		}
		t.bus.Emit(events.EvBtreeInsert, events.F{"root": t.root, "page": pgno, "depth": depth})
		if n.size() <= pager.PageSize {
			return nil, t.writeNode(pgno, n)
		}
		return t.splitLeaf(pgno, n, depth)
	}

	child := childFor(n, key)
	sp, err := t.insert(child, key, val, depth+1)
	if err != nil || sp == nil {
		return nil, err
	}
	// The child split: child kept keys <= sp.sep, sp.right got the rest.
	// Insert (sep -> child) before the old reference and repoint the old
	// reference at the new right node.
	idx, _ := findCell(n, sp.sep)
	newCell := cell{key: sp.sep, child: child}
	if idx < len(n.cells) {
		n.cells[idx].child = sp.right
	} else {
		n.next = sp.right
	}
	n.cells = append(n.cells, cell{})
	copy(n.cells[idx+1:], n.cells[idx:])
	n.cells[idx] = newCell

	if n.size() <= pager.PageSize {
		return nil, t.writeNode(pgno, n)
	}
	return t.splitInterior(pgno, n, depth)
}

// splitLeaf divides an overflowing leaf at a byte-balanced point. The left
// half stays on the original page; the right half gets a new page spliced
// into the leaf chain.
func (t *BTree) splitLeaf(pgno uint32, n *node, depth int) (*split, error) {
	total := 0
	for i := range n.cells {
		total += n.cells[i].size(true)
	}
	splitIdx, acc := 0, 0
	for i := range n.cells {
		acc += n.cells[i].size(true)
		if acc >= total/2 {
			splitIdx = i + 1
			break
		}
	}
	if splitIdx <= 0 {
		splitIdx = 1
	}
	if splitIdx >= len(n.cells) {
		splitIdx = len(n.cells) - 1
	}

	rightPg, err := t.p.Allocate(n.kind)
	if err != nil {
		return nil, err
	}
	rightID := rightPg.ID
	right := &node{kind: n.kind, next: n.next, cells: n.cells[splitIdx:]}
	right.write(rightPg)
	rightPg.Release()

	n.cells = n.cells[:splitIdx]
	n.next = rightID
	if err := t.writeNode(pgno, n); err != nil {
		return nil, err
	}
	sep := append([]byte(nil), n.cells[len(n.cells)-1].key...)
	t.bus.Emit(events.EvBtreeSplit, events.F{
		"page": pgno, "newPage": rightID, "depth": depth,
	})
	return &split{sep: sep, right: rightID}, nil
}

// splitInterior promotes the middle separator: it moves up to the parent
// rather than staying in either half.
func (t *BTree) splitInterior(pgno uint32, n *node, depth int) (*split, error) {
	m := len(n.cells) / 2
	sep := n.cells[m].key
	midChild := n.cells[m].child

	rightPg, err := t.p.Allocate(n.kind)
	if err != nil {
		return nil, err
	}
	rightID := rightPg.ID
	right := &node{kind: n.kind, next: n.next, cells: n.cells[m+1:]}
	right.write(rightPg)
	rightPg.Release()

	n.cells = n.cells[:m]
	n.next = midChild // the promoted cell's child covers keys <= sep
	if err := t.writeNode(pgno, n); err != nil {
		return nil, err
	}
	t.bus.Emit(events.EvBtreeSplit, events.F{
		"page": pgno, "newPage": rightID, "depth": depth,
	})
	return &split{sep: sep, right: rightID}, nil
}

// Get returns the value stored under key.
func (t *BTree) Get(key []byte) ([]byte, bool, error) {
	pgno, depth := t.root, 0
	for {
		n, err := t.readNode(pgno)
		if err != nil {
			return nil, false, err
		}
		if n.leaf() {
			idx, found := findCell(n, key)
			t.bus.Emit(events.EvBtreeSearch, events.F{
				"root": t.root, "page": pgno, "depth": depth, "found": found,
			})
			if !found {
				return nil, false, nil
			}
			return n.cells[idx].val, true, nil
		}
		pgno = childFor(n, key)
		depth++
	}
}

// Delete removes key if present (lazy: no rebalancing) and reports whether
// it was found. Requires an active transaction when it was.
func (t *BTree) Delete(key []byte) (bool, error) {
	pgno := t.root
	for {
		n, err := t.readNode(pgno)
		if err != nil {
			return false, err
		}
		if n.leaf() {
			idx, found := findCell(n, key)
			if !found {
				return false, nil
			}
			n.cells = append(n.cells[:idx], n.cells[idx+1:]...)
			if err := t.writeNode(pgno, n); err != nil {
				return false, err
			}
			t.bus.Emit(events.EvBtreeDelete, events.F{"root": t.root, "page": pgno})
			return true, nil
		}
		pgno = childFor(n, key)
	}
}

// Cursor iterates leaf cells in key order. It reads one leaf at a time and
// is only valid while the tree is not mutated.
type Cursor struct {
	t    *BTree
	pgno uint32
	n    *node
	idx  int
}

// Seek positions a cursor at the first key >= target.
func (t *BTree) Seek(key []byte) (*Cursor, error) {
	pgno, depth := t.root, 0
	for {
		n, err := t.readNode(pgno)
		if err != nil {
			return nil, err
		}
		if n.leaf() {
			idx, _ := findCell(n, key)
			t.bus.Emit(events.EvBtreeSearch, events.F{
				"root": t.root, "page": pgno, "depth": depth,
			})
			c := &Cursor{t: t, pgno: pgno, n: n, idx: idx}
			return c, c.normalize()
		}
		pgno = childFor(n, key)
		depth++
	}
}

// First positions a cursor at the smallest key.
func (t *BTree) First() (*Cursor, error) {
	pgno := t.root
	for {
		n, err := t.readNode(pgno)
		if err != nil {
			return nil, err
		}
		if n.leaf() {
			c := &Cursor{t: t, pgno: pgno, n: n, idx: 0}
			return c, c.normalize()
		}
		if len(n.cells) > 0 {
			pgno = n.cells[0].child
		} else {
			pgno = n.next
		}
	}
}

// normalize advances past exhausted (or lazily-emptied) leaves.
func (c *Cursor) normalize() error {
	for c.n != nil && c.idx >= len(c.n.cells) {
		if c.n.next == 0 {
			c.n = nil
			return nil
		}
		next, err := c.t.readNode(c.n.next)
		if err != nil {
			return err
		}
		c.pgno = c.n.next
		c.n = next
		c.idx = 0
	}
	return nil
}

// Valid reports whether the cursor points at a cell.
func (c *Cursor) Valid() bool { return c.n != nil && c.idx < len(c.n.cells) }

// Key returns the current cell's key. Only valid when Valid().
func (c *Cursor) Key() []byte { return c.n.cells[c.idx].key }

// Value returns the current cell's value. Only valid when Valid().
func (c *Cursor) Value() []byte { return c.n.cells[c.idx].val }

// Next advances to the following key.
func (c *Cursor) Next() error {
	c.idx++
	return c.normalize()
}

// MaxKey returns the largest key in the tree, descending rightmost
// pointers. Lazy deletes can leave the rightmost leaf empty; in that rare
// case it falls back to a full scan.
func (t *BTree) MaxKey() ([]byte, bool, error) {
	pgno := t.root
	for {
		n, err := t.readNode(pgno)
		if err != nil {
			return nil, false, err
		}
		if !n.leaf() {
			pgno = n.next
			continue
		}
		if len(n.cells) > 0 {
			return n.cells[len(n.cells)-1].key, true, nil
		}
		break
	}
	// Rightmost leaf is empty: scan.
	var last []byte
	c, err := t.First()
	if err != nil {
		return nil, false, err
	}
	for c.Valid() {
		last = append(last[:0], c.Key()...)
		if err := c.Next(); err != nil {
			return nil, false, err
		}
	}
	return last, last != nil, nil
}

// TreeDump is a JSON-friendly rendering of (part of) the tree for the
// X-ray's /tree endpoint.
type TreeDump struct {
	Page      uint32     `json:"page"`
	Kind      string     `json:"kind"`
	NCells    int        `json:"nCells"`
	Keys      []string   `json:"keys,omitempty"`
	Children  []TreeDump `json:"children,omitempty"`
	NextLeaf  uint32     `json:"nextLeaf,omitempty"`
	Truncated bool       `json:"truncated,omitempty"`
}

const dumpMaxKeys = 8

// Structure renders the tree down to maxDepth levels (<=0 means unlimited).
func (t *BTree) Structure(maxDepth int) (TreeDump, error) {
	return t.dump(t.root, maxDepth)
}

func (t *BTree) dump(pgno uint32, remaining int) (TreeDump, error) {
	n, err := t.readNode(pgno)
	if err != nil {
		return TreeDump{}, err
	}
	d := TreeDump{Page: pgno, Kind: pager.KindName(n.kind), NCells: len(n.cells)}
	for i, c := range n.cells {
		if i >= dumpMaxKeys {
			d.Truncated = true
			break
		}
		d.Keys = append(d.Keys, t.renderKey(c.key))
	}
	if n.leaf() {
		d.NextLeaf = n.next
		return d, nil
	}
	if remaining == 1 {
		d.Truncated = true
		return d, nil
	}
	for _, c := range n.cells {
		child, err := t.dump(c.child, remaining-1)
		if err != nil {
			return TreeDump{}, err
		}
		d.Children = append(d.Children, child)
	}
	right, err := t.dump(n.next, remaining-1)
	if err != nil {
		return TreeDump{}, err
	}
	d.Children = append(d.Children, right)
	return d, nil
}

func (t *BTree) renderKey(key []byte) string {
	if t.kind == Table && len(key) == 8 {
		return strconv.FormatUint(binary.BigEndian.Uint64(key), 10)
	}
	if len(key) > 10 {
		return fmt.Sprintf("%x…", key[:10])
	}
	return fmt.Sprintf("%x", key)
}
