package server

import (
	"bufio"
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/suryansh98/glassdb/db"
	"github.com/suryansh98/glassdb/events"
)

func newTestServer(t *testing.T) (*Server, *httptest.Server) {
	t.Helper()
	bus := events.NewBus()
	d, err := db.Open(filepath.Join(t.TempDir(), "t.db"), db.Options{Bus: bus})
	if err != nil {
		t.Fatal(err)
	}
	s := New(d, bus)
	ts := httptest.NewServer(s.Handler())
	t.Cleanup(func() {
		ts.Close()
		bus.SetGate(nil)
		d.Close()
	})
	return s, ts
}

func postJSON(t *testing.T, url string, body any) *http.Response {
	t.Helper()
	b, _ := json.Marshal(body)
	resp, err := http.Post(url, "application/json", bytes.NewReader(b))
	if err != nil {
		t.Fatal(err)
	}
	return resp
}

func TestSQLRoundTrip(t *testing.T) {
	_, ts := newTestServer(t)
	resp := postJSON(t, ts.URL+"/sql", map[string]string{
		"sql": "CREATE TABLE t (id INTEGER PRIMARY KEY, name TEXT); INSERT INTO t (name) VALUES ('ada'); SELECT * FROM t",
	})
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		t.Fatalf("status %d", resp.StatusCode)
	}
	var out struct {
		Results []struct {
			Columns      []string `json:"columns"`
			Rows         [][]any  `json:"rows"`
			RowsAffected int64    `json:"rowsAffected"`
		} `json:"results"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		t.Fatal(err)
	}
	if len(out.Results) != 3 {
		t.Fatalf("results: %+v", out.Results)
	}
	sel := out.Results[2]
	if len(sel.Rows) != 1 || sel.Rows[0][0].(float64) != 1 || sel.Rows[0][1].(string) != "ada" {
		t.Fatalf("select rows: %+v", sel.Rows)
	}

	resp = postJSON(t, ts.URL+"/sql", map[string]string{"sql": "SELECT * FROM missing"})
	defer resp.Body.Close()
	if resp.StatusCode != 400 {
		t.Fatalf("error status %d", resp.StatusCode)
	}
}

func TestSnapshotAndTree(t *testing.T) {
	_, ts := newTestServer(t)
	postJSON(t, ts.URL+"/sql", map[string]string{
		"sql": "CREATE TABLE t (id INTEGER PRIMARY KEY, name TEXT); INSERT INTO t (name) VALUES ('a'), ('b')",
	}).Body.Close()

	resp, err := http.Get(ts.URL + "/snapshot")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	var snap struct {
		Pages  []any `json:"pages"`
		Tables []any `json:"tables"`
		Cache  struct {
			Cap int `json:"cap"`
		} `json:"cache"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&snap); err != nil {
		t.Fatal(err)
	}
	if len(snap.Pages) < 3 || len(snap.Tables) != 1 || snap.Cache.Cap != 256 {
		t.Fatalf("snapshot: %+v", snap)
	}

	resp, err = http.Get(ts.URL + "/tree?name=t")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	var dump struct {
		Kind   string `json:"kind"`
		NCells int    `json:"nCells"`
	}
	json.NewDecoder(resp.Body).Decode(&dump)
	if dump.Kind != "table-leaf" || dump.NCells != 2 {
		t.Fatalf("tree: %+v", dump)
	}

	resp, err = http.Get(ts.URL + "/tree?name=missing")
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != 404 {
		t.Fatalf("missing tree status %d", resp.StatusCode)
	}
}

func TestEventsSSE(t *testing.T) {
	_, ts := newTestServer(t)

	resp, err := http.Get(ts.URL + "/events")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if ct := resp.Header.Get("Content-Type"); ct != "text/event-stream" {
		t.Fatalf("content type %q", ct)
	}

	lines := make(chan string, 100)
	go func() {
		sc := bufio.NewScanner(resp.Body)
		for sc.Scan() {
			lines <- sc.Text()
		}
	}()

	postJSON(t, ts.URL+"/sql", map[string]string{"sql": "CREATE TABLE t (a INTEGER)"}).Body.Close()

	deadline := time.After(5 * time.Second)
	for {
		select {
		case line := <-lines:
			if strings.HasPrefix(line, "data: ") {
				var ev events.Event
				if err := json.Unmarshal([]byte(line[6:]), &ev); err != nil {
					t.Fatalf("bad event JSON: %v in %q", err, line)
				}
				if ev.Type == events.EvStmtStart {
					return // saw the statement flow through SSE
				}
			}
		case <-deadline:
			t.Fatal("no stmt.start event arrived over SSE")
		}
	}
}

func TestControlPauseBlocksEngine(t *testing.T) {
	_, ts := newTestServer(t)
	postJSON(t, ts.URL+"/control", map[string]any{"action": "pause"}).Body.Close()

	done := make(chan struct{})
	go func() {
		postJSON(t, ts.URL+"/sql", map[string]string{"sql": "CREATE TABLE t (a INTEGER)"}).Body.Close()
		close(done)
	}()

	select {
	case <-done:
		t.Fatal("statement completed while engine was paused")
	case <-time.After(150 * time.Millisecond):
	}

	postJSON(t, ts.URL+"/control", map[string]any{"action": "resume"}).Body.Close()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("statement did not finish after resume")
	}
}

func TestCrashEndpoint(t *testing.T) {
	s, ts := newTestServer(t)
	exited := make(chan int, 1)
	s.exitFn = func(code int) { exited <- code }

	postJSON(t, ts.URL+"/crash", map[string]string{}).Body.Close()
	select {
	case code := <-exited:
		if code != 2 {
			t.Fatalf("exit code %d", code)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("crash endpoint did not exit")
	}
}
