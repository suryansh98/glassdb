package db

import (
	"fmt"
	"path/filepath"
	"strings"
	"testing"

	"github.com/suryansh98/glassdb/events"
	"github.com/suryansh98/glassdb/vfs"
)

func openTemp(t *testing.T, opts Options) (*DB, string) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "t.db")
	d, err := Open(path, opts)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { d.Close() })
	return d, path
}

func mustExec(t *testing.T, d *DB, src string) []Result {
	t.Helper()
	res, err := d.Exec(src)
	if err != nil {
		t.Fatalf("exec %q: %v", src, err)
	}
	return res
}

func rowStrings(res Result) [][]string {
	out := make([][]string, len(res.Rows))
	for i, row := range res.Rows {
		for _, v := range row {
			out[i] = append(out[i], v.String())
		}
	}
	return out
}

const usersDDL = "CREATE TABLE users (id INTEGER PRIMARY KEY, name TEXT, score REAL)"

func TestCreateInsertSelect(t *testing.T) {
	d, _ := openTemp(t, Options{})
	mustExec(t, d, usersDDL)
	res := mustExec(t, d, "INSERT INTO users (name, score) VALUES ('ada', 99.5), ('bob', 42), ('eve', 7)")
	if res[0].RowsAffected != 3 {
		t.Fatalf("affected=%d", res[0].RowsAffected)
	}
	sel := mustExec(t, d, "SELECT * FROM users")[0]
	if want := []string{"id", "name", "score"}; strings.Join(sel.Columns, ",") != strings.Join(want, ",") {
		t.Fatalf("columns %v", sel.Columns)
	}
	rows := rowStrings(sel)
	if len(rows) != 3 || rows[0][0] != "1" || rows[0][1] != "ada" || rows[2][1] != "eve" {
		t.Fatalf("rows %v", rows)
	}
}

func TestWhereProjectionArithmetic(t *testing.T) {
	d, _ := openTemp(t, Options{})
	mustExec(t, d, usersDDL)
	mustExec(t, d, "INSERT INTO users (name, score) VALUES ('a', 10), ('b', 20), ('c', 30)")
	res := mustExec(t, d, "SELECT name, score * 2 + 1 FROM users WHERE score > 10 AND score < 30")[0]
	rows := rowStrings(res)
	if len(rows) != 1 || rows[0][0] != "b" || rows[0][1] != "41" {
		t.Fatalf("rows %v", rows)
	}
	if res.Columns[1] != "((score * 2) + 1)" {
		t.Fatalf("expr column name %q", res.Columns[1])
	}
	// OR, NOT, unary minus, division
	res = mustExec(t, d, "SELECT name FROM users WHERE score / 10 = 1 OR NOT score < 25")[0]
	if len(res.Rows) != 2 {
		t.Fatalf("OR/NOT rows: %v", rowStrings(res))
	}
	if _, err := d.Exec("SELECT score / 0 FROM users"); err == nil || !strings.Contains(err.Error(), "division by zero") {
		t.Fatalf("division by zero: %v", err)
	}
}

func TestOrderByLimitCount(t *testing.T) {
	d, _ := openTemp(t, Options{})
	mustExec(t, d, usersDDL)
	mustExec(t, d, "INSERT INTO users (name, score) VALUES ('a', 3), ('b', 1), ('c', 2)")
	res := mustExec(t, d, "SELECT name FROM users ORDER BY score DESC LIMIT 2")[0]
	rows := rowStrings(res)
	if len(rows) != 2 || rows[0][0] != "a" || rows[1][0] != "c" {
		t.Fatalf("rows %v", rows)
	}
	res = mustExec(t, d, "SELECT COUNT(*) FROM users WHERE score >= 2")[0]
	if rowStrings(res)[0][0] != "2" {
		t.Fatalf("count %v", rowStrings(res))
	}
}

func TestNullSemantics(t *testing.T) {
	d, _ := openTemp(t, Options{})
	mustExec(t, d, usersDDL)
	mustExec(t, d, "INSERT INTO users (name, score) VALUES ('x', NULL), ('y', 5)")
	if rows := mustExec(t, d, "SELECT * FROM users WHERE score = NULL")[0].Rows; len(rows) != 0 {
		t.Fatalf("= NULL matched %d rows", len(rows))
	}
	if rows := mustExec(t, d, "SELECT * FROM users WHERE score > 0")[0].Rows; len(rows) != 1 {
		t.Fatalf("NULL leaked through comparison: %d rows", len(rows))
	}
	res := mustExec(t, d, "SELECT name, score + 1 FROM users WHERE name = 'x'")[0]
	if rowStrings(res)[0][1] != "NULL" {
		t.Fatalf("NULL arithmetic: %v", rowStrings(res))
	}
}

