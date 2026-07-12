package exec

import (
	"bytes"
	"fmt"
	"math"
	"sort"

	"github.com/suryansh98/glassdb/catalog"
	"github.com/suryansh98/glassdb/events"
	"github.com/suryansh98/glassdb/record"
	"github.com/suryansh98/glassdb/sql"
)

// Result is the outcome of one statement.
type Result struct {
	Columns      []string         `json:"columns,omitempty"`
	Rows         [][]record.Value `json:"-"`
	RowsAffected int64            `json:"rowsAffected"`
}

// rowSource produces table rows in some order — the volcano model's Next().
type rowSource interface {
	next() (rowid uint64, row []record.Value, ok bool, err error)
}

// --- expression evaluation -------------------------------------------------

// evalExpr evaluates e against one row (nil for no-row contexts such as
// INSERT VALUES). NULL propagates through comparisons and arithmetic.
func evalExpr(e sql.Expr, t *catalog.Table, row []record.Value) (record.Value, error) {
	switch x := e.(type) {
	case sql.Lit:
		return x.Val, nil
	case sql.Col:
		if row == nil {
			return record.Value{}, fmt.Errorf("column reference %q is not allowed here", x.Name)
		}
		i := t.ColumnIndex(x.Name)
		if i < 0 {
			return record.Value{}, fmt.Errorf("no such column: %s", x.Name)
		}
		return row[i], nil
	case sql.Unary:
		v, err := evalExpr(x.X, t, row)
		if err != nil {
			return record.Value{}, err
		}
		if v.IsNull() {
			return record.NullV(), nil
		}
		switch x.Op {
		case "-":
			switch v.Type {
			case record.TInt:
				return record.IntV(-v.Int), nil
			case record.TReal:
				return record.RealV(-v.Real), nil
			}
			return record.Value{}, fmt.Errorf("cannot negate %s", v.Type)
		case "NOT":
			b, err := truthy(v)
			if err != nil {
				return record.Value{}, err
			}
			if b {
				return record.IntV(0), nil
			}
			return record.IntV(1), nil
		}
	case sql.Binary:
		return evalBinary(x, t, row)
	}
	return record.Value{}, fmt.Errorf("unsupported expression")
}

func evalBinary(b sql.Binary, t *catalog.Table, row []record.Value) (record.Value, error) {
	l, err := evalExpr(b.L, t, row)
	if err != nil {
		return record.Value{}, err
	}
	// AND/OR short-circuit where the left side already decides.
	if b.Op == "AND" || b.Op == "OR" {
		lb, lerr := boolOrNull(l)
		if lerr != nil {
			return record.Value{}, lerr
		}
		if b.Op == "AND" && lb != nil && !*lb {
			return record.IntV(0), nil
		}
		if b.Op == "OR" && lb != nil && *lb {
			return record.IntV(1), nil
		}
		r, err := evalExpr(b.R, t, row)
		if err != nil {
			return record.Value{}, err
		}
		rb, rerr := boolOrNull(r)
		if rerr != nil {
			return record.Value{}, rerr
		}
		// Three-valued logic with the undecided left side.
		if lb == nil || rb == nil {
			if b.Op == "AND" && rb != nil && !*rb {
				return record.IntV(0), nil
			}
			if b.Op == "OR" && rb != nil && *rb {
				return record.IntV(1), nil
			}
			return record.NullV(), nil
		}
		if b.Op == "AND" {
			return boolV(*lb && *rb), nil
		}
		return boolV(*lb || *rb), nil
	}

	r, err := evalExpr(b.R, t, row)
	if err != nil {
		return record.Value{}, err
	}
	switch b.Op {
	case "=", "!=", "<", "<=", ">", ">=":
		if l.IsNull() || r.IsNull() {
			return record.NullV(), nil // NULL compares to nothing
		}
		c := l.Compare(r)
		switch b.Op {
		case "=":
			return boolV(c == 0), nil
		case "!=":
			return boolV(c != 0), nil
		case "<":
			return boolV(c < 0), nil
		case "<=":
			return boolV(c <= 0), nil
		case ">":
			return boolV(c > 0), nil
		default:
			return boolV(c >= 0), nil
		}
	case "+", "-", "*", "/":
		if l.IsNull() || r.IsNull() {
			return record.NullV(), nil
		}
		if l.Type == record.TText || r.Type == record.TText {
			return record.Value{}, fmt.Errorf("cannot apply %q to TEXT", b.Op)
		}
		if l.Type == record.TInt && r.Type == record.TInt {
			switch b.Op {
			case "+":
				return record.IntV(l.Int + r.Int), nil
			case "-":
				return record.IntV(l.Int - r.Int), nil
			case "*":
				return record.IntV(l.Int * r.Int), nil
			default:
				if r.Int == 0 {
					return record.Value{}, fmt.Errorf("division by zero")
				}
				return record.IntV(l.Int / r.Int), nil
			}
		}
		lf, rf := l.Num(), r.Num()
		switch b.Op {
		case "+":
			return record.RealV(lf + rf), nil
		case "-":
			return record.RealV(lf - rf), nil
		case "*":
			return record.RealV(lf * rf), nil
		default:
			if rf == 0 {
				return record.Value{}, fmt.Errorf("division by zero")
			}
			return record.RealV(lf / rf), nil
		}
	}
	return record.Value{}, fmt.Errorf("unsupported operator %q", b.Op)
}

