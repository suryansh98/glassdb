package sql

import (
	"fmt"
	"strconv"
	"strings"
	"unicode"
)

// lex tokenizes src. SQL keywords and identifiers are case-insensitive;
// identifiers are normalized to lowercase. Strings use single quotes with
// ” as the escape. -- starts a comment running to end of line.
func lex(src string) ([]Token, error) {
	var toks []Token
	line, col := 1, 1
	i := 0
	advance := func(n int) {
		for j := 0; j < n; j++ {
			if src[i+j] == '\n' {
				line++
				col = 1
			} else {
				col++
			}
		}
		i += n
	}

	for i < len(src) {
		c := src[i]
		switch {
		case c == ' ' || c == '\t' || c == '\r' || c == '\n':
			advance(1)

		case c == '-' && i+1 < len(src) && src[i+1] == '-':
			for i < len(src) && src[i] != '\n' {
				advance(1)
			}

		case c == '\'':
			startLine, startCol := line, col
			advance(1)
			var sb strings.Builder
			closed := false
			for i < len(src) {
				if src[i] == '\'' {
					if i+1 < len(src) && src[i+1] == '\'' {
						sb.WriteByte('\'')
						advance(2)
						continue
					}
					advance(1)
					closed = true
					break
				}
				sb.WriteByte(src[i])
				advance(1)
			}
			if !closed {
				return nil, fmt.Errorf("%d:%d: unterminated string", startLine, startCol)
			}
			toks = append(toks, Token{Kind: TokString, Text: sb.String(), Line: startLine, Col: startCol})

		case c >= '0' && c <= '9':
			startLine, startCol := line, col
			start := i
			for i < len(src) && src[i] >= '0' && src[i] <= '9' {
				advance(1)
			}
			isFloat := false
			if i < len(src) && src[i] == '.' {
				isFloat = true
				advance(1)
				for i < len(src) && src[i] >= '0' && src[i] <= '9' {
					advance(1)
				}
			}
			text := src[start:i]
			if isFloat {
				f, err := strconv.ParseFloat(text, 64)
				if err != nil {
					return nil, fmt.Errorf("%d:%d: bad number %q", startLine, startCol, text)
				}
				toks = append(toks, Token{Kind: TokFloat, Text: text, Float: f, Line: startLine, Col: startCol})
			} else {
				n, err := strconv.ParseInt(text, 10, 64)
				if err != nil {
					return nil, fmt.Errorf("%d:%d: bad number %q", startLine, startCol, text)
				}
				toks = append(toks, Token{Kind: TokInt, Text: text, Int: n, Line: startLine, Col: startCol})
			}

		case c == '_' || unicode.IsLetter(rune(c)):
			startLine, startCol := line, col
			start := i
			for i < len(src) && (src[i] == '_' || unicode.IsLetter(rune(src[i])) || unicode.IsDigit(rune(src[i]))) {
				advance(1)
			}
			word := src[start:i]
			upper := strings.ToUpper(word)
			if keywords[upper] {
				toks = append(toks, Token{Kind: TokKeyword, Text: upper, Line: startLine, Col: startCol})
			} else {
				toks = append(toks, Token{Kind: TokIdent, Text: strings.ToLower(word), Line: startLine, Col: startCol})
			}

		default:
			startLine, startCol := line, col
			two := ""
			if i+1 < len(src) {
				two = src[i : i+2]
			}
			switch two {
			case "<=", ">=", "!=", "<>":
				text := two
				if text == "<>" {
					text = "!="
				}
				toks = append(toks, Token{Kind: TokSymbol, Text: text, Line: startLine, Col: startCol})
				advance(2)
				continue
			}
			switch c {
			case '(', ')', ',', ';', '*', '=', '<', '>', '+', '-', '/':
				toks = append(toks, Token{Kind: TokSymbol, Text: string(c), Line: startLine, Col: startCol})
				advance(1)
			default:
				return nil, fmt.Errorf("%d:%d: unexpected character %q", line, col, string(c))
			}
		}
	}
	toks = append(toks, Token{Kind: TokEOF, Line: line, Col: col})
	return toks, nil
}
