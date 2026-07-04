package btree

import (
	"encoding/binary"
	"fmt"

	"github.com/suryansh98/glassdb/pager"
)

// On-disk node layout (a slotted page):
//
//	0      kind (pager.Kind*)
//	1      unused
//	2-3    nCells u16
//	4-5    cellContentStart u16
//	6-9    next u32 — leaf: right sibling (0 = none); interior: rightmost child
//	10+    slot array, one u16 page offset per cell, in key order
//
// Cell content grows from the end of the page toward the slot array.
//
//	leaf cell:     keyLen u16 | valLen u16 | key | val
//	interior cell: keyLen u16 | child u32  | key
//
// Nodes are parsed into memory, mutated, and serialized back whole. The
// disk format is a genuine slotted page; rewriting the page on change
// trades a few microseconds for a large reduction in pointer-surgery bugs,
// and implicitly compacts fragmentation on every write.
const nodeHdr = 10

type cell struct {
	key   []byte
	val   []byte // leaf only
	child uint32 // interior only
}

type node struct {
	kind  byte
	next  uint32
	cells []cell
}

func (n *node) leaf() bool {
	return n.kind == pager.KindTableLeaf || n.kind == pager.KindIndexLeaf
}

func (c *cell) size(leaf bool) int {
	if leaf {
		return 4 + len(c.key) + len(c.val)
	}
	return 6 + len(c.key)
}

// size returns the serialized byte size of the node.
func (n *node) size() int {
	s := nodeHdr + 2*len(n.cells)
	for i := range n.cells {
		s += n.cells[i].size(n.leaf())
	}
	return s
}

// parseNode reads a node out of page data, copying all byte slices so the
// result stays valid after the page is released or evicted.
func parseNode(data []byte) (*node, error) {
	n := &node{
		kind: data[0],
		next: binary.BigEndian.Uint32(data[6:10]),
	}
	nCells := int(binary.BigEndian.Uint16(data[2:4]))
	leaf := n.leaf()
	n.cells = make([]cell, nCells)
	for i := 0; i < nCells; i++ {
		off := int(binary.BigEndian.Uint16(data[nodeHdr+2*i:]))
		if off < nodeHdr || off+4 > len(data) {
			return nil, fmt.Errorf("btree: corrupt slot %d (offset %d)", i, off)
		}
		keyLen := int(binary.BigEndian.Uint16(data[off:]))
		if leaf {
			valLen := int(binary.BigEndian.Uint16(data[off+2:]))
			if off+4+keyLen+valLen > len(data) {
				return nil, fmt.Errorf("btree: corrupt leaf cell %d", i)
			}
			n.cells[i].key = append([]byte(nil), data[off+4:off+4+keyLen]...)
			n.cells[i].val = append([]byte(nil), data[off+4+keyLen:off+4+keyLen+valLen]...)
		} else {
			if off+6+keyLen > len(data) {
				return nil, fmt.Errorf("btree: corrupt interior cell %d", i)
			}
			n.cells[i].child = binary.BigEndian.Uint32(data[off+2:])
			n.cells[i].key = append([]byte(nil), data[off+6:off+6+keyLen]...)
		}
	}
	return n, nil
}

// write serializes the node into the page and marks it dirty. The caller
// must have verified n.size() <= pager.PageSize.
func (n *node) write(pg *pager.Page) {
	data := pg.Data
	for i := range data {
		data[i] = 0
	}
	data[0] = n.kind
	binary.BigEndian.PutUint16(data[2:4], uint16(len(n.cells)))
	binary.BigEndian.PutUint32(data[6:10], n.next)

	leaf := n.leaf()
	off := pager.PageSize
	for i := range n.cells {
		c := &n.cells[i]
		off -= c.size(leaf)
		binary.BigEndian.PutUint16(data[nodeHdr+2*i:], uint16(off))
		binary.BigEndian.PutUint16(data[off:], uint16(len(c.key)))
		if leaf {
			binary.BigEndian.PutUint16(data[off+2:], uint16(len(c.val)))
			copy(data[off+4:], c.key)
			copy(data[off+4+len(c.key):], c.val)
		} else {
			binary.BigEndian.PutUint32(data[off+2:], c.child)
			copy(data[off+6:], c.key)
		}
	}
	binary.BigEndian.PutUint16(data[4:6], uint16(off))
	pg.MarkDirty()
}