func boolV(b bool) record.Value {
	if b {
		return record.IntV(1)
	}
	return record.IntV(0)
}

// truthy interprets a value as a condition: nonzero numbers are true.
func truthy(v record.Value) (bool, error) {
	switch v.Type {
	case record.TInt:
		return v.Int != 0, nil
	case record.TReal:
		return v.Real != 0, nil
	case record.TNull:
		return false, nil
	}
	return false, fmt.Errorf("TEXT value used as a condition")
}

// boolOrNull maps a value to true/false/nil-for-NULL for 3-valued logic.
func boolOrNull(v record.Value) (*bool, error) {
	if v.IsNull() {
		return nil, nil
	}
	b, err := truthy(v)
	if err != nil {
		return nil, err
	}
	return &b, nil
}

// matches applies the WHERE clause; NULL results exclude the row.
func matches(where sql.Expr, t *catalog.Table, row []record.Value) (bool, error) {
	if where == nil {
		return true, nil
	}
	v, err := evalExpr(where, t, row)
	if err != nil {
		return false, err
	}
	if v.IsNull() {
		return false, nil
	}
	return truthy(v)
}

// coerce validates/adapts a value for a column (INTEGER literals fit REAL
// columns; everything else must match exactly; NULL always allowed).
func coerce(v record.Value, col catalog.Column) (record.Value, error) {
	if v.IsNull() {
		return v, nil
	}
	if v.Type == col.Type {
		return v, nil
	}
	if col.Type == record.TReal && v.Type == record.TInt {
		return record.RealV(float64(v.Int)), nil
	}
	return record.Value{}, fmt.Errorf("column %q wants %s, got %s", col.Name, col.Type, v.Type)
}

// --- row sources -------------------------------------------------------------

type tableCursorSource struct {
	t   *catalog.Table
	cur interface {
		Valid() bool
		Key() []byte
		Value() []byte
		Next() error
	}
	stop []byte // conservative inclusive upper bound on the btree key
}

func (s *tableCursorSource) next() (uint64, []record.Value, bool, error) {
	if !s.cur.Valid() {
		return 0, nil, false, nil
	}
	key := s.cur.Key()
	if s.stop != nil && bytes.Compare(key, s.stop) > 0 {
		return 0, nil, false, nil
	}
	rowid := record.DecodeKeyRowid(key)
	row, err := record.DecodeRow(s.cur.Value())
	if err != nil {
		return 0, nil, false, err
	}
	if err := s.cur.Next(); err != nil {
		return 0, nil, false, err
	}
	return rowid, row, true, nil
}

type pkLookupSource struct {
	t     *catalog.Table
	rowid uint64
	done  bool
}

func (s *pkLookupSource) next() (uint64, []record.Value, bool, error) {
	if s.done {
		return 0, nil, false, nil
	}
	s.done = true
	val, ok, err := s.t.Tree.Get(record.EncodeKeyRowid(s.rowid))
	if err != nil || !ok {
		return 0, nil, false, err
	}
	row, err := record.DecodeRow(val)
	if err != nil {
		return 0, nil, false, err
	}
	return s.rowid, row, true, nil
}

type indexSource struct {
	t   *catalog.Table
	cur interface {
		Valid() bool
		Key() []byte
		Next() error
	}
	stop []byte
}

func (s *indexSource) next() (uint64, []record.Value, bool, error) {
	if !s.cur.Valid() {
		return 0, nil, false, nil
	}
	key := s.cur.Key()
	if s.stop != nil && bytes.Compare(key, s.stop) > 0 {
		return 0, nil, false, nil
	}
	rowid := record.DecodeIndexKeyRowid(key)
	if err := s.cur.Next(); err != nil {
		return 0, nil, false, err
	}
	val, ok, err := s.t.Tree.Get(record.EncodeKeyRowid(rowid))
	if err != nil {
		return 0, nil, false, err
	}
	if !ok {
		return 0, nil, false, fmt.Errorf("index %s points at missing rowid %d", s.t.Name, rowid)
	}
	row, err := record.DecodeRow(val)
	if err != nil {
		return 0, nil, false, err
	}
	return rowid, row, true, nil
}

