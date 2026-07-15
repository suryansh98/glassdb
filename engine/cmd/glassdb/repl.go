package main

import (
	"bufio"
	"fmt"
	"os"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/suryansh98/glassdb/db"
	"github.com/suryansh98/glassdb/events"
)

const helpText = `statements: CREATE TABLE, CREATE INDEX, INSERT, SELECT, UPDATE, DELETE,
            BEGIN, COMMIT, ROLLBACK, CHECKPOINT, EXPLAIN SELECT ...
            terminate with ;

dot commands:
  .help          this text
  .tables        list tables
  .schema [t]    show schema (all tables or one)
  .stats         event counters since start (page reads, splits, WAL...)
  .crash         kill the process WITHOUT flushing — reopen to watch WAL recovery
  .quit          exit
`

// stats counts events by type for the .stats command.
type stats struct {
	mu     sync.Mutex
	counts map[string]uint64
}

func watchStats(bus *events.Bus) *stats {
	s := &stats{counts: make(map[string]uint64)}
	ch, _ := bus.Subscribe(1 << 14)
	go func() {
		for ev := range ch {
			s.mu.Lock()
			s.counts[ev.Type]++
			s.mu.Unlock()
		}
	}()
	return s
}

func (s *stats) render() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	keys := make([]string, 0, len(s.counts))
	for k := range s.counts {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	var sb strings.Builder
	for _, k := range keys {
		fmt.Fprintf(&sb, "%-20s %d\n", k, s.counts[k])
	}
	if sb.Len() == 0 {
		return "no events yet\n"
	}
	return sb.String()
}

// complete reports whether buffered input is a finished statement: it ends
// with ';' outside of any string literal.
func complete(buf string) bool {
	quotes := strings.Count(buf, "'")
	trimmed := strings.TrimSpace(buf)
	return quotes%2 == 0 && strings.HasSuffix(trimmed, ";")
}

func runREPL(d *db.DB, bus *events.Bus) {
	st := watchStats(bus)
	fmt.Println("glassdb — a small database you can see inside. Type .help for help.")

	scanner := bufio.NewScanner(os.Stdin)
	scanner.Buffer(make([]byte, 0, 1<<20), 1<<20)

	var buf strings.Builder
	prompt := func() {
		if buf.Len() == 0 {
			fmt.Print("glassdb> ")
		} else {
			fmt.Print("    ...> ")
		}
	}

	prompt()
	for scanner.Scan() {
		line := scanner.Text()
		if buf.Len() == 0 && strings.HasPrefix(strings.TrimSpace(line), ".") {
			if quit := dotCommand(d, st, strings.TrimSpace(line)); quit {
				return
			}
			prompt()
			continue
		}
		buf.WriteString(line)
		buf.WriteString("\n")
		if complete(buf.String()) {
			execute(d, buf.String())
			buf.Reset()
		}
		prompt()
	}
}

func dotCommand(d *db.DB, st *stats, line string) (quit bool) {
	fields := strings.Fields(line)
	switch fields[0] {
	case ".quit", ".exit":
		return true
	case ".crash":
		fmt.Println("💥 crashing without checkpoint or close — reopen this file to watch recovery")
		os.Exit(2)
	case ".help":
		fmt.Print(helpText)
	case ".tables":
		for _, t := range d.Snapshot().Tables {
			fmt.Println(t.Name)
		}
	case ".schema":
		snap := d.Snapshot()
		for _, t := range snap.Tables {
			if len(fields) > 1 && t.Name != strings.ToLower(fields[1]) {
				continue
			}
			var cols []string
			for _, c := range t.Columns {
				col := fmt.Sprintf("%s %s", c.Name, c.Type)
				if c.PK {
					col += " PRIMARY KEY"
				}
				cols = append(cols, col)
			}
			fmt.Printf("CREATE TABLE %s (%s);\n", t.Name, strings.Join(cols, ", "))
			for _, idx := range t.Indexes {
				fmt.Printf("CREATE INDEX %s ON %s(%s);\n", idx.Name, t.Name, idx.Column)
			}
		}
	case ".stats":
		fmt.Print(st.render())
	default:
		fmt.Fprintf(os.Stderr, "unknown command %s (try .help)\n", fields[0])
	}
	return false
}

func execute(d *db.DB, src string) {
	start := time.Now()
	results, err := d.Exec(src)
	elapsed := time.Since(start)
	for _, res := range results {
		if len(res.Columns) > 0 {
			fmt.Print(renderTable(res.Columns, res.Rows))
			fmt.Printf("(%d rows)\n", len(res.Rows))
		} else if res.RowsAffected > 0 {
			fmt.Printf("OK, %d row(s) affected\n", res.RowsAffected)
		} else {
			fmt.Println("OK")
		}
	}
	if err != nil {
		fmt.Fprintf(os.Stderr, "error: %v\n", err)
		return
	}
	fmt.Printf("-- %.2fms\n", float64(elapsed.Microseconds())/1000)
}
