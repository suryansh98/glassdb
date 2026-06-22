// Package pager owns the database file: fixed-size pages, an LRU buffer
// pool, and the transaction cycle (begin / commit-through-WAL / rollback).
// Every other storage layer sees the database only as pages requested from
// here.
//
// Read path: buffer pool → WAL (newest committed image, or this
// transaction's own spilled frames) → main file. Write path: pages are
// dirtied in the pool and reach the WAL at commit; the main file is only
// written at checkpoint.
package pager

import (
	"container/list"
	"encoding/binary"
	"fmt"

	"github.com/suryansh98/glassdb/events"
	"github.com/suryansh98/glassdb/vfs"
	"github.com/suryansh98/glassdb/wal"
)

// PageSize is the size of every page in bytes.
const PageSize = 4096

// Page kinds, stored in byte 0 of every page.
const (
	KindFree          byte = 0x00
	KindMeta          byte = 0x01
	KindTableLeaf     byte = 0x02
	KindTableInterior byte = 0x03
	KindIndexLeaf     byte = 0x04
	KindIndexInterior byte = 0x05
)

// KindName returns a short human-readable page kind label.
func KindName(k byte) string {
	switch k {
	case KindMeta:
		return "meta"
	case KindTableLeaf:
		return "table-leaf"
	case KindTableInterior:
		return "table-interior"
	case KindIndexLeaf:
		return "index-leaf"
	case KindIndexInterior:
		return "index-interior"
	}
	return "free"
}

// Meta page (page 0) layout.
const (
	metaMagicOff       = 1  // "GLDB"
	metaVersionOff     = 5  // u16
	metaPageSizeOff    = 7  // u16
	metaPageCountOff   = 9  // u32
	metaCatalogRootOff = 13 // u32
)

var metaMagic = []byte("GLDB")

// Options configures a Pager.
type Options struct {
	CacheSize       int // buffer pool capacity in pages (default 256)
	CheckpointEvery int // auto-checkpoint when the WAL exceeds this many frames (default 1000)
}

// Page is a pinned page in the buffer pool. Callers mutate Data and must
// call MarkDirty afterwards, then Release when done.
type Page struct {
	ID   uint32
	Data []byte

	pins  int
	dirty bool
	elem  *list.Element
	pager *Pager
}

// MarkDirty registers the page for the current transaction's commit.
// Writing outside a transaction is an engine bug, hence the panic.
func (p *Page) MarkDirty() {
	if !p.pager.inTxn {
		panic("pager: page write outside transaction")
	}
	if !p.dirty {
		p.dirty = true
		p.pager.dirty[p.ID] = p
	}
	p.pager.bus.Emit(events.EvPageWrite, events.F{
		"page": p.ID, "kind": KindName(p.Data[0]),
	})
}

// Release unpins the page.
func (p *Page) Release() {
	if p.pins <= 0 {
		panic("pager: release of unpinned page")
	}
	p.pins--
}

// Pager manages one database file plus its WAL. Not safe for concurrent
// use; the db layer serializes access.
type Pager struct {
	fs   vfs.VFS
	file vfs.File
	wal  *wal.WAL
	bus  *events.Bus

	capacity        int
	checkpointEvery int

	cache map[uint32]*Page
	lru   *list.List // front = most recently used

	inTxn bool
	dirty map[uint32]*Page

	pageCount   uint32
	catalogRoot uint32

	committedPageCount   uint32
	committedCatalogRoot uint32
	metaDirty            bool
}

// Open opens (or creates) the database at path, running WAL recovery if a
// log is present.
func Open(fs vfs.VFS, path string, bus *events.Bus, opts Options) (*Pager, error) {
	if opts.CacheSize <= 0 {
		opts.CacheSize = 256
	}
	if opts.CheckpointEvery <= 0 {
		opts.CheckpointEvery = 1000
	}
	file, err := fs.Open(path)
	if err != nil {
		return nil, err
	}
	w, err := wal.Open(fs, path+"-wal", PageSize, bus)
	if err != nil {
		file.Close()
		return nil, err
	}
	p := &Pager{
		fs:              fs,
		file:            file,
		wal:             w,
		bus:             bus,
		capacity:        opts.CacheSize,
		checkpointEvery: opts.CheckpointEvery,
		cache:           make(map[uint32]*Page),
		lru:             list.New(),
		dirty:           make(map[uint32]*Page),
	}
	size, err := file.Size()
	if err != nil {
		return nil, err
	}
	if size == 0 && w.CommittedFrames() == 0 {
		// Brand-new database: bootstrap the meta page directly.
		p.pageCount = 1
		if _, err := file.WriteAt(p.buildMeta(), 0); err != nil {
			return nil, err
		}
		if err := file.Sync(); err != nil {
			return nil, err
		}
	} else {
		data, _, err := p.readPage(0)
		if err != nil {
			return nil, fmt.Errorf("pager: reading meta page: %w", err)
		}
		if err := p.loadMeta(data); err != nil {
			return nil, err
		}
	}
	p.committedPageCount = p.pageCount
	p.committedCatalogRoot = p.catalogRoot
	return p, nil
}

