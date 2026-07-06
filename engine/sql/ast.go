package sql

import (
	"fmt"
	"strings"

	"github.com/suryansh98/glassdb/record"
)

// Stmt is any parsed SQL statement.
type Stmt interface{ stmt() }

// Expr is any parsed expression.
type Expr interface {
	expr()
	String() string
}

// ColDef is one column definition in CREATE TABLE.
type ColDef struct {
	Name string
	Type record.Type
	PK   bool
}

type CreateTableStmt struct {
	Name string
	Cols []ColDef
}

type CreateIndexStmt struct {
	Name   string
	Table  string
	Column string
}

type InsertStmt struct {
	Table string
	Cols  []string // empty = all columns in table order
	Rows  [][]Expr
}

type OrderBy struct {
	Col  string
	Desc bool
}

type SelectStmt struct {
	Star      bool
	CountStar bool
	Exprs     []Expr
	Table     string
	Where     Expr
	Order     *OrderBy
	Limit     int64 // -1 = no limit
}

type SetClause struct {
	Col string
	Val Expr
}

type UpdateStmt struct {
	Table string
	Sets  []SetClause
	Where Expr
}

type DeleteStmt struct {
	Table string
	Where Expr
}

type BeginStmt struct{}
type CommitStmt struct{}
type RollbackStmt struct{}
type CheckpointStmt struct{}

type ExplainStmt struct {
	Inner Stmt
}

func (CreateTableStmt) stmt() {}
func (CreateIndexStmt) stmt() {}
func (InsertStmt) stmt()      {}
func (SelectStmt) stmt()      {}
func (UpdateStmt) stmt()      {}
func (DeleteStmt) stmt()      {}
func (BeginStmt) stmt()       {}
func (CommitStmt) stmt()      {}
func (RollbackStmt) stmt()    {}
func (CheckpointStmt) stmt()  {}
func (ExplainStmt) stmt()     {}

// Lit is a literal value.
type Lit struct {
	Val record.Value
}

// Col is a column reference.
type Col struct {
	Name string
}

// Unary is -x or NOT x.
type Unary struct {
	Op string
	X  Expr
}

// Binary is a two-operand expression (comparison, logic, arithmetic).
type Binary struct {
	Op   string
	L, R Expr
}

func (Lit) expr()    {}
func (Col) expr()    {}
func (Unary) expr()  {}
func (Binary) expr() {}

func (l Lit) String() string {
	if l.Val.Type == record.TText {
		return "'" + strings.ReplaceAll(l.Val.Text, "'", "''") + "'"
	}
	return l.Val.String()
}

func (c Col) String() string { return c.Name }

func (u Unary) String() string {
	if u.Op == "NOT" {
		return fmt.Sprintf("NOT %s", u.X)
	}
	return fmt.Sprintf("%s%s", u.Op, u.X)
}

func (b Binary) String() string {
	return fmt.Sprintf("(%s %s %s)", b.L, b.Op, b.R)
}
