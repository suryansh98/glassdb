package catalog

import (
	"path/filepath"
	"testing"

	"github.com/suryansh98/glassdb/pager"
	"github.com/suryansh98/glassdb/record"
	"github.com/suryansh98/glassdb/vfs"
)

var userCols = []Column{
	{Name: "id", Type: record.TInt, PK: true},
	{Name: "name", Type: record.TText},
	{Name: "score", Type: record.TReal},
}

func TestCreateAndReopen(t *testing.T) {
	path := filepath.Join(t.TempDir(), "t.db")
	p, err := pager.Open(vfs.OS{}, path, nil, pager.Options{})
	if err != nil {
		t.Fatal(err)
	}
	p.Begin()
	c, err := Open(p, nil)
	if err != nil {
		t.Fatal(err)
	}
	table, err := c.CreateTable("users", userCols)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := c.CreateIndex("idx_name", "users", "name"); err != nil {
		t.Fatal(err)
	}
	// Store two rows so NextRowid is meaningful after reopen.
	for i := uint64(1); i <= 2; i++ {
		row := record.EncodeRow([]record.Value{
			record.IntV(int64(i)), record.TextV("u"), record.RealV(1),
		})
		if err := table.Tree.Insert(record.EncodeKeyRowid(i), row); err != nil {
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
	c, err = Open(p, nil)
	if err != nil {
		t.Fatal(err)
	}
	got, ok := c.GetTable("users")
	if !ok {
		t.Fatal("users table lost")
	}
	if len(got.Columns) != 3 || got.Columns[1].Name != "name" || !got.Columns[0].PK {
		t.Fatalf("columns corrupted: %+v", got.Columns)
	}
	if got.NextRowid != 3 {
		t.Fatalf("NextRowid=%d, want 3", got.NextRowid)
	}
	idxs := c.IndexesFor("users")
	if len(idxs) != 1 || idxs[0].Column != "name" {
		t.Fatalf("indexes lost: %+v", idxs)
	}
	if got.PKColumn() != 0 || got.ColumnIndex("score") != 2 {
		t.Fatal("column lookups broken")
	}
}

func TestValidation(t *testing.T) {
	path := filepath.Join(t.TempDir(), "t.db")
	p, _ := pager.Open(vfs.OS{}, path, nil, pager.Options{})
	defer p.Close()
	p.Begin()
	defer p.Rollback()
	c, err := Open(p, nil)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := c.CreateTable("users", userCols); err != nil {
		t.Fatal(err)
	}

	cases := []struct {
		name string
		fn   func() error
	}{
		{"duplicate table", func() error { _, err := c.CreateTable("users", userCols); return err }},
		{"no columns", func() error { _, err := c.CreateTable("t2", nil); return err }},
		{"duplicate column", func() error {
			_, err := c.CreateTable("t3", []Column{{Name: "a", Type: record.TInt}, {Name: "a", Type: record.TText}})
			return err
		}},
		{"text pk", func() error {
			_, err := c.CreateTable("t4", []Column{{Name: "a", Type: record.TText, PK: true}})
			return err
		}},
		{"two pks", func() error {
			_, err := c.CreateTable("t5", []Column{{Name: "a", Type: record.TInt, PK: true}, {Name: "b", Type: record.TInt, PK: true}})
			return err
		}},
		{"index on missing table", func() error { _, err := c.CreateIndex("i1", "nope", "a"); return err }},
		{"index on missing column", func() error { _, err := c.CreateIndex("i2", "users", "nope"); return err }},
	}
	for _, tc := range cases {
		if tc.fn() == nil {
			t.Errorf("%s: expected error", tc.name)
		}
	}

	if _, err := c.CreateIndex("idx_name", "users", "name"); err != nil {
		t.Fatal(err)
	}
	if _, err := c.CreateIndex("idx_name2", "users", "name"); err == nil {
		t.Error("double-indexing a column should error")
	}
	if _, err := c.CreateIndex("idx_name", "users", "score"); err == nil {
		t.Error("duplicate index name should error")
	}
}

func TestReloadAfterRollback(t *testing.T) {
	path := filepath.Join(t.TempDir(), "t.db")
	p, _ := pager.Open(vfs.OS{}, path, nil, pager.Options{})
	defer p.Close()

	// Catalog bootstrap must be committed first (as db.Open does).
	p.Begin()
	c, err := Open(p, nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := p.Commit(); err != nil {
		t.Fatal(err)
	}

	p.Begin()
	if _, err := c.CreateTable("ghost", userCols); err != nil {
		t.Fatal(err)
	}
	if err := p.Rollback(); err != nil {
		t.Fatal(err)
	}
	if err := c.Reload(); err != nil {
		t.Fatal(err)
	}
	if _, ok := c.GetTable("ghost"); ok {
		t.Fatal("rolled-back table still visible after Reload")
	}
	if len(c.Tables()) != 0 {
		t.Fatalf("tables: %v", c.Tables())
	}
}
