package main

import (
	"strings"

	"github.com/suryansh98/glassdb/record"
)

// renderTable formats a result set with aligned columns, sqlite3-style.
func renderTable(columns []string, rows [][]record.Value) string {
	widths := make([]int, len(columns))
	for i, c := range columns {
		widths[i] = len(c)
	}
	cells := make([][]string, len(rows))
	for r, row := range rows {
		cells[r] = make([]string, len(row))
		for i, v := range row {
			s := v.String()
			cells[r][i] = s
			if i < len(widths) && len(s) > widths[i] {
				widths[i] = len(s)
			}
		}
	}

	var sb strings.Builder
	writeRow := func(vals []string) {
		for i, v := range vals {
			if i > 0 {
				sb.WriteString("  ")
			}
			sb.WriteString(v)
			if pad := widths[i] - len(v); pad > 0 && i < len(vals)-1 {
				sb.WriteString(strings.Repeat(" ", pad))
			}
		}
		sb.WriteString("\n")
	}
	writeRow(columns)
	seps := make([]string, len(columns))
	for i, w := range widths {
		seps[i] = strings.Repeat("-", w)
	}
	writeRow(seps)
	for _, row := range cells {
		writeRow(row)
	}
	return sb.String()
}