func (p *Pager) buildMeta() []byte {
	buf := make([]byte, PageSize)
	buf[0] = KindMeta
	copy(buf[metaMagicOff:], metaMagic)
	binary.BigEndian.PutUint16(buf[metaVersionOff:], 1)
	binary.BigEndian.PutUint16(buf[metaPageSizeOff:], PageSize)
	binary.BigEndian.PutUint32(buf[metaPageCountOff:], p.pageCount)
	binary.BigEndian.PutUint32(buf[metaCatalogRootOff:], p.catalogRoot)
	return buf
}

func (p *Pager) loadMeta(data []byte) error {
	if data[0] != KindMeta || string(data[metaMagicOff:metaMagicOff+4]) != string(metaMagic) {
		return fmt.Errorf("pager: not a glassdb file")
	}
	if v := binary.BigEndian.Uint16(data[metaVersionOff:]); v != 1 {
		return fmt.Errorf("pager: unsupported format version %d", v)
	}
	if ps := binary.BigEndian.Uint16(data[metaPageSizeOff:]); ps != PageSize {
		return fmt.Errorf("pager: unsupported page size %d", ps)
	}
	p.pageCount = binary.BigEndian.Uint32(data[metaPageCountOff:])
	p.catalogRoot = binary.BigEndian.Uint32(data[metaCatalogRootOff:])
	return nil
}

// readPage fetches a page image from the WAL or the main file, bypassing
// the cache. Returns the data and its source ("wal" or "file").
func (p *Pager) readPage(pgno uint32) ([]byte, string, error) {
	data, ok, err := p.wal.ReadPage(pgno)
	if err != nil {
		return nil, "", err
	}
	if ok {
		return data, "wal", nil
	}
	data = make([]byte, PageSize)
	if _, err := p.file.ReadAt(data, int64(pgno)*PageSize); err != nil {
		return nil, "", fmt.Errorf("pager: reading page %d: %w", pgno, err)
	}
	return data, "file", nil
}

// Get pins and returns page pgno.
func (p *Pager) Get(pgno uint32) (*Page, error) {
	if pgno == 0 {
		return nil, fmt.Errorf("pager: the meta page is managed by the pager")
	}
	if pgno >= p.pageCount {
		return nil, fmt.Errorf("pager: page %d out of range (page count %d)", pgno, p.pageCount)
	}
	if pg, ok := p.cache[pgno]; ok {
		p.lru.MoveToFront(pg.elem)
		pg.pins++
		p.bus.Emit(events.EvCacheHit, events.F{"page": pgno})
		p.bus.Emit(events.EvPageRead, events.F{
			"page": pgno, "src": "cache", "kind": KindName(pg.Data[0]),
		})
		return pg, nil
	}
	p.bus.Emit(events.EvCacheMiss, events.F{"page": pgno})
	if err := p.ensureRoom(); err != nil {
		return nil, err
	}
	data, src, err := p.readPage(pgno)
	if err != nil {
		return nil, err
	}
	pg := &Page{ID: pgno, Data: data, pins: 1, pager: p}
	pg.elem = p.lru.PushFront(pg)
	p.cache[pgno] = pg
	p.bus.Emit(events.EvPageRead, events.F{
		"page": pgno, "src": src, "kind": KindName(data[0]),
	})
	return pg, nil
}

// Allocate extends the database by one page of the given kind and returns
// it pinned and dirty.
func (p *Pager) Allocate(kind byte) (*Page, error) {
	if !p.inTxn {
		return nil, fmt.Errorf("pager: allocate outside transaction")
	}
	if err := p.ensureRoom(); err != nil {
		return nil, err
	}
	pgno := p.pageCount
	p.pageCount++
	p.metaDirty = true

	data := make([]byte, PageSize)
	data[0] = kind
	pg := &Page{ID: pgno, Data: data, pins: 1, dirty: true, pager: p}
	pg.elem = p.lru.PushFront(pg)
	p.cache[pgno] = pg
	p.dirty[pgno] = pg
	p.bus.Emit(events.EvPageWrite, events.F{"page": pgno, "kind": KindName(kind)})
	return pg, nil
}

// ensureRoom evicts unpinned pages (spilling dirty ones to the WAL) until
// the pool is under capacity. If everything is pinned the pool temporarily
// overflows rather than failing.
func (p *Pager) ensureRoom() error {
	for len(p.cache) >= p.capacity {
		var victim *Page
		for e := p.lru.Back(); e != nil; e = e.Prev() {
			if pg := e.Value.(*Page); pg.pins == 0 {
				victim = pg
				break
			}
		}
		if victim == nil {
			return nil // everything pinned; allow overflow
		}
		if victim.dirty {
			if err := p.wal.Append(victim.ID, victim.Data, 0); err != nil {
				return err
			}
			delete(p.dirty, victim.ID)
			p.bus.Emit(events.EvCacheSpill, events.F{"page": victim.ID})
		}
		p.lru.Remove(victim.elem)
		delete(p.cache, victim.ID)
		p.bus.Emit(events.EvCacheEvict, events.F{"page": victim.ID})
	}
	return nil
}

