// Package sql turns SQL text into an AST: a hand-written lexer and
// recursive-descent parser for glassdb's SQL subset.
package sql

import (
	"fmt"

	"github.com/suryansh98/glassdb/record"
)

// Parse parses zero or more semicolon-separated statements.
func Parse(src string) ([]Stmt, error) {
	toks, err := lex(src)
	if err != nil {
		return nil, err
	}
	p := &parser{toks: toks}
	var stmts []Stmt
	for {
		for p.acceptSymbol(";") {
		}
		if p.peek().Kind == TokEOF {
			return stmts, nil
		}
		s, err := p.parseStmt()
		if err != nil {
			return nil, err
		}
		stmts = append(stmts, s)
		if !p.acceptSymbol(";") && p.peek().Kind != TokEOF {
			return nil, p.errf("expected ';' or end of input, got %s", p.peek())
		}
	}
}

type parser struct {
	toks []Token
	pos  int
}

func (p *parser) peek() Token { return p.toks[p.pos] }

func (p *parser) next() Token {
	t := p.toks[p.pos]
	if t.Kind != TokEOF {
		p.pos++
	}
	return t
}

func (p *parser) errf(format string, args ...any) error {
	t := p.peek()
	return fmt.Errorf("%d:%d: %s", t.Line, t.Col, fmt.Sprintf(format, args...))
}

func (p *parser) acceptKeyword(kw string) bool {
	if t := p.peek(); t.Kind == TokKeyword && t.Text == kw {
		p.pos++
		return true
	}
	return false
}

func (p *parser) expectKeyword(kw string) error {
	if !p.acceptKeyword(kw) {
		return p.errf("expected %s, got %s", kw, p.peek())
	}
	return nil
}

func (p *parser) acceptSymbol(sym string) bool {
	if t := p.peek(); t.Kind == TokSymbol && t.Text == sym {
		p.pos++
		return true
	}
	return false
}

func (p *parser) expectSymbol(sym string) error {
	if !p.acceptSymbol(sym) {
		return p.errf("expected %q, got %s", sym, p.peek())
	}
	return nil
}

func (p *parser) expectIdent() (string, error) {
	if t := p.peek(); t.Kind == TokIdent {
		p.pos++
		return t.Text, nil
	}
	return "", p.errf("expected identifier, got %s", p.peek())
}

func (p *parser) parseStmt() (Stmt, error) {
	t := p.peek()
	if t.Kind != TokKeyword {
		return nil, p.errf("expected statement, got %s", t)
	}
	switch t.Text {
	case "CREATE":
		p.pos++
		if p.acceptKeyword("TABLE") {
			return p.parseCreateTable()
		}
		if p.acceptKeyword("INDEX") {
			return p.parseCreateIndex()
		}
		return nil, p.errf("expected TABLE or INDEX after CREATE")
	case "INSERT":
		p.pos++
		return p.parseInsert()
	case "SELECT":
		p.pos++
		return p.parseSelect()
	case "UPDATE":
		p.pos++
		return p.parseUpdate()
	case "DELETE":
		p.pos++
		return p.parseDelete()
	case "BEGIN":
		p.pos++
		return BeginStmt{}, nil
	case "COMMIT":
		p.pos++
		return CommitStmt{}, nil
	case "ROLLBACK":
		p.pos++
		return RollbackStmt{}, nil
	case "CHECKPOINT":
		p.pos++
		return CheckpointStmt{}, nil
	case "EXPLAIN":
		p.pos++
		start := p.peek()
		inner, err := p.parseStmt()
		if err != nil {
			return nil, err
		}
		if _, ok := inner.(SelectStmt); !ok {
			return nil, fmt.Errorf("%d:%d: EXPLAIN only supports SELECT", start.Line, start.Col)
		}
		return ExplainStmt{Inner: inner}, nil
	default:
		return nil, p.errf("unexpected keyword %s at statement start", t.Text)
	}
}

func (p *parser) parseType() (record.Type, error) {
	t := p.peek()
	if t.Kind == TokKeyword {
		switch t.Text {
		case "INTEGER", "INT":
			p.pos++
			return record.TInt, nil
		case "REAL":
			p.pos++
			return record.TReal, nil
		case "TEXT":
			p.pos++
			return record.TText, nil
		}
	}
	return 0, p.errf("expected column type (INTEGER, REAL, TEXT), got %s", t)
}