// openSource builds the row source for an access path.
func openSource(t *catalog.Table, a Access) (rowSource, error) {
	switch a.Kind {
	case PKLookup:
		if a.Eq.Int < 1 {
			return &pkLookupSource{done: true}, nil
		}
		return &pkLookupSource{t: t, rowid: uint64(a.Eq.Int)}, nil

	case PKRange:
		var start []byte
		if a.Lo != nil && a.Lo.Int > 1 {
			start = record.EncodeKeyRowid(uint64(a.Lo.Int))
		}
		var stop []byte
		if a.Hi != nil {
			if a.Hi.Int < 1 {
				return &pkLookupSource{done: true}, nil
			}
			stop = record.EncodeKeyRowid(uint64(a.Hi.Int))
		}
		cur, err := seekOrFirst(t, start)
		if err != nil {
			return nil, err
		}
		return &tableCursorSource{t: t, cur: cur, stop: stop}, nil

	case IndexEq:
		cur, err := a.Index.Tree.Seek(record.IndexKeyPrefix(*a.Eq))
		if err != nil {
			return nil, err
		}
		return &indexSource{t: t, cur: cur, stop: record.EncodeIndexKey(*a.Eq, math.MaxUint64)}, nil

	case IndexRange:
		var cur interface {
			Valid() bool
			Key() []byte
			Next() error
		}
		var err error
		if a.Lo != nil {
			cur, err = a.Index.Tree.Seek(record.IndexKeyPrefix(*a.Lo))
		} else {
			cur, err = a.Index.Tree.First()
		}
		if err != nil {
			return nil, err
		}
		var stop []byte
		if a.Hi != nil {
			stop = record.EncodeIndexKey(*a.Hi, math.MaxUint64)
		}
		return &indexSource{t: t, cur: cur, stop: stop}, nil

	default: // SeqScan
		cur, err := t.Tree.First()
		if err != nil {
			return nil, err
		}
		return &tableCursorSource{t: t, cur: cur}, nil
	}
}

func seekOrFirst(t *catalog.Table, start []byte) (interface {
	Valid() bool
	Key() []byte
	Value() []byte
	Next() error
}, error) {
	if start != nil {
		return t.Tree.Seek(start)
	}
	return t.Tree.First()
}

// --- statements ---------------------------------------------------------------

// Env carries what statement execution needs.
type Env struct {
	Cat *catalog.Catalog
	Bus *events.Bus
}

// Select runs a SELECT (or its COUNT(*) form).
func Select(env Env, s sql.SelectStmt) (Result, error) {
	t, ok := env.Cat.GetTable(s.Table)
	if !ok {
		return Result{}, fmt.Errorf("no such table: %s", s.Table)
	}
	access := planAccess(env.Cat, t, s.Where)
	fields := events.F{"table": t.Name, "access": access.String()}
	if access.Index != nil {
		fields["index"] = access.Index.Name
	}
	env.Bus.Emit(events.EvPlan, fields)

	src, err := openSource(t, access)
	if err != nil {
		return Result{}, err
	}

	if s.CountStar {
		var count int64
		for {
			_, row, ok, err := src.next()
			if err != nil {
				return Result{}, err
			}
			if !ok {
				break
			}
			m, err := matches(s.Where, t, row)
			if err != nil {
				return Result{}, err
			}
			if m {
				count++
			}
		}
		return Result{Columns: []string{"COUNT(*)"}, Rows: [][]record.Value{{record.IntV(count)}}}, nil
	}

	// Column names for the projection.
	var columns []string
	if s.Star {
		for _, c := range t.Columns {
			columns = append(columns, c.Name)
		}
	} else {
		for _, e := range s.Exprs {
			columns = append(columns, e.String())
		}
	}

	sortCol := -1
	if s.Order != nil {
		sortCol = t.ColumnIndex(s.Order.Col)
		if sortCol < 0 {
			return Result{}, fmt.Errorf("no such column in ORDER BY: %s", s.Order.Col)
		}
	}

	type outRow struct {
		sortKey record.Value
		vals    []record.Value
	}
	var out []outRow
	for {
		_, row, ok, err := src.next()
		if err != nil {
			return Result{}, err
		}
		if !ok {
			break
		}
		m, err := matches(s.Where, t, row)
		if err != nil {
			return Result{}, err
		}
		if !m {
			continue
		}
		var projected []record.Value
		if s.Star {
			projected = append(projected, row...)
		} else {
			for _, e := range s.Exprs {
				v, err := evalExpr(e, t, row)
				if err != nil {
					return Result{}, err
				}
				projected = append(projected, v)
			}
		}
		r := outRow{vals: projected}
		if sortCol >= 0 {
			r.sortKey = row[sortCol]
		}
		out = append(out, r)
	}

	if s.Order != nil {
		sort.SliceStable(out, func(i, j int) bool {
			c := out[i].sortKey.Compare(out[j].sortKey)
			if s.Order.Desc {
				return c > 0
			}
			return c < 0
		})
	}
	if s.Limit >= 0 && int64(len(out)) > s.Limit {
		out = out[:s.Limit]
	}

	rows := make([][]record.Value, len(out))
	for i, r := range out {
		rows[i] = r.vals
	}
	return Result{Columns: columns, Rows: rows}, nil
}