// Begin starts a write transaction.
func (p *Pager) Begin() error {
	if p.inTxn {
		return fmt.Errorf("pager: transaction already active")
	}
	p.inTxn = true
	return nil
}

// Commit appends all dirty pages to the WAL, ends with a commit-marked meta
// frame, and fsyncs. Auto-checkpoints when the WAL has grown past the
// configured threshold.
func (p *Pager) Commit() error {
	if !p.inTxn {
		return fmt.Errorf("pager: commit without transaction")
	}
	nothingSpilled := p.wal.Frames() == p.wal.CommittedFrames()
	if len(p.dirty) == 0 && !p.metaDirty && nothingSpilled {
		p.inTxn = false // read-only transaction
		return nil
	}
	for pgno, pg := range p.dirty {
		if err := p.wal.Append(pgno, pg.Data, 0); err != nil {
			return err
		}
		pg.dirty = false
	}
	// The meta page is always the commit frame: it carries the new page
	// count and catalog root, and its commitSize marker seals the txn.
	if err := p.wal.Append(0, p.buildMeta(), p.pageCount); err != nil {
		return err
	}
	if err := p.wal.Commit(); err != nil {
		return err
	}
	p.dirty = make(map[uint32]*Page)
	p.metaDirty = false
	p.committedPageCount = p.pageCount
	p.committedCatalogRoot = p.catalogRoot
	p.inTxn = false

	if p.wal.Frames() > p.checkpointEvery {
		return p.Checkpoint()
	}
	return nil
}

// Rollback discards the transaction: the WAL is truncated back to the last
// commit and the buffer pool is cleared (uncommitted images may be cached).
func (p *Pager) Rollback() error {
	if !p.inTxn {
		return fmt.Errorf("pager: rollback without transaction")
	}
	if err := p.wal.Rollback(); err != nil {
		return err
	}
	p.cache = make(map[uint32]*Page)
	p.lru = list.New()
	p.dirty = make(map[uint32]*Page)
	p.pageCount = p.committedPageCount
	p.catalogRoot = p.committedCatalogRoot
	p.metaDirty = false
	p.inTxn = false
	return nil
}

// Checkpoint copies committed WAL frames into the main file and resets the
// log.
func (p *Pager) Checkpoint() error {
	if p.inTxn {
		return fmt.Errorf("pager: checkpoint during transaction")
	}
	return p.wal.Checkpoint(p.file)
}

// Close checkpoints and closes both files.
func (p *Pager) Close() error {
	if p.inTxn {
		return fmt.Errorf("pager: close during transaction")
	}
	cpErr := p.Checkpoint()
	if err := p.wal.Close(); err != nil && cpErr == nil {
		cpErr = err
	}
	if err := p.file.Close(); err != nil && cpErr == nil {
		cpErr = err
	}
	return cpErr
}

// CrashClose releases the underlying file handles without checkpointing,
// committing, or flushing anything — it simulates the process dying. Crash
// tests need it because abandoned handles keep files locked on Windows.
func (p *Pager) CrashClose() {
	p.wal.Close()
	p.file.Close()
}

// PageCount returns the database size in pages (including the meta page).
func (p *Pager) PageCount() uint32 { return p.pageCount }

// CatalogRoot returns the catalog B+tree root page (0 = not yet created).
func (p *Pager) CatalogRoot() uint32 { return p.catalogRoot }

// SetCatalogRoot records the catalog root in the meta page.
func (p *Pager) SetCatalogRoot(pgno uint32) error {
	if !p.inTxn {
		return fmt.Errorf("pager: SetCatalogRoot outside transaction")
	}
	p.catalogRoot = pgno
	p.metaDirty = true
	return nil
}

// InTxn reports whether a transaction is active.
func (p *Pager) InTxn() bool { return p.inTxn }

// CacheLen returns the number of pages currently in the buffer pool.
func (p *Pager) CacheLen() int { return len(p.cache) }

// CacheCap returns the buffer pool capacity.
func (p *Pager) CacheCap() int { return p.capacity }

// CachedIDs returns the cached page numbers in LRU order (most recent
// first), for snapshots.
func (p *Pager) CachedIDs() []uint32 {
	ids := make([]uint32, 0, p.lru.Len())
	for e := p.lru.Front(); e != nil; e = e.Next() {
		ids = append(ids, e.Value.(*Page).ID)
	}
	return ids
}

// WALFrames returns (total, committed) WAL frame counts, for snapshots.
func (p *Pager) WALFrames() (int, int) {
	return p.wal.Frames(), p.wal.CommittedFrames()
}

// RawKind reports the page kind of pgno without touching the cache or
// emitting events (used by snapshots).
func (p *Pager) RawKind(pgno uint32) byte {
	if pg, ok := p.cache[pgno]; ok {
		return pg.Data[0]
	}
	if data, ok, _ := p.wal.ReadPage(pgno); ok {
		return data[0]
	}
	var b [1]byte
	if _, err := p.file.ReadAt(b[:], int64(pgno)*PageSize); err != nil {
		return KindFree
	}
	return b[0]
}