func (p *parser) parseCreateTable() (Stmt, error) {
	name, err := p.expectIdent()
	if err != nil {
		return nil, err
	}
	if err := p.expectSymbol("("); err != nil {
		return nil, err
	}
	var cols []ColDef
	for {
		colName, err := p.expectIdent()
		if err != nil {
			return nil, err
		}
		typ, err := p.parseType()
		if err != nil {
			return nil, err
		}
		col := ColDef{Name: colName, Type: typ}
		if p.acceptKeyword("PRIMARY") {
			if err := p.expectKeyword("KEY"); err != nil {
				return nil, err
			}
			col.PK = true
		}
		cols = append(cols, col)
		if p.acceptSymbol(",") {
			continue
		}
		break
	}
	if err := p.expectSymbol(")"); err != nil {
		return nil, err
	}
	return CreateTableStmt{Name: name, Cols: cols}, nil
}

func (p *parser) parseCreateIndex() (Stmt, error) {
	name, err := p.expectIdent()
	if err != nil {
		return nil, err
	}
	if err := p.expectKeyword("ON"); err != nil {
		return nil, err
	}
	table, err := p.expectIdent()
	if err != nil {
		return nil, err
	}
	if err := p.expectSymbol("("); err != nil {
		return nil, err
	}
	column, err := p.expectIdent()
	if err != nil {
		return nil, err
	}
	if err := p.expectSymbol(")"); err != nil {
		return nil, err
	}
	return CreateIndexStmt{Name: name, Table: table, Column: column}, nil
}

func (p *parser) parseInsert() (Stmt, error) {
	if err := p.expectKeyword("INTO"); err != nil {
		return nil, err
	}
	table, err := p.expectIdent()
	if err != nil {
		return nil, err
	}
	var cols []string
	if p.acceptSymbol("(") {
		for {
			c, err := p.expectIdent()
			if err != nil {
				return nil, err
			}
			cols = append(cols, c)
			if p.acceptSymbol(",") {
				continue
			}
			break
		}
		if err := p.expectSymbol(")"); err != nil {
			return nil, err
		}
	}
	if err := p.expectKeyword("VALUES"); err != nil {
		return nil, err
	}
	var rows [][]Expr
	for {
		if err := p.expectSymbol("("); err != nil {
			return nil, err
		}
		var row []Expr
		for {
			e, err := p.parseExpr()
			if err != nil {
				return nil, err
			}
			row = append(row, e)
			if p.acceptSymbol(",") {
				continue
			}
			break
		}
		if err := p.expectSymbol(")"); err != nil {
			return nil, err
		}
		rows = append(rows, row)
		if p.acceptSymbol(",") {
			continue
		}
		break
	}
	return InsertStmt{Table: table, Cols: cols, Rows: rows}, nil
}

func (p *parser) parseSelect() (Stmt, error) {
	s := SelectStmt{Limit: -1}
	switch {
	case p.acceptSymbol("*"):
		s.Star = true
	case p.peek().Kind == TokKeyword && p.peek().Text == "COUNT":
		p.pos++
		if err := p.expectSymbol("("); err != nil {
			return nil, err
		}
		if err := p.expectSymbol("*"); err != nil {
			return nil, err
		}
		if err := p.expectSymbol(")"); err != nil {
			return nil, err
		}
		s.CountStar = true
	default:
		for {
			e, err := p.parseExpr()
			if err != nil {
				return nil, err
			}
			s.Exprs = append(s.Exprs, e)
			if p.acceptSymbol(",") {
				continue
			}
			break
		}
	}
	if err := p.expectKeyword("FROM"); err != nil {
		return nil, err
	}
	table, err := p.expectIdent()
	if err != nil {
		return nil, err
	}
	s.Table = table
	if p.acceptKeyword("WHERE") {
		if s.Where, err = p.parseExpr(); err != nil {
			return nil, err
		}
	}
	if p.acceptKeyword("ORDER") {
		if err := p.expectKeyword("BY"); err != nil {
			return nil, err
		}
		col, err := p.expectIdent()
		if err != nil {
			return nil, err
		}
		ob := &OrderBy{Col: col}
		if p.acceptKeyword("DESC") {
			ob.Desc = true
		} else {
			p.acceptKeyword("ASC")
		}
		s.Order = ob
	}
	if p.acceptKeyword("LIMIT") {
		t := p.peek()
		if t.Kind != TokInt {
			return nil, p.errf("expected integer after LIMIT, got %s", t)
		}
		p.pos++
		s.Limit = t.Int
	}
	return s, nil
}

func (p *parser) parseUpdate() (Stmt, error) {
	table, err := p.expectIdent()
	if err != nil {
		return nil, err
	}
	if err := p.expectKeyword("SET"); err != nil {
		return nil, err
	}
	var sets []SetClause
	for {
		col, err := p.expectIdent()
		if err != nil {
			return nil, err
		}
		if err := p.expectSymbol("="); err != nil {
			return nil, err
		}
		val, err := p.parseExpr()
		if err != nil {
			return nil, err
		}
		sets = append(sets, SetClause{Col: col, Val: val})
		if p.acceptSymbol(",") {
			continue
		}
		break
	}
	u := UpdateStmt{Table: table, Sets: sets}
	if p.acceptKeyword("WHERE") {
		if u.Where, err = p.parseExpr(); err != nil {
			return nil, err
		}
	}
	return u, nil
}

