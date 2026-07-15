// Package db is the engine facade: it owns the pager, catalog, and event
// bus, parses SQL, drives transactions (autocommit or explicit), and
// serializes all access — glassdb is a single-writer engine.
package db

import (
	"fmt"
	"sync"

	"github.com/suryansh98/glassdb/btree"
	"github.com/suryansh98/glassdb/catalog"
	"github.com/suryansh98/glassdb/events"
	"github.com/suryansh98/glassdb/exec"
	"github.com/suryansh98/glassdb/pager"
	"github.com/suryansh98/glassdb/record"
	"github.com/suryansh98/glassdb/sql"
	"github.com/suryansh98/glassdb/vfs"
)

// Options configures Open.
type Options struct {
	CacheSize       int         // buffer pool pages (default 256)
	CheckpointEvery int         // WAL frames before auto-checkpoint (default 1000)
	Bus             *events.Bus // nil = no instrumentation
	VFS             vfs.VFS     // nil = the OS filesystem
}

// Result re-exports exec.Result.
type Result = exec.Result

// DB is one open database.
type DB struct {
	mu          sync.Mutex
	p           *pager.Pager
	cat         *catalog.Catalog
	bus         *events.Bus
	explicitTxn bool
}

// Open opens (or creates) the database at path.
func Open(path string, opts Options) (*DB, error) {
	fs := opts.VFS
	if fs == nil {
		fs = vfs.OS{}
	}
	p, err := pager.Open(fs, path, opts.Bus, pager.Options{
		CacheSize:       opts.CacheSize,
		CheckpointEvery: opts.CheckpointEvery,
	})
	if err != nil {
		return nil, err
	}
	d := &DB{p: p, bus: opts.Bus}
	if p.CatalogRoot() == 0 {
		// First open: creating the catalog tree is itself a transaction.
		if err := p.Begin(); err != nil {
			return nil, err
		}
		cat, err := catalog.Open(p, opts.Bus)
		if err != nil {
			p.Rollback()
			p.Close()
			return nil, err
		}
		if err := p.Commit(); err != nil {
			p.Close()
			return nil, err
		}
		d.cat = cat
	} else {
		cat, err := catalog.Open(p, opts.Bus)
		if err != nil {
			p.Close()
			return nil, err
		}
		d.cat = cat
	}
	return d, nil
}

// Exec parses and runs one or more semicolon-separated statements,
// returning one Result per completed statement. Execution stops at the
// first error. Inside an explicit transaction the transaction stays open on
// error (like psql/sqlite3) so the caller can ROLLBACK.
func (d *DB) Exec(src string) ([]Result, error) {
	d.mu.Lock()
	defer d.mu.Unlock()

	stmts, err := sql.Parse(src)
	if err != nil {
		return nil, err
	}
	var results []Result
	for _, stmt := range stmts {
		d.bus.Emit(events.EvStmtStart, events.F{"stmt": stmtName(stmt)})
		res, err := d.execStmt(stmt)
		if err != nil {
			return results, err
		}
		d.bus.Emit(events.EvStmtEnd, events.F{
			"stmt": stmtName(stmt), "rows": len(res.Rows), "affected": res.RowsAffected,
		})
		results = append(results, res)
	}
	return results, nil
}

func stmtName(s sql.Stmt) string {
	switch s.(type) {
	case sql.CreateTableStmt:
		return "CREATE TABLE"
	case sql.CreateIndexStmt:
		return "CREATE INDEX"
	case sql.InsertStmt:
		return "INSERT"
	case sql.SelectStmt:
		return "SELECT"
	case sql.UpdateStmt:
		return "UPDATE"
	case sql.DeleteStmt:
		return "DELETE"
	case sql.BeginStmt:
		return "BEGIN"
	case sql.CommitStmt:
		return "COMMIT"
	case sql.RollbackStmt:
		return "ROLLBACK"
	case sql.CheckpointStmt:
		return "CHECKPOINT"
	case sql.ExplainStmt:
		return "EXPLAIN"
	}
	return "?"
}

func (d *DB) execStmt(stmt sql.Stmt) (Result, error) {
	env := exec.Env{Cat: d.cat, Bus: d.bus}
	switch s := stmt.(type) {
	case sql.BeginStmt:
		if d.explicitTxn {
			return Result{}, fmt.Errorf("transaction already active")
		}
		if err := d.p.Begin(); err != nil {
			return Result{}, err
		}
		d.explicitTxn = true
		d.bus.Emit(events.EvTxnBegin, nil)
		return Result{}, nil

	case sql.CommitStmt:
		if !d.explicitTxn {
			return Result{}, fmt.Errorf("no transaction active")
		}
		if err := d.p.Commit(); err != nil {
			return Result{}, err
		}
		d.explicitTxn = false
		d.bus.Emit(events.EvTxnCommit, nil)
		return Result{}, nil

	case sql.RollbackStmt:
		if !d.explicitTxn {
			return Result{}, fmt.Errorf("no transaction active")
		}
		if err := d.p.Rollback(); err != nil {
			return Result{}, err
		}
		if err := d.cat.Reload(); err != nil {
			return Result{}, err
		}
		d.explicitTxn = false
		d.bus.Emit(events.EvTxnRollback, nil)
		return Result{}, nil

	case sql.CheckpointStmt:
		if d.explicitTxn {
			return Result{}, fmt.Errorf("cannot CHECKPOINT inside a transaction")
		}
		return Result{}, d.p.Checkpoint()

	case sql.ExplainStmt:
		lines, err := exec.Explain(d.cat, s.Inner.(sql.SelectStmt))
		if err != nil {
			return Result{}, err
		}
		rows := make([][]record.Value, len(lines))
		for i, l := range lines {
			rows[i] = []record.Value{record.TextV(l)}
		}
		return Result{Columns: []string{"plan"}, Rows: rows}, nil

	case sql.SelectStmt:
		return exec.Select(env, s)

	case sql.CreateTableStmt:
		return d.write(func() (Result, error) { return exec.CreateTable(env, s) })
	case sql.CreateIndexStmt:
		return d.write(func() (Result, error) { return exec.CreateIndex(env, s) })
	case sql.InsertStmt:
		return d.write(func() (Result, error) { return exec.Insert(env, s) })
	case sql.UpdateStmt:
		return d.write(func() (Result, error) { return exec.Update(env, s) })
	case sql.DeleteStmt:
		return d.write(func() (Result, error) { return exec.Delete(env, s) })
	}
	return Result{}, fmt.Errorf("unsupported statement")
}

