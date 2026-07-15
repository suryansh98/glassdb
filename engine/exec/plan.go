// Package exec plans and executes SQL statements against the catalog and
// storage layers. The planner is rule-based: it looks at the WHERE clause
// for predicates that can use the rowid (PRIMARY KEY alias) or a secondary
// index, and falls back to a sequential scan.
package exec

import (
	"fmt"
	"strings"

	"github.com/suryansh98/glassdb/catalog"
	"github.com/suryansh98/glassdb/record"
	"github.com/suryansh98/glassdb/sql"
)

// AccessKind is how the executor reaches a table's rows.
type AccessKind int

const (
	SeqScan AccessKind = iota
	PKLookup
	PKRange
	IndexEq
	IndexRange
)

// Access describes the chosen access path. Lo/Hi/Eq are conservative
// bounds: the executor always re-applies the full WHERE clause, so the
// bounds only decide which pages get touched, never correctness.
type Access struct {
	Kind   AccessKind
	Index  *catalog.Index
	Column string        // the column driving the access path
	Eq     *record.Value // equality probe
	Lo, Hi *record.Value // range bounds (inclusive, conservative)
}

// String renders the access path for EXPLAIN and plan events.
func (a Access) String() string {
	render := func(v *record.Value) string { return sql.Lit{Val: *v}.String() }
	bounds := func() string {
		var parts []string
		if a.Lo != nil {
			parts = append(parts, fmt.Sprintf("%s >= %s", a.Column, render(a.Lo)))
		}
		if a.Hi != nil {
			parts = append(parts, fmt.Sprintf("%s <= %s", a.Column, render(a.Hi)))
		}
		return strings.Join(parts, " AND ")
	}
	switch a.Kind {
	case PKLookup:
		return fmt.Sprintf("rowid lookup (%s = %s)", a.Column, render(a.Eq))
	case PKRange:
		return fmt.Sprintf("rowid range scan (%s)", bounds())
	case IndexEq:
		return fmt.Sprintf("index scan using %s (%s = %s)", a.Index.Name, a.Column, render(a.Eq))
	case IndexRange:
		return fmt.Sprintf("index range scan using %s (%s)", a.Index.Name, bounds())
	}
	return "sequential scan"
}

// conjuncts splits top-level ANDs: a AND b AND c → [a, b, c].
func conjuncts(e sql.Expr) []sql.Expr {
	if b, ok := e.(sql.Binary); ok && b.Op == "AND" {
		return append(conjuncts(b.L), conjuncts(b.R)...)
	}
	return []sql.Expr{e}
}

// colOpLit recognizes `col op literal` (either operand order), normalizing
// so the column is on the left.
func colOpLit(e sql.Expr) (col string, op string, val record.Value, ok bool) {
	b, isBin := e.(sql.Binary)
	if !isBin {
		return "", "", record.Value{}, false
	}
	switch b.Op {
	case "=", "<", "<=", ">", ">=":
	default:
		return "", "", record.Value{}, false
	}
	if c, isCol := b.L.(sql.Col); isCol {
		if l, isLit := b.R.(sql.Lit); isLit {
			return c.Name, b.Op, l.Val, true
		}
	}
	if c, isCol := b.R.(sql.Col); isCol {
		if l, isLit := b.L.(sql.Lit); isLit {
			flip := map[string]string{"=": "=", "<": ">", "<=": ">=", ">": "<", ">=": "<="}
			return c.Name, flip[b.Op], l.Val, true
		}
	}
	return "", "", record.Value{}, false
}

type candidate struct {
	eq     *record.Value
	lo, hi *record.Value
}

// planAccess picks the access path for a WHERE clause on a table.
func planAccess(cat *catalog.Catalog, t *catalog.Table, where sql.Expr) Access {
	cands := make(map[string]*candidate)
	if where != nil {
		for _, e := range conjuncts(where) {
			col, op, val, ok := colOpLit(e)
			if !ok || val.IsNull() {
				continue
			}
			c := cands[col]
			if c == nil {
				c = &candidate{}
				cands[col] = c
			}
			v := val
			switch op {
			case "=":
				c.eq = &v
			case ">", ">=":
				c.lo = &v // conservative: inclusive; the filter re-checks
			case "<", "<=":
				c.hi = &v
			}
		}
	}

	pkName := ""
	if i := t.PKColumn(); i >= 0 {
		pkName = t.Columns[i].Name
	}

	// Preference order: PK equality, index equality, PK range, index range.
	if c := cands[pkName]; c != nil && c.eq != nil && c.eq.Type == record.TInt {
		return Access{Kind: PKLookup, Column: pkName, Eq: c.eq}
	}
	for _, idx := range cat.IndexesFor(t.Name) {
		if c := cands[idx.Column]; c != nil && c.eq != nil {
			return Access{Kind: IndexEq, Index: idx, Column: idx.Column, Eq: c.eq}
		}
	}
	if c := cands[pkName]; c != nil && (c.lo != nil || c.hi != nil) {
		intOnly := func(v *record.Value) *record.Value {
			if v != nil && v.Type == record.TInt {
				return v
			}
			return nil
		}
		lo, hi := intOnly(c.lo), intOnly(c.hi)
		if lo != nil || hi != nil {
			return Access{Kind: PKRange, Column: pkName, Lo: lo, Hi: hi}
		}
	}
	for _, idx := range cat.IndexesFor(t.Name) {
		if c := cands[idx.Column]; c != nil && (c.lo != nil || c.hi != nil) {
			return Access{Kind: IndexRange, Index: idx, Column: idx.Column, Lo: c.lo, Hi: c.hi}
		}
	}
	return Access{Kind: SeqScan}
}

// Explain renders a SELECT plan as text lines.
func Explain(cat *catalog.Catalog, s sql.SelectStmt) ([]string, error) {
	t, ok := cat.GetTable(s.Table)
	if !ok {
		return nil, fmt.Errorf("no such table: %s", s.Table)
	}
	access := planAccess(cat, t, s.Where)
	lines := []string{
		fmt.Sprintf("SELECT on %s", t.Name),
		fmt.Sprintf("access: %s", access),
	}
	if s.Where != nil {
		lines = append(lines, fmt.Sprintf("filter: %s", s.Where))
	}
	if s.Order != nil {
		dir := "ASC"
		if s.Order.Desc {
			dir = "DESC"
		}
		lines = append(lines, fmt.Sprintf("order: %s %s (in-memory sort)", s.Order.Col, dir))
	}
	if s.Limit >= 0 {
		lines = append(lines, fmt.Sprintf("limit: %d", s.Limit))
	}
	return lines, nil
}