func (p *parser) parseDelete() (Stmt, error) {
	if err := p.expectKeyword("FROM"); err != nil {
		return nil, err
	}
	table, err := p.expectIdent()
	if err != nil {
		return nil, err
	}
	d := DeleteStmt{Table: table}
	if p.acceptKeyword("WHERE") {
		if d.Where, err = p.parseExpr(); err != nil {
			return nil, err
		}
	}
	return d, nil
}

// Expression grammar, loosest to tightest:
// or → and (OR and)* · and → not (AND not)* · not → [NOT] cmp ·
// cmp → add ((= != < <= > >=) add)? · add → mul ((+ -) mul)* ·
// mul → unary ((* /) unary)* · unary → [-] primary ·
// primary → literal | column | ( or )
func (p *parser) parseExpr() (Expr, error) { return p.parseOr() }

func (p *parser) parseOr() (Expr, error) {
	l, err := p.parseAnd()
	if err != nil {
		return nil, err
	}
	for p.acceptKeyword("OR") {
		r, err := p.parseAnd()
		if err != nil {
			return nil, err
		}
		l = Binary{Op: "OR", L: l, R: r}
	}
	return l, nil
}

func (p *parser) parseAnd() (Expr, error) {
	l, err := p.parseNot()
	if err != nil {
		return nil, err
	}
	for p.acceptKeyword("AND") {
		r, err := p.parseNot()
		if err != nil {
			return nil, err
		}
		l = Binary{Op: "AND", L: l, R: r}
	}
	return l, nil
}

func (p *parser) parseNot() (Expr, error) {
	if p.acceptKeyword("NOT") {
		x, err := p.parseNot()
		if err != nil {
			return nil, err
		}
		return Unary{Op: "NOT", X: x}, nil
	}
	return p.parseCmp()
}

func (p *parser) parseCmp() (Expr, error) {
	l, err := p.parseAdd()
	if err != nil {
		return nil, err
	}
	for _, op := range []string{"=", "!=", "<=", ">=", "<", ">"} {
		if p.acceptSymbol(op) {
			r, err := p.parseAdd()
			if err != nil {
				return nil, err
			}
			return Binary{Op: op, L: l, R: r}, nil
		}
	}
	return l, nil
}

func (p *parser) parseAdd() (Expr, error) {
	l, err := p.parseMul()
	if err != nil {
		return nil, err
	}
	for {
		var op string
		switch {
		case p.acceptSymbol("+"):
			op = "+"
		case p.acceptSymbol("-"):
			op = "-"
		default:
			return l, nil
		}
		r, err := p.parseMul()
		if err != nil {
			return nil, err
		}
		l = Binary{Op: op, L: l, R: r}
	}
}

func (p *parser) parseMul() (Expr, error) {
	l, err := p.parseUnary()
	if err != nil {
		return nil, err
	}
	for {
		var op string
		switch {
		case p.acceptSymbol("*"):
			op = "*"
		case p.acceptSymbol("/"):
			op = "/"
		default:
			return l, nil
		}
		r, err := p.parseUnary()
		if err != nil {
			return nil, err
		}
		l = Binary{Op: op, L: l, R: r}
	}
}

func (p *parser) parseUnary() (Expr, error) {
	if p.acceptSymbol("-") {
		x, err := p.parseUnary()
		if err != nil {
			return nil, err
		}
		return Unary{Op: "-", X: x}, nil
	}
	return p.parsePrimary()
}

func (p *parser) parsePrimary() (Expr, error) {
	t := p.peek()
	switch t.Kind {
	case TokInt:
		p.pos++
		return Lit{Val: record.IntV(t.Int)}, nil
	case TokFloat:
		p.pos++
		return Lit{Val: record.RealV(t.Float)}, nil
	case TokString:
		p.pos++
		return Lit{Val: record.TextV(t.Text)}, nil
	case TokIdent:
		p.pos++
		return Col{Name: t.Text}, nil
	case TokKeyword:
		if t.Text == "NULL" {
			p.pos++
			return Lit{Val: record.NullV()}, nil
		}
	case TokSymbol:
		if t.Text == "(" {
			p.pos++
			e, err := p.parseExpr()
			if err != nil {
				return nil, err
			}
			if err := p.expectSymbol(")"); err != nil {
				return nil, err
			}
			return e, nil
		}
	}
	return nil, p.errf("expected expression, got %s", t)
}
