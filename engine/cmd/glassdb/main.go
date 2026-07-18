// Command glassdb is the database binary: an interactive REPL (like
// sqlite3), a serve mode exposing SQL + the event stream to the X-ray
// visualizer, and a trace recorder for offline replay.
//
//	glassdb <file.db> [--cache N] [--record trace.jsonl]
//	glassdb serve <file.db> [--addr 127.0.0.1:4980] [--cache N] [--record trace.jsonl]
package main

import (
	"encoding/json"
	"fmt"
	"os"
	"os/signal"
	"strconv"
	"strings"

	"github.com/suryansh98/glassdb/db"
	"github.com/suryansh98/glassdb/events"
	"github.com/suryansh98/glassdb/server"
)

const usage = `glassdb — a small database you can see inside

usage:
  glassdb <file.db> [--cache N] [--record trace.jsonl]         interactive REPL
  glassdb serve <file.db> [--addr HOST:PORT] [--cache N] [--record trace.jsonl]

flags:
  --cache N          buffer pool size in pages (default 256; try 32 to watch evictions)
  --record FILE      record the event stream to FILE as JSON lines
  --addr HOST:PORT   serve address (default 127.0.0.1:4980)
`

type options struct {
	serve  bool
	path   string
	addr   string
	cache  int
	record string
}

func parseArgs(args []string) (options, error) {
	o := options{addr: "127.0.0.1:4980"}
	if len(args) > 0 && args[0] == "serve" {
		o.serve = true
		args = args[1:]
	}
	i := 0
	value := func(name string) (string, error) {
		i++
		if i >= len(args) {
			return "", fmt.Errorf("%s needs a value", name)
		}
		return args[i], nil
	}
	for ; i < len(args); i++ {
		switch a := args[i]; {
		case a == "--cache":
			v, err := value(a)
			if err != nil {
				return o, err
			}
			n, err := strconv.Atoi(v)
			if err != nil || n < 1 {
				return o, fmt.Errorf("--cache wants a positive integer, got %q", v)
			}
			o.cache = n
		case a == "--record":
			v, err := value(a)
			if err != nil {
				return o, err
			}
			o.record = v
		case a == "--addr" && o.serve:
			v, err := value(a)
			if err != nil {
				return o, err
			}
			o.addr = v
		case strings.HasPrefix(a, "-"):
			return o, fmt.Errorf("unknown flag %s", a)
		case o.path == "":
			o.path = a
		default:
			return o, fmt.Errorf("unexpected argument %s", a)
		}
	}
	if o.path == "" {
		return o, fmt.Errorf("missing database file")
	}
	return o, nil
}

// startRecorder streams bus events into a JSON-lines trace file.
func startRecorder(bus *events.Bus, path string) (func(), error) {
	f, err := os.Create(path)
	if err != nil {
		return nil, err
	}
	ch, cancel := bus.Subscribe(1 << 16)
	done := make(chan struct{})
	go func() {
		enc := json.NewEncoder(f)
		for ev := range ch {
			enc.Encode(ev)
		}
		close(done)
	}()
	return func() {
		cancel()
		<-done
		f.Close()
	}, nil
}

func main() {
	args := os.Args[1:]
	if len(args) == 0 || args[0] == "-h" || args[0] == "--help" {
		fmt.Print(usage)
		return
	}
	opts, err := parseArgs(args)
	if err != nil {
		fmt.Fprintf(os.Stderr, "glassdb: %v\n\n%s", err, usage)
		os.Exit(1)
	}

	bus := events.NewBus()

	// The recorder starts before Open so that WAL recovery events (emitted
	// while opening a crashed database) land in the trace.
	var stopRecorder func()
	if opts.record != "" {
		var err error
		stopRecorder, err = startRecorder(bus, opts.record)
		if err != nil {
			fmt.Fprintf(os.Stderr, "glassdb: %v\n", err)
			os.Exit(1)
		}
		fmt.Fprintf(os.Stderr, "recording events to %s\n", opts.record)
	}

	d, err := db.Open(opts.path, db.Options{Bus: bus, CacheSize: opts.cache})
	if err != nil {
		fmt.Fprintf(os.Stderr, "glassdb: %v\n", err)
		os.Exit(1)
	}

	shutdown := func() {
		if stopRecorder != nil {
			stopRecorder()
		}
		if err := d.Close(); err != nil {
			fmt.Fprintf(os.Stderr, "glassdb: close: %v\n", err)
		}
	}

	// Ctrl+C closes cleanly (checkpointing the WAL). The /crash endpoint
	// intentionally bypasses this.
	sig := make(chan os.Signal, 1)
	signal.Notify(sig, os.Interrupt)
	go func() {
		<-sig
		shutdown()
		os.Exit(0)
	}()

	if opts.serve {
		fmt.Printf("glassdb serving %s\n", opts.path)
		fmt.Printf("  api:     http://%s  (POST /sql, GET /snapshot, GET /tree?name=)\n", opts.addr)
		fmt.Printf("  events:  http://%s/events  (SSE)\n", opts.addr)
		fmt.Printf("  control: POST /control {\"action\":\"pause|resume|step\",\"speedMs\":n}\n")
		fmt.Printf("  crash:   POST /crash  — kill -9 the engine, then restart to watch recovery\n")
		if err := server.New(d, bus).ListenAndServe(opts.addr); err != nil {
			fmt.Fprintf(os.Stderr, "glassdb: %v\n", err)
			shutdown()
			os.Exit(1)
		}
		return
	}

	runREPL(d, bus)
	shutdown()
}