// CreateTable runs CREATE TABLE.
func CreateTable(env Env, s sql.CreateTableStmt) (Result, error) {
	cols := make([]catalog.Column, len(s.Cols))
	for i, c := range s.Cols {
		cols[i] = catalog.Column{Name: c.Name, Type: c.Type, PK: c.PK}
	}
	if _, err := env.Cat.CreateTable(s.Name, cols); err != nil {
		return Result{}, err
	}
	return Result{}, nil
}

// CreateIndex runs CREATE INDEX, backfilling existing rows.
func CreateIndex(env Env, s sql.CreateIndexStmt) (Result, error) {
	idx, err := env.Cat.CreateIndex(s.Name, s.Table, s.Column)
	if err != nil {
		return Result{}, err
	}
	t, _ := env.Cat.GetTable(s.Table)
	colIdx := t.ColumnIndex(s.Column)
	src := &tableCursorSource{t: t}
	cur, err := t.Tree.First()
	if err != nil {
		return Result{}, err
	}
	src.cur = cur
	var n int64
	for {
		rowid, row, ok, err := src.next()
		if err != nil {
			return Result{}, err
		}
		if !ok {
			break
		}
		if err := idx.Tree.Insert(record.EncodeIndexKey(row[colIdx], rowid), nil); err != nil {
			return Result{}, err
		}
		n++
	}
	return Result{RowsAffected: n}, nil
}

// Insert runs INSERT.
func Insert(env Env, s sql.InsertStmt) (Result, error) {
	t, ok := env.Cat.GetTable(s.Table)
	if !ok {
		return Result{}, fmt.Errorf("no such table: %s", s.Table)
	}
	// Resolve the target column positions.
	var positions []int
	if len(s.Cols) == 0 {
		positions = make([]int, len(t.Columns))
		for i := range t.Columns {
			positions[i] = i
		}
	} else {
		seen := make(map[string]bool)
		for _, name := range s.Cols {
			i := t.ColumnIndex(name)
			if i < 0 {
				return Result{}, fmt.Errorf("no such column: %s.%s", t.Name, name)
			}
			if seen[name] {
				return Result{}, fmt.Errorf("column %s specified twice", name)
			}
			seen[name] = true
			positions = append(positions, i)
		}
	}
	pkIdx := t.PKColumn()
	indexes := env.Cat.IndexesFor(t.Name)

	var n int64
	for _, exprRow := range s.Rows {
		if len(exprRow) != len(positions) {
			return Result{}, fmt.Errorf("%d values for %d columns", len(exprRow), len(positions))
		}
		vals := make([]record.Value, len(t.Columns))
		for i := range vals {
			vals[i] = record.NullV()
		}
		for i, e := range exprRow {
			v, err := evalExpr(e, t, nil)
			if err != nil {
				return Result{}, err
			}
			v, err = coerce(v, t.Columns[positions[i]])
			if err != nil {
				return Result{}, err
			}
			vals[positions[i]] = v
		}

		// Assign the rowid: explicit PK value or the next sequence value.
		var rowid uint64
		if pkIdx >= 0 && !vals[pkIdx].IsNull() {
			if vals[pkIdx].Int < 1 {
				return Result{}, fmt.Errorf("PRIMARY KEY must be >= 1")
			}
			rowid = uint64(vals[pkIdx].Int)
			if _, exists, err := t.Tree.Get(record.EncodeKeyRowid(rowid)); err != nil {
				return Result{}, err
			} else if exists {
				return Result{}, fmt.Errorf("UNIQUE constraint failed: %s.%s = %d", t.Name, t.Columns[pkIdx].Name, rowid)
			}
		} else {
			rowid = t.NextRowid
			if pkIdx >= 0 {
				vals[pkIdx] = record.IntV(int64(rowid))
			}
		}
		if rowid >= t.NextRowid {
			t.NextRowid = rowid + 1
		}

		if err := t.Tree.Insert(record.EncodeKeyRowid(rowid), record.EncodeRow(vals)); err != nil {
			return Result{}, err
		}
		for _, idx := range indexes {
			ci := t.ColumnIndex(idx.Column)
			if err := idx.Tree.Insert(record.EncodeIndexKey(vals[ci], rowid), nil); err != nil {
				return Result{}, err
			}
		}
		n++
	}
	return Result{RowsAffected: n}, nil
}

