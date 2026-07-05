// Package catalog stores the schema — tables and indexes — inside the
// database itself, as rows in a dedicated catalog B+tree (the same trick as
// sqlite_master / pg_catalog). The meta page points at the catalog root;
// everything else is discovered from there.
package catalog

import (
	"encoding/json"
	"fmt"
	"sort"

	"github.com/suryansh98/glassdb/btree"
	"github.com/suryansh98/glassdb/events"
	"github.com/suryansh98/glassdb/pager"
	"github.com/suryansh98/glassdb/record"
)

// Column describes one table column.
type Column struct {
	Name string      `json:"name"`
	Type record.Type `json:"type"`
	PK   bool        `json:"pk,omitempty"`
}

// Table is a table's schema plus its loaded B+tree.
type Table struct {
	Name      string
	Columns   []Column
	Root      uint32
	NextRowid uint64
	Tree      *btree.BTree
}

// PKColumn returns the index of the INTEGER PRIMARY KEY (rowid alias)
// column, or -1.
func (t *Table) PKColumn() int {
	for i, c := range t.Columns {
		if c.PK {
			return i
		}
	}
	return -1
}

// ColumnIndex returns the position of the named column, or -1.
func (t *Table) ColumnIndex(name string) int {
	for i, c := range t.Columns {
		if c.Name == name {
			return i
		}
	}
	return -1
}

// Index is a secondary index's schema plus its loaded B+tree.
type Index struct {
	Name   string
	Table  string
	Column string
	Root   uint32
	Tree   *btree.BTree
}

// Catalog is the in-memory schema cache backed by the catalog B+tree.
type Catalog struct {
	p   *pager.Pager
	bus *events.Bus

	tree      *btree.BTree
	nextRowid uint64

	tables  map[string]*Table
	indexes map[string]*Index
	byTable map[string][]*Index
}

// Open loads the catalog, creating the catalog B+tree on first use (which
// requires an active transaction).
func Open(p *pager.Pager, bus *events.Bus) (*Catalog, error) {
	c := &Catalog{p: p, bus: bus}
	if p.CatalogRoot() == 0 {
		tree, err := btree.New(p, bus, btree.Table)
		if err != nil {
			return nil, err
		}
		if err := p.SetCatalogRoot(tree.Root()); err != nil {
			return nil, err
		}
		c.tree = tree
		c.resetMaps()
		c.nextRowid = 1
		return c, nil
	}
	c.tree = btree.Load(p, bus, p.CatalogRoot(), btree.Table)
	if err := c.load(); err != nil {
		return nil, err
	}
	return c, nil
}

func (c *Catalog) resetMaps() {
	c.tables = make(map[string]*Table)
	c.indexes = make(map[string]*Index)
	c.byTable = make(map[string][]*Index)
}

// load rescans the catalog tree and rebuilds the schema cache.
func (c *Catalog) load() error {
	c.resetMaps()
	c.nextRowid = 1

	cur, err := c.tree.First()
	if err != nil {
		return err
	}
	for cur.Valid() {
		c.nextRowid = record.DecodeKeyRowid(cur.Key()) + 1
		vals, err := record.DecodeRow(cur.Value())
		if err != nil {
			return fmt.Errorf("catalog: corrupt row: %w", err)
		}
		if len(vals) != 5 {
			return fmt.Errorf("catalog: row has %d values, want 5", len(vals))
		}
		name, kind := vals[0].Text, vals[1].Text
		tableName, detail := vals[2].Text, vals[3].Text
		root := uint32(vals[4].Int)
		switch kind {
		case "table":
			var cols []Column
			if err := json.Unmarshal([]byte(detail), &cols); err != nil {
				return fmt.Errorf("catalog: table %s columns: %w", name, err)
			}
			c.tables[name] = &Table{
				Name:    name,
				Columns: cols,
				Root:    root,
				Tree:    btree.Load(c.p, c.bus, root, btree.Table),
			}
		case "index":
			idx := &Index{
				Name:   name,
				Table:  tableName,
				Column: detail,
				Root:   root,
				Tree:   btree.Load(c.p, c.bus, root, btree.Index),
			}
			c.indexes[name] = idx
			c.byTable[tableName] = append(c.byTable[tableName], idx)
		default:
			return fmt.Errorf("catalog: unknown object kind %q", kind)
		}
		if err := cur.Next(); err != nil {
			return err
		}
	}
	// Next rowid per table = its largest rowid + 1.
	for _, t := range c.tables {
		max, ok, err := t.Tree.MaxKey()
		if err != nil {
			return err
		}
		if ok {
			t.NextRowid = record.DecodeKeyRowid(max) + 1
		} else {
			t.NextRowid = 1
		}
	}
	return nil
}

