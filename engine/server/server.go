// Package server is glassdb's serve mode: a localhost HTTP API that the
// X-ray visualizer attaches to. Events stream out over Server-Sent Events
// (no third-party WebSocket dependency), SQL comes in over POST, and a
// control endpoint drives the event gate so the engine can be paused,
// single-stepped, or slowed to human speed.
package server

import (
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"sync"
	"time"

	"github.com/suryansh98/glassdb/db"
	"github.com/suryansh98/glassdb/events"
	"github.com/suryansh98/glassdb/record"
)

// gate implements pause / single-step / slow-motion for the event bus. It
// runs on the engine goroutine: while paused, the engine is genuinely
// frozen mid-statement — that's the point.
type gate struct {
	mu         sync.Mutex
	cond       *sync.Cond
	paused     bool
	stepBudget int
	delay      time.Duration
}

func newGate() *gate {
	g := &gate{}
	g.cond = sync.NewCond(&g.mu)
	return g
}

func (g *gate) wait() {
	g.mu.Lock()
	for g.paused && g.stepBudget == 0 {
		g.cond.Wait()
	}
	if g.stepBudget > 0 {
		g.stepBudget--
	}
	delay := g.delay
	g.mu.Unlock()
	if delay > 0 {
		time.Sleep(delay)
	}
}

func (g *gate) control(action string, speedMs *int) error {
	g.mu.Lock()
	defer g.mu.Unlock()
	switch action {
	case "pause":
		g.paused = true
	case "resume":
		g.paused = false
	case "step":
		g.stepBudget++
	case "":
	default:
		return fmt.Errorf("unknown action %q", action)
	}
	if speedMs != nil {
		g.delay = time.Duration(*speedMs) * time.Millisecond
	}
	g.cond.Broadcast()
	return nil
}

// Server exposes one DB over HTTP.
type Server struct {
	db   *db.DB
	bus  *events.Bus
	gate *gate
	mux  *http.ServeMux

	// exitFn is os.Exit in production; tests override it.
	exitFn func(code int)
}

// New wires the endpoints and installs the control gate on the bus.
func New(d *db.DB, bus *events.Bus) *Server {
	s := &Server{db: d, bus: bus, gate: newGate(), mux: http.NewServeMux(), exitFn: os.Exit}
	bus.SetGate(s.gate.wait)
	s.mux.HandleFunc("GET /events", s.handleEvents)
	s.mux.HandleFunc("POST /sql", s.handleSQL)
	s.mux.HandleFunc("GET /snapshot", s.handleSnapshot)
	s.mux.HandleFunc("GET /tree", s.handleTree)
	s.mux.HandleFunc("POST /control", s.handleControl)
	s.mux.HandleFunc("POST /crash", s.handleCrash)
	return s
}

// Handler returns the HTTP handler (CORS-wrapped), for embedding and tests.
func (s *Server) Handler() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		h := w.Header()
		h.Set("Access-Control-Allow-Origin", "*")
		h.Set("Access-Control-Allow-Methods", "GET, POST, OPTIONS")
		h.Set("Access-Control-Allow-Headers", "Content-Type")
		if r.Method == http.MethodOptions {
			w.WriteHeader(http.StatusNoContent)
			return
		}
		s.mux.ServeHTTP(w, r)
	})
}

// ListenAndServe blocks serving on addr.
func (s *Server) ListenAndServe(addr string) error {
	srv := &http.Server{Addr: addr, Handler: s.Handler()}
	return srv.ListenAndServe()
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	json.NewEncoder(w).Encode(v)
}

func writeErr(w http.ResponseWriter, status int, err error) {
	writeJSON(w, status, map[string]string{"error": err.Error()})
}

func (s *Server) handleEvents(w http.ResponseWriter, r *http.Request) {
	flusher, ok := w.(http.Flusher)
	if !ok {
		http.Error(w, "streaming unsupported", http.StatusInternalServerError)
		return
	}
	h := w.Header()
	h.Set("Content-Type", "text/event-stream")
	h.Set("Cache-Control", "no-cache")
	h.Set("Connection", "keep-alive")
	w.WriteHeader(http.StatusOK)
	fmt.Fprint(w, ": connected\n\n")
	flusher.Flush()

	ch, cancel := s.bus.Subscribe(8192)
	defer cancel()
	heartbeat := time.NewTicker(15 * time.Second)
	defer heartbeat.Stop()

	for {
		select {
		case <-r.Context().Done():
			return
		case <-heartbeat.C:
			fmt.Fprint(w, ": ping\n\n")
			flusher.Flush()
		case ev, ok := <-ch:
			if !ok {
				return
			}
			data, err := json.Marshal(ev)
			if err != nil {
				continue
			}
			fmt.Fprintf(w, "data: %s\n\n", data)
			flusher.Flush()
		}
	}
}

// sqlRequest / sqlResponse are the POST /sql wire types.
type sqlRequest struct {
	SQL string `json:"sql"`
}

type sqlResult struct {
	Columns      []string `json:"columns,omitempty"`
	Rows         [][]any  `json:"rows,omitempty"`
	RowsAffected int64    `json:"rowsAffected"`
}

func toJSONValue(v record.Value) any {
	switch v.Type {
	case record.TInt:
		return v.Int
	case record.TReal:
		return v.Real
	case record.TText:
		return v.Text
	}
	return nil
}

func (s *Server) handleSQL(w http.ResponseWriter, r *http.Request) {
	var req sqlRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeErr(w, http.StatusBadRequest, fmt.Errorf("bad request body: %w", err))
		return
	}
	results, err := s.db.Exec(req.SQL)
	if err != nil {
		writeErr(w, http.StatusBadRequest, err)
		return
	}
	out := make([]sqlResult, len(results))
	for i, res := range results {
		out[i] = sqlResult{Columns: res.Columns, RowsAffected: res.RowsAffected}
		for _, row := range res.Rows {
			jsonRow := make([]any, len(row))
			for j, v := range row {
				jsonRow[j] = toJSONValue(v)
			}
			out[i].Rows = append(out[i].Rows, jsonRow)
		}
	}
	writeJSON(w, http.StatusOK, map[string]any{"results": out})
}

func (s *Server) handleSnapshot(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, s.db.Snapshot())
}

func (s *Server) handleTree(w http.ResponseWriter, r *http.Request) {
	name := r.URL.Query().Get("name")
	if name == "" {
		writeErr(w, http.StatusBadRequest, fmt.Errorf("missing ?name="))
		return
	}
	dump, err := s.db.Tree(name)
	if err != nil {
		writeErr(w, http.StatusNotFound, err)
		return
	}
	writeJSON(w, http.StatusOK, dump)
}

type controlRequest struct {
	Action  string `json:"action"`
	SpeedMs *int   `json:"speedMs"`
}

func (s *Server) handleControl(w http.ResponseWriter, r *http.Request) {
	var req controlRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeErr(w, http.StatusBadRequest, err)
		return
	}
	if err := s.gate.control(req.Action, req.SpeedMs); err != nil {
		writeErr(w, http.StatusBadRequest, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]bool{"ok": true})
}

// handleCrash kills the process mid-flight — the crash-recovery demo.
// Restart `glassdb serve` afterwards and watch the WAL recovery events.
func (s *Server) handleCrash(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, map[string]string{"status": "crashing"})
	if f, ok := w.(http.Flusher); ok {
		f.Flush()
	}
	go func() {
		time.Sleep(50 * time.Millisecond) // let the response out
		s.exitFn(2)
	}()
}
