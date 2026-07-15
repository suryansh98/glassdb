package sql

import "fmt"

// TokKind classifies a token.
type TokKind int

const (
	TokEOF TokKind = iota
	TokIdent
	TokKeyword
	TokInt
	TokFloat
	TokString
	TokSymbol
)

// Token is one lexical unit with its source position (1-based).
type Token struct {
	Kind  TokKind
	Text  string // keywords uppercased, identifiers lowercased
	Int   int64
	Float float64
	Line  int
	Col   int
}

func (t Token) String() string {
	if t.Kind == TokEOF {
		return "end of input"
	}
	return fmt.Sprintf("%q", t.Text)
}

// keywords recognized by the lexer (case-insensitive in source).
var keywords = map[string]bool{
	"SELECT": true, "FROM": true, "WHERE": true, "ORDER": true, "BY": true,
	"ASC": true, "DESC": true, "LIMIT": true,
	"INSERT": true, "INTO": true, "VALUES": true,
	"CREATE": true, "TABLE": true, "INDEX": true, "ON": true,
	"UPDATE": true, "SET": true, "DELETE": true,
	"BEGIN": true, "COMMIT": true, "ROLLBACK": true, "CHECKPOINT": true,
	"EXPLAIN": true, "PRIMARY": true, "KEY": true,
	"AND": true, "OR": true, "NOT": true, "NULL": true,
	"INTEGER": true, "INT": true, "REAL": true, "TEXT": true,
	"COUNT": true,
}