// Reload rebuilds the cache from disk. The db layer calls this after a
// rollback, since a rolled-back transaction may have created tables or
// indexes that only existed in memory.
func (c *Catalog) Reload() error {
	c.tree = btree.Load(c.p, c.bus, c.p.CatalogRoot(), btree.Table)
	return c.load()
}

func (c *Catalog) appendRow(vals []record.Value) error {
	key := record.EncodeKeyRowid(c.nextRowid)
	c.nextRowid++
	return c.tree.Insert(key, record.EncodeRow(vals))
}

// CreateTable validates the definition, allocates the table's B+tree, and
// records it. Requires an active transaction.
func (c *Catalog) CreateTable(name string, cols []Column) (*Table, error) {
	if _, ok := c.tables[name]; ok {
		return nil, fmt.Errorf("table %q already exists", name)
	}
	if len(cols) == 0 {
		return nil, fmt.Errorf("table %q needs at least one column", name)
	}
	seen := make(map[string]bool)
	pks := 0
	for _, col := range cols {
		if seen[col.Name] {
			return nil, fmt.Errorf("duplicate column %q", col.Name)
		}
		seen[col.Name] = true
		if col.PK {
			pks++
			if col.Type != record.TInt {
				return nil, fmt.Errorf("PRIMARY KEY column %q must be INTEGER (it aliases the rowid)", col.Name)
			}
		}
	}
	if pks > 1 {
		return nil, fmt.Errorf("at most one PRIMARY KEY column is supported")
	}

	tree, err := btree.New(c.p, c.bus, btree.Table)
	if err != nil {
		return nil, err
	}
	colsJSON, err := json.Marshal(cols)
	if err != nil {
		return nil, err
	}
	err = c.appendRow([]record.Value{
		record.TextV(name), record.TextV("table"), record.TextV(""),
		record.TextV(string(colsJSON)), record.IntV(int64(tree.Root())),
	})
	if err != nil {
		return nil, err
	}
	t := &Table{Name: name, Columns: cols, Root: tree.Root(), NextRowid: 1, Tree: tree}
	c.tables[name] = t
	return t, nil
}

// CreateIndex allocates an index B+tree and records it. The caller
// backfills existing rows. Requires an active transaction.
func (c *Catalog) CreateIndex(name, tableName, column string) (*Index, error) {
	if _, ok := c.indexes[name]; ok {
		return nil, fmt.Errorf("index %q already exists", name)
	}
	t, ok := c.tables[tableName]
	if !ok {
		return nil, fmt.Errorf("no such table: %s", tableName)
	}
	if t.ColumnIndex(column) < 0 {
		return nil, fmt.Errorf("no such column: %s.%s", tableName, column)
	}
	for _, idx := range c.byTable[tableName] {
		if idx.Column == column {
			return nil, fmt.Errorf("column %s.%s is already indexed by %q", tableName, column, idx.Name)
		}
	}

	tree, err := btree.New(c.p, c.bus, btree.Index)
	if err != nil {
		return nil, err
	}
	err = c.appendRow([]record.Value{
		record.TextV(name), record.TextV("index"), record.TextV(tableName),
		record.TextV(column), record.IntV(int64(tree.Root())),
	})
	if err != nil {
		return nil, err
	}
	idx := &Index{Name: name, Table: tableName, Column: column, Root: tree.Root(), Tree: tree}
	c.indexes[name] = idx
	c.byTable[tableName] = append(c.byTable[tableName], idx)
	return idx, nil
}

// GetTable returns a table by name.
func (c *Catalog) GetTable(name string) (*Table, bool) {
	t, ok := c.tables[name]
	return t, ok
}

// GetIndex returns an index by name.
func (c *Catalog) GetIndex(name string) (*Index, bool) {
	i, ok := c.indexes[name]
	return i, ok
}

// Tables returns all tables sorted by name.
func (c *Catalog) Tables() []*Table {
	out := make([]*Table, 0, len(c.tables))
	for _, t := range c.tables {
		out = append(out, t)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out
}

// IndexesFor returns the indexes on a table.
func (c *Catalog) IndexesFor(table string) []*Index {
	return c.byTable[table]
}