// write runs a mutating statement, wrapping it in an autocommit transaction
// unless an explicit one is open. A failed autocommit rolls back and the
// catalog cache is rebuilt from disk.
func (d *DB) write(fn func() (Result, error)) (Result, error) {
	if d.explicitTxn {
		return fn()
	}
	if err := d.p.Begin(); err != nil {
		return Result{}, err
	}
	d.bus.Emit(events.EvTxnBegin, events.F{"auto": true})
	res, err := fn()
	if err != nil {
		d.p.Rollback()
		d.cat.Reload()
		d.bus.Emit(events.EvTxnRollback, events.F{"auto": true})
		return Result{}, err
	}
	if err := d.p.Commit(); err != nil {
		d.p.Rollback()
		d.cat.Reload()
		return Result{}, err
	}
	d.bus.Emit(events.EvTxnCommit, events.F{"auto": true})
	return res, nil
}

// Close rolls back any open transaction, checkpoints, and closes files.
func (d *DB) Close() error {
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.explicitTxn {
		d.p.Rollback()
		d.explicitTxn = false
	}
	return d.p.Close()
}

// CrashClose abandons the database without flushing — the crash-recovery
// demo (and tests) use it to simulate the process dying.
func (d *DB) CrashClose() {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.p.CrashClose()
}

// --- snapshot for the X-ray ---------------------------------------------------

// PageInfo is one page in the file map.
type PageInfo struct {
	ID   uint32 `json:"id"`
	Kind string `json:"kind"`
}

// IndexInfo describes an index in the snapshot schema.
type IndexInfo struct {
	Name   string `json:"name"`
	Column string `json:"column"`
	Root   uint32 `json:"root"`
}

// ColumnInfo describes a column in the snapshot schema.
type ColumnInfo struct {
	Name string `json:"name"`
	Type string `json:"type"`
	PK   bool   `json:"pk,omitempty"`
}

// TableInfo describes a table in the snapshot schema.
type TableInfo struct {
	Name    string       `json:"name"`
	Columns []ColumnInfo `json:"columns"`
	Root    uint32       `json:"root"`
	Indexes []IndexInfo  `json:"indexes"`
}

// Snapshot is the X-ray's boot image of engine state.
type Snapshot struct {
	Pages  []PageInfo  `json:"pages"`
	Tables []TableInfo `json:"tables"`
	WAL    struct {
		Frames    int `json:"frames"`
		Committed int `json:"committed"`
	} `json:"wal"`
	Cache struct {
		Cap     int      `json:"cap"`
		Entries []uint32 `json:"entries"`
	} `json:"cache"`
	InTxn bool `json:"inTxn"`
}

// Snapshot captures current engine state without emitting events.
func (d *DB) Snapshot() Snapshot {
	d.mu.Lock()
	defer d.mu.Unlock()
	var s Snapshot
	for pgno := uint32(0); pgno < d.p.PageCount(); pgno++ {
		s.Pages = append(s.Pages, PageInfo{ID: pgno, Kind: pager.KindName(d.p.RawKind(pgno))})
	}
	for _, t := range d.cat.Tables() {
		ti := TableInfo{Name: t.Name, Root: t.Root, Indexes: []IndexInfo{}}
		for _, c := range t.Columns {
			ti.Columns = append(ti.Columns, ColumnInfo{Name: c.Name, Type: c.Type.String(), PK: c.PK})
		}
		for _, idx := range d.cat.IndexesFor(t.Name) {
			ti.Indexes = append(ti.Indexes, IndexInfo{Name: idx.Name, Column: idx.Column, Root: idx.Root})
		}
		s.Tables = append(s.Tables, ti)
	}
	s.WAL.Frames, s.WAL.Committed = d.p.WALFrames()
	s.Cache.Cap = d.p.CacheCap()
	s.Cache.Entries = d.p.CachedIDs()
	s.InTxn = d.p.InTxn()
	return s
}

// Tree dumps the B+tree structure of a table or index by name.
func (d *DB) Tree(name string) (btree.TreeDump, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	if t, ok := d.cat.GetTable(name); ok {
		return t.Tree.Structure(0)
	}
	if idx, ok := d.cat.GetIndex(name); ok {
		return idx.Tree.Structure(0)
	}
	return btree.TreeDump{}, fmt.Errorf("no such table or index: %s", name)
}
