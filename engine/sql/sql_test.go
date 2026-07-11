package sql

import (
	"reflect"
	"strings"
	"testing"

	"github.com/suryansh98/glassdb/record"
)

func parseOne(t *testing.T, src string) Stmt {
	t.Helper()
	stmts, err := Parse(src)
	if err != nil {
		t.Fatalf("parse %q: %v", src, err)
	}
	if len(stmts) != 1 {
		t.Fatalf("parse %q: %d statements", src, len(stmts))
	}
	return stmts[0]
}

func TestParseStatements(t *testing.T) {
	cases := []struct {
		src  string
		want Stmt
	}{
		{
			"CREATE TABLE Users (id INTEGER PRIMARY KEY, name TEXT, score REAL)",
			CreateTableStmt{Name: "users", Cols: []ColDef{
				{Name: "id", Type: record.TInt, PK: true},
				{Name: "name", Type: record.TText},
				{Name: "score", Type: record.TReal},
			}},
		},
		{
			"create index idx_name on users(name)",
			CreateIndexStmt{Name: "idx_name", Table: "users", Column: "name"},
		},
		{
			"INSERT INTO users (name, score) VALUES ('ada', 99.5), ('bob''s', 42)",
			InsertStmt{Table: "users", Cols: []string{"name", "score"}, Rows: [][]Expr{
				{Lit{record.TextV("ada")}, Lit{record.RealV(99.5)}},
				{Lit{record.TextV("bob's")}, Lit{record.IntV(42)}},
			}},
		},
		{
			"INSERT INTO users VALUES (1, NULL)",
			InsertStmt{Table: "users", Rows: [][]Expr{
				{Lit{record.IntV(1)}, Lit{record.NullV()}},
			}},
		},
		{
			"SELECT * FROM users",
			SelectStmt{Star: true, Table: "users", Limit: -1},
		},
		{
			"SELECT COUNT(*) FROM users WHERE score > 50",
			SelectStmt{CountStar: true, Table: "users", Limit: -1,
				Where: Binary{Op: ">", L: Col{"score"}, R: Lit{record.IntV(50)}}},
		},
		{
			"SELECT name, score FROM users WHERE score >= 10 AND name != 'x' ORDER BY score DESC LIMIT 5",
			SelectStmt{
				Exprs: []Expr{Col{"name"}, Col{"score"}},
				Table: "users",
				Where: Binary{Op: "AND",
					L: Binary{Op: ">=", L: Col{"score"}, R: Lit{record.IntV(10)}},
					R: Binary{Op: "!=", L: Col{"name"}, R: Lit{record.TextV("x")}},
				},
				Order: &OrderBy{Col: "score", Desc: true},
				Limit: 5,
			},
		},
		{
			"UPDATE users SET score = score + 1, name = 'z' WHERE id = 3",
			UpdateStmt{Table: "users",
				Sets: []SetClause{
					{Col: "score", Val: Binary{Op: "+", L: Col{"score"}, R: Lit{record.IntV(1)}}},
					{Col: "name", Val: Lit{record.TextV("z")}},
				},
				Where: Binary{Op: "=", L: Col{"id"}, R: Lit{record.IntV(3)}},
			},
		},
		{
			"DELETE FROM users WHERE score < 10",
			DeleteStmt{Table: "users",
				Where: Binary{Op: "<", L: Col{"score"}, R: Lit{record.IntV(10)}}},
		},
		{"BEGIN", BeginStmt{}},
		{"COMMIT", CommitStmt{}},
		{"ROLLBACK", RollbackStmt{}},
		{"CHECKPOINT", CheckpointStmt{}},
		{
			"EXPLAIN SELECT * FROM users",
			ExplainStmt{Inner: SelectStmt{Star: true, Table: "users", Limit: -1}},
		},
	}
	for _, tc := range cases {
		got := parseOne(t, tc.src)
		if !reflect.DeepEqual(got, tc.want) {
			t.Errorf("parse %q:\n got  %#v\n want %#v", tc.src, got, tc.want)
		}
	}
}

func TestPrecedence(t *testing.T) {
	// a OR b AND c  →  a OR (b AND c)
	s := parseOne(t, "SELECT * FROM t WHERE a OR b AND c").(SelectStmt)
	or, ok := s.Where.(Binary)
	if !ok || or.Op != "OR" {
		t.Fatalf("top must be OR, got %v", s.Where)
	}
	if and, ok := or.R.(Binary); !ok || and.Op != "AND" {
		t.Fatalf("right of OR must be AND, got %v", or.R)
	}

	// 1 + 2 * 3  →  1 + (2 * 3)
	s = parseOne(t, "SELECT 1 + 2 * 3 FROM t").(SelectStmt)
	add := s.Exprs[0].(Binary)
	if add.Op != "+" {
		t.Fatalf("top must be +, got %v", add.Op)
	}
	if mul, ok := add.R.(Binary); !ok || mul.Op != "*" {
		t.Fatalf("right of + must be *, got %v", add.R)
	}

	// (1 + 2) * 3 respects parens
	s = parseOne(t, "SELECT (1 + 2) * 3 FROM t").(SelectStmt)
	if mul := s.Exprs[0].(Binary); mul.Op != "*" {
		t.Fatalf("parens ignored: %v", mul)
	}

	// comparison binds looser than arithmetic
	s = parseOne(t, "SELECT * FROM t WHERE a + 1 > b * 2").(SelectStmt)
	if cmp := s.Where.(Binary); cmp.Op != ">" {
		t.Fatalf("top must be >, got %v", cmp.Op)
	}

	// <> is normalized to !=
	s = parseOne(t, "SELECT * FROM t WHERE a <> 1").(SelectStmt)
	if cmp := s.Where.(Binary); cmp.Op != "!=" {
		t.Fatalf("<> not normalized: %v", cmp.Op)
	}

	// NOT and unary minus
	s = parseOne(t, "SELECT * FROM t WHERE NOT a = -1").(SelectStmt)
	if not := s.Where.(Unary); not.Op != "NOT" {
		t.Fatalf("top must be NOT, got %v", s.Where)
	}
}

func TestMultiStatement(t *testing.T) {
	stmts, err := Parse("BEGIN; INSERT INTO t VALUES (1); COMMIT;")
	if err != nil {
		t.Fatal(err)
	}
	if len(stmts) != 3 {
		t.Fatalf("got %d statements", len(stmts))
	}
	if _, err := Parse("-- just a comment\n"); err != nil {
		t.Fatal(err)
	}
}

func TestParseErrorsCarryPosition(t *testing.T) {
	cases := []struct {
		src     string
		wantPos string
	}{
		{"SELECT * FROM", "1:14"},
		{"CREATE TABLE t (a BANANA)", "1:19"},
		{"SELECT * FROM t WHERE", "1:22"},
		{"INSERT INTO t VALUES (1", "1:24"},
		{"SELECT * FROM t LIMIT 'x'", "1:23"},
		{"SELECT *\nFROM t WHERE @", "2:14"},
		{"SELECT * FROM t WHERE 'unterminated", "1:23"},
		{"EXPLAIN INSERT INTO t VALUES (1)", "1:9"},
	}
	for _, tc := range cases {
		_, err := Parse(tc.src)
		if err == nil {
			t.Errorf("parse %q: expected error", tc.src)
			continue
		}
		if !strings.HasPrefix(err.Error(), tc.wantPos+":") {
			t.Errorf("parse %q: error %q does not start with %q", tc.src, err, tc.wantPos)
		}
	}
}