func TestUpdateDelete(t *testing.T) {
	d, _ := openTemp(t, Options{})
	mustExec(t, d, usersDDL)
	mustExec(t, d, "INSERT INTO users (name, score) VALUES ('a', 1), ('b', 2), ('c', 3)")
	res := mustExec(t, d, "UPDATE users SET score = score + 10 WHERE score >= 2")[0]
	if res.RowsAffected != 2 {
		t.Fatalf("update affected %d", res.RowsAffected)
	}
	rows := rowStrings(mustExec(t, d, "SELECT score FROM users ORDER BY score ASC")[0])
	if rows[0][0] != "1" || rows[1][0] != "12" || rows[2][0] != "13" {
		t.Fatalf("after update: %v", rows)
	}
	res = mustExec(t, d, "DELETE FROM users WHERE score > 11")[0]
	if res.RowsAffected != 2 {
		t.Fatalf("delete affected %d", res.RowsAffected)
	}
	if rows := mustExec(t, d, "SELECT COUNT(*) FROM users")[0]; rowStrings(rows)[0][0] != "1" {
		t.Fatalf("after delete: %v", rowStrings(rows))
	}
}

func drainCount(ch <-chan events.Event, typ string) int {
	n := 0
	for {
		select {
		case ev := <-ch:
			if ev.Type == typ {
				n++
			}
			continue
		default:
		}
		return n
	}
}

func TestPKLookupTouchesFewerPagesThanScan(t *testing.T) {
	bus := events.NewBus()
	d, _ := openTemp(t, Options{Bus: bus})
	mustExec(t, d, usersDDL)
	var sb strings.Builder
	sb.WriteString("INSERT INTO users (name, score) VALUES ")
	for i := 0; i < 1000; i++ {
		if i > 0 {
			sb.WriteString(", ")
		}
		fmt.Fprintf(&sb, "('user-%04d', %d)", i, i)
	}
	mustExec(t, d, sb.String())

	ch, cancel := bus.Subscribe(1 << 16)
	defer cancel()

	mustExec(t, d, "SELECT * FROM users WHERE id = 500")
	lookupReads := drainCount(ch, events.EvPageRead)

	mustExec(t, d, "SELECT COUNT(*) FROM users WHERE score >= 0")
	scanReads := drainCount(ch, events.EvPageRead)

	if lookupReads > 6 {
		t.Fatalf("PK lookup read %d pages, want <= 6", lookupReads)
	}
	if scanReads < lookupReads*5 {
		t.Fatalf("scan reads (%d) suspiciously close to lookup reads (%d)", scanReads, lookupReads)
	}
}

func TestIndexBackfillAndPlanner(t *testing.T) {
	bus := events.NewBus()
	d, _ := openTemp(t, Options{Bus: bus})
	mustExec(t, d, usersDDL)
	var sb strings.Builder
	sb.WriteString("INSERT INTO users (name, score) VALUES ")
	for i := 0; i < 100; i++ {
		if i > 0 {
			sb.WriteString(", ")
		}
		fmt.Fprintf(&sb, "('user-%02d', %d)", i, i)
	}
	mustExec(t, d, sb.String())
	res := mustExec(t, d, "CREATE INDEX idx_name ON users(name)")[0]
	if res.RowsAffected != 100 {
		t.Fatalf("backfill affected %d", res.RowsAffected)
	}

	ch, cancel := bus.Subscribe(1 << 16)
	defer cancel()
	sel := mustExec(t, d, "SELECT id FROM users WHERE name = 'user-50'")[0]
	if len(sel.Rows) != 1 || rowStrings(sel)[0][0] != "51" {
		t.Fatalf("index eq result: %v", rowStrings(sel))
	}
	planAccess := ""
	for {
		select {
		case ev := <-ch:
			if ev.Type == events.EvPlan {
				planAccess = ev.Fields["access"].(string)
			}
			continue
		default:
		}
		break
	}
	if !strings.Contains(planAccess, "index scan using idx_name") {
		t.Fatalf("planner chose %q", planAccess)
	}

	sel = mustExec(t, d, "SELECT COUNT(*) FROM users WHERE name >= 'user-90'")[0]
	if rowStrings(sel)[0][0] != "10" {
		t.Fatalf("index range count: %v", rowStrings(sel))
	}

	expl := mustExec(t, d, "EXPLAIN SELECT id FROM users WHERE name = 'user-50'")[0]
	text := ""
	for _, r := range rowStrings(expl) {
		text += r[0] + "\n"
	}
	if !strings.Contains(text, "index scan using idx_name (name = 'user-50')") {
		t.Fatalf("explain:\n%s", text)
	}

	expl = mustExec(t, d, "EXPLAIN SELECT * FROM users WHERE id > 10 ORDER BY score DESC LIMIT 3")[0]
	text = ""
	for _, r := range rowStrings(expl) {
		text += r[0] + "\n"
	}
	for _, want := range []string{"rowid range scan", "order: score DESC", "limit: 3"} {
		if !strings.Contains(text, want) {
			t.Fatalf("explain missing %q:\n%s", want, text)
		}
	}
}