// collectMatches materializes (rowid, row) pairs matching WHERE, so that
// UPDATE and DELETE never mutate the tree under a live cursor.
func collectMatches(env Env, t *catalog.Table, where sql.Expr) ([]uint64, [][]record.Value, error) {
	access := planAccess(env.Cat, t, where)
	src, err := openSource(t, access)
	if err != nil {
		return nil, nil, err
	}
	var ids []uint64
	var rows [][]record.Value
	for {
		rowid, row, ok, err := src.next()
		if err != nil {
			return nil, nil, err
		}
		if !ok {
			break
		}
		m, err := matches(where, t, row)
		if err != nil {
			return nil, nil, err
		}
		if m {
			ids = append(ids, rowid)
			rows = append(rows, row)
		}
	}
	return ids, rows, nil
}

// Update runs UPDATE.
func Update(env Env, s sql.UpdateStmt) (Result, error) {
	t, ok := env.Cat.GetTable(s.Table)
	if !ok {
		return Result{}, fmt.Errorf("no such table: %s", s.Table)
	}
	pkIdx := t.PKColumn()
	type setOp struct {
		col int
		e   sql.Expr
	}
	var sets []setOp
	for _, sc := range s.Sets {
		i := t.ColumnIndex(sc.Col)
		if i < 0 {
			return Result{}, fmt.Errorf("no such column: %s.%s", t.Name, sc.Col)
		}
		if i == pkIdx {
			return Result{}, fmt.Errorf("updating the PRIMARY KEY is not supported")
		}
		sets = append(sets, setOp{col: i, e: sc.Val})
	}
	ids, rows, err := collectMatches(env, t, s.Where)
	if err != nil {
		return Result{}, err
	}
	indexes := env.Cat.IndexesFor(t.Name)

	for i, rowid := range ids {
		oldRow := rows[i]
		newRow := append([]record.Value(nil), oldRow...)
		for _, op := range sets {
			v, err := evalExpr(op.e, t, oldRow)
			if err != nil {
				return Result{}, err
			}
			v, err = coerce(v, t.Columns[op.col])
			if err != nil {
				return Result{}, err
			}
			newRow[op.col] = v
		}
		for _, idx := range indexes {
			ci := t.ColumnIndex(idx.Column)
			if oldRow[ci].Compare(newRow[ci]) != 0 || oldRow[ci].Type != newRow[ci].Type {
				if _, err := idx.Tree.Delete(record.EncodeIndexKey(oldRow[ci], rowid)); err != nil {
					return Result{}, err
				}
				if err := idx.Tree.Insert(record.EncodeIndexKey(newRow[ci], rowid), nil); err != nil {
					return Result{}, err
				}
			}
		}
		if err := t.Tree.Insert(record.EncodeKeyRowid(rowid), record.EncodeRow(newRow)); err != nil {
			return Result{}, err
		}
	}
	return Result{RowsAffected: int64(len(ids))}, nil
}

// Delete runs DELETE.
func Delete(env Env, s sql.DeleteStmt) (Result, error) {
	t, ok := env.Cat.GetTable(s.Table)
	if !ok {
		return Result{}, fmt.Errorf("no such table: %s", s.Table)
	}
	ids, rows, err := collectMatches(env, t, s.Where)
	if err != nil {
		return Result{}, err
	}
	indexes := env.Cat.IndexesFor(t.Name)
	for i, rowid := range ids {
		for _, idx := range indexes {
			ci := t.ColumnIndex(idx.Column)
			if _, err := idx.Tree.Delete(record.EncodeIndexKey(rows[i][ci], rowid)); err != nil {
				return Result{}, err
			}
		}
		if _, err := t.Tree.Delete(record.EncodeKeyRowid(rowid)); err != nil {
			return Result{}, err
		}
	}
	return Result{RowsAffected: int64(len(ids))}, nil
}