func TestTransactions(t *testing.T) {
	d, path := openTemp(t, Options{})
	mustExec(t, d, usersDDL)

	mustExec(t, d, "BEGIN; INSERT INTO users (name) VALUES ('ghost'); ROLLBACK;")
	if rows := rowStrings(mustExec(t, d, "SELECT COUNT(*) FROM users")[0]); rows[0][0] != "0" {
		t.Fatalf("rollback leaked: %v", rows)
	}

	mustExec(t, d, "BEGIN; INSERT INTO users (name) VALUES ('kept'); COMMIT;")
	if err := d.Close(); err != nil {
		t.Fatal(err)
	}
	reopened, err := Open(path, Options{})
	if err != nil {
		t.Fatal(err)
	}
	defer reopened.Close()
	rows := rowStrings(mustExec(t, reopened, "SELECT name FROM users")[0])
	if len(rows) != 1 || rows[0][0] != "kept" {
		t.Fatalf("committed row lost: %v", rows)
	}

	// A rolled-back CREATE TABLE must vanish from the schema cache too.
	mustExec(t, reopened, "BEGIN; CREATE TABLE temp1 (a INTEGER); ROLLBACK;")
	if _, err := reopened.Exec("SELECT * FROM temp1"); err == nil {
		t.Fatal("rolled-back table still queryable")
	}
	if _, err := reopened.Exec("BEGIN; CHECKPOINT"); err == nil {
		t.Fatal("CHECKPOINT inside txn must error")
	}
	mustExec(t, reopened, "ROLLBACK")
}

func TestConstraintsAndErrors(t *testing.T) {
	d, _ := openTemp(t, Options{})
	mustExec(t, d, usersDDL)
	mustExec(t, d, "INSERT INTO users (id, name) VALUES (5, 'five')")

	cases := []struct {
		src, wantErr string
	}{
		{"INSERT INTO users (id, name) VALUES (5, 'dup')", "UNIQUE constraint"},
		{"INSERT INTO users (id) VALUES (0)", "PRIMARY KEY must be >= 1"},
		{"UPDATE users SET id = 9", "PRIMARY KEY is not supported"},
		{"INSERT INTO users (name) VALUES (42)", `column "name" wants TEXT`},
		{"INSERT INTO users (name) VALUES ('a', 'b')", "2 values for 1 columns"},
		{"INSERT INTO users (nope) VALUES (1)", "no such column"},
		{"SELECT * FROM missing", "no such table"},
		{"SELECT nope FROM users", "no such column"},
		{"SELECT name + 1 FROM users", "cannot apply"},
	}
	for _, tc := range cases {
		_, err := d.Exec(tc.src)
		if err == nil || !strings.Contains(err.Error(), tc.wantErr) {
			t.Errorf("%q: got error %v, want contains %q", tc.src, err, tc.wantErr)
		}
	}
	// The failed statements must not have left partial state behind.
	if rows := rowStrings(mustExec(t, d, "SELECT COUNT(*) FROM users")[0]); rows[0][0] != "1" {
		t.Fatalf("failed writes leaked rows: %v", rows)
	}
}

func TestCrashRecoveryThroughSQL(t *testing.T) {
	path := filepath.Join(t.TempDir(), "t.db")
	fault := vfs.NewFault(vfs.OS{})
	d, err := Open(path, Options{VFS: fault})
	if err != nil {
		t.Fatal(err)
	}
	mustExec(t, d, usersDDL)
	mustExec(t, d, "INSERT INTO users (name) VALUES ('committed')")

	fault.FailAfter(1)
	if _, err := d.Exec("INSERT INTO users (name) VALUES ('lost')"); err == nil {
		t.Fatal("write should have failed")
	}
	d.CrashClose()

	reopened, err := Open(path, Options{})
	if err != nil {
		t.Fatal(err)
	}
	defer reopened.Close()
	rows := rowStrings(mustExec(t, reopened, "SELECT name FROM users")[0])
	if len(rows) != 1 || rows[0][0] != "committed" {
		t.Fatalf("recovered rows: %v", rows)
	}
}

func TestSnapshotAndTree(t *testing.T) {
	d, _ := openTemp(t, Options{})
	mustExec(t, d, usersDDL)
	mustExec(t, d, "INSERT INTO users (name) VALUES ('a'), ('b')")
	mustExec(t, d, "CREATE INDEX idx_name ON users(name)")

	snap := d.Snapshot()
	if len(snap.Pages) < 3 || snap.Pages[0].Kind != "meta" {
		t.Fatalf("pages: %+v", snap.Pages)
	}
	if len(snap.Tables) != 1 || snap.Tables[0].Name != "users" || len(snap.Tables[0].Indexes) != 1 {
		t.Fatalf("tables: %+v", snap.Tables)
	}
	if snap.Cache.Cap != 256 {
		t.Fatalf("cache cap %d", snap.Cache.Cap)
	}

	dump, err := d.Tree("users")
	if err != nil {
		t.Fatal(err)
	}
	if dump.NCells != 2 || dump.Kind != "table-leaf" {
		t.Fatalf("tree dump: %+v", dump)
	}
	if _, err := d.Tree("idx_name"); err != nil {
		t.Fatal(err)
	}
	if _, err := d.Tree("nope"); err == nil {
		t.Fatal("Tree of missing object should error")
	}
}
