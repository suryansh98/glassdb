package pager

import (
	"bytes"
	"os"
	"path/filepath"
	"testing"

	"github.com/suryansh98/glassdb/events"
	"github.com/suryansh98/glassdb/vfs"
)

func openAt(t *testing.T, path string, opts Options) *Pager {
	t.Helper()
	p, err := Open(vfs.OS{}, path, nil, opts)
	if err != nil {
		t.Fatal(err)
	}
	return p
}

func TestFreshInitAndReopen(t *testing.T) {
	path := filepath.Join(t.TempDir(), "t.db")
	p := openAt(t, path, Options{})
	if p.PageCount() != 1 || p.CatalogRoot() != 0 {
		t.Fatalf("fresh db: pageCount=%d catalogRoot=%d", p.PageCount(), p.CatalogRoot())
	}
	if err := p.Close(); err != nil {
		t.Fatal(err)
	}
	p = openAt(t, path, Options{})
	defer p.Close()
	if p.PageCount() != 1 {
		t.Fatalf("reopened pageCount=%d", p.PageCount())
	}
}

func TestCommitPersistsAcrossReopen(t *testing.T) {
	path := filepath.Join(t.TempDir(), "t.db")
	p := openAt(t, path, Options{})
	if err := p.Begin(); err != nil {
		t.Fatal(err)
	}
	pg, err := p.Allocate(KindTableLeaf)
	if err != nil {
		t.Fatal(err)
	}
	copy(pg.Data[100:], "persist me")
	pg.MarkDirty()
	pg.Release()
	if err := p.SetCatalogRoot(pg.ID); err != nil {
		t.Fatal(err)
	}
	if err := p.Commit(); err != nil {
		t.Fatal(err)
	}
	if err := p.Close(); err != nil {
		t.Fatal(err)
	}

	p = openAt(t, path, Options{})
	defer p.Close()
	if p.PageCount() != 2 || p.CatalogRoot() != 1 {
		t.Fatalf("pageCount=%d catalogRoot=%d", p.PageCount(), p.CatalogRoot())
	}
	pg, err = p.Get(1)
	if err != nil {
		t.Fatal(err)
	}
	defer pg.Release()
	if !bytes.Equal(pg.Data[100:110], []byte("persist me")) {
		t.Fatalf("data lost: %q", pg.Data[100:110])
	}
}

func TestRollbackDiscards(t *testing.T) {
	path := filepath.Join(t.TempDir(), "t.db")
	p := openAt(t, path, Options{})
	defer p.Close()

	p.Begin()
	pg, _ := p.Allocate(KindTableLeaf)
	pg.Release()
	if err := p.Rollback(); err != nil {
		t.Fatal(err)
	}
	if p.PageCount() != 1 {
		t.Fatalf("pageCount after rollback = %d, want 1", p.PageCount())
	}
	// The page number must be reusable.
	p.Begin()
	pg2, _ := p.Allocate(KindIndexLeaf)
	if pg2.ID != 1 {
		t.Fatalf("allocated %d after rollback, want 1", pg2.ID)
	}
	pg2.Release()
	if err := p.Commit(); err != nil {
		t.Fatal(err)
	}
}

func TestLRUEvictionRespectsCapacity(t *testing.T) {
	path := filepath.Join(t.TempDir(), "t.db")
	bus := events.NewBus()
	ch, cancel := bus.Subscribe(4096)
	defer cancel()
	p, err := Open(vfs.OS{}, path, bus, Options{CacheSize: 3})
	if err != nil {
		t.Fatal(err)
	}
	defer p.Close()

	p.Begin()
	for i := 0; i < 6; i++ {
		pg, err := p.Allocate(KindTableLeaf)
		if err != nil {
			t.Fatal(err)
		}
		pg.Release()
	}
	if err := p.Commit(); err != nil {
		t.Fatal(err)
	}
	for pgno := uint32(1); pgno <= 6; pgno++ {
		pg, err := p.Get(pgno)
		if err != nil {
			t.Fatal(err)
		}
		pg.Release()
		if p.CacheLen() > 3 {
			t.Fatalf("cache grew to %d, capacity 3", p.CacheLen())
		}
	}
	evicts := 0
	for {
		select {
		case ev := <-ch:
			if ev.Type == events.EvCacheEvict {
				evicts++
			}
			continue
		default:
		}
		break
	}
	if evicts == 0 {
		t.Fatal("expected eviction events")
	}
}

func TestDirtySpillCommitsCorrectly(t *testing.T) {
	path := filepath.Join(t.TempDir(), "t.db")
	p := openAt(t, path, Options{CacheSize: 2})
	p.Begin()
	for i := 0; i < 4; i++ {
		pg, err := p.Allocate(KindTableLeaf)
		if err != nil {
			t.Fatal(err)
		}
		pg.Data[200] = byte(0xA0 + i)
		pg.MarkDirty()
		pg.Release()
	}
	if err := p.Commit(); err != nil {
		t.Fatal(err)
	}
	if err := p.Close(); err != nil {
		t.Fatal(err)
	}

	p = openAt(t, path, Options{})
	defer p.Close()
	for i := 0; i < 4; i++ {
		pg, err := p.Get(uint32(1 + i))
		if err != nil {
			t.Fatal(err)
		}
		if pg.Data[200] != byte(0xA0+i) {
			t.Fatalf("page %d: got %#x want %#x", pg.ID, pg.Data[200], 0xA0+i)
		}
		pg.Release()
	}
}

func TestCrashMidCommitPreservesOldState(t *testing.T) {
	path := filepath.Join(t.TempDir(), "t.db")
	fault := vfs.NewFault(vfs.OS{})
	p, err := Open(fault, path, nil, Options{})
	if err != nil {
		t.Fatal(err)
	}

	// Transaction A commits cleanly.
	p.Begin()
	pg, _ := p.Allocate(KindTableLeaf)
	copy(pg.Data[50:], "state A")
	pg.MarkDirty()
	pg.Release()
	if err := p.Commit(); err != nil {
		t.Fatal(err)
	}

	// Transaction B "crashes" partway through its commit: allow a couple of
	// WAL appends, then fail everything (including the commit fsync).
	p.Begin()
	pg, _ = p.Get(1)
	copy(pg.Data[50:], "state B")
	pg.MarkDirty()
	pg.Release()
	fault.FailAfter(1)
	if err := p.Commit(); err == nil {
		t.Fatal("commit should have failed")
	}
	p.CrashClose() // the crash: no checkpoint, no flush

	reopened := openAt(t, path, Options{})
	defer reopened.Close()
	pg, err = reopened.Get(1)
	if err != nil {
		t.Fatal(err)
	}
	defer pg.Release()
	if !bytes.Equal(pg.Data[50:57], []byte("state A")) {
		t.Fatalf("recovered %q, want state A", pg.Data[50:57])
	}
}

func TestTornFrameRecoversToLastCommit(t *testing.T) {
	path := filepath.Join(t.TempDir(), "t.db")
	p := openAt(t, path, Options{})

	p.Begin()
	pg, _ := p.Allocate(KindTableLeaf)
	copy(pg.Data[50:], "AAAA")
	pg.MarkDirty()
	pg.Release()
	if err := p.Commit(); err != nil {
		t.Fatal(err)
	}
	p.Begin()
	pg, _ = p.Get(1)
	copy(pg.Data[50:], "BBBB")
	pg.MarkDirty()
	pg.Release()
	if err := p.Commit(); err != nil {
		t.Fatal(err)
	}
	p.CrashClose()
	// Tear commit B's first frame on disk. Layout: 16-byte header,
	// 4112-byte frames; commit A = frames 0-1, commit B = frames 2-3.
	// Corrupt frame 2's page data.
	f, err := os.OpenFile(path+"-wal", os.O_RDWR, 0)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.WriteAt([]byte{0xFF}, 16+2*4112+16+50); err != nil {
		t.Fatal(err)
	}
	f.Close()

	reopened := openAt(t, path, Options{})
	defer reopened.Close()
	pg, err = reopened.Get(1)
	if err != nil {
		t.Fatal(err)
	}
	defer pg.Release()
	if !bytes.Equal(pg.Data[50:54], []byte("AAAA")) {
		t.Fatalf("recovered %q, want AAAA", pg.Data[50:54])
	}
}

func TestCheckpointMovesDataToMainFile(t *testing.T) {
	path := filepath.Join(t.TempDir(), "t.db")
	p := openAt(t, path, Options{})
	p.Begin()
	pg, _ := p.Allocate(KindTableLeaf)
	copy(pg.Data[10:], "checkpointed")
	pg.MarkDirty()
	pg.Release()
	if err := p.Commit(); err != nil {
		t.Fatal(err)
	}
	if err := p.Checkpoint(); err != nil {
		t.Fatal(err)
	}
	total, committed := p.WALFrames()
	if total != 0 || committed != 0 {
		t.Fatalf("WAL not reset after checkpoint: %d/%d", total, committed)
	}
	if err := p.Close(); err != nil {
		t.Fatal(err)
	}
	st, err := os.Stat(path + "-wal")
	if err != nil {
		t.Fatal(err)
	}
	if st.Size() != 16 {
		t.Fatalf("wal size %d, want header only (16)", st.Size())
	}

	reopened := openAt(t, path, Options{})
	defer reopened.Close()
	pg, err = reopened.Get(1)
	if err != nil {
		t.Fatal(err)
	}
	defer pg.Release()
	if !bytes.Equal(pg.Data[10:22], []byte("checkpointed")) {
		t.Fatalf("data lost after checkpoint: %q", pg.Data[10:22])
	}
}

func TestGuards(t *testing.T) {
	path := filepath.Join(t.TempDir(), "t.db")
	p := openAt(t, path, Options{})
	defer p.Close()
	if _, err := p.Get(0); err == nil {
		t.Fatal("Get(0) must error: meta page is pager-managed")
	}
	if _, err := p.Get(42); err == nil {
		t.Fatal("Get out of range must error")
	}
	if _, err := p.Allocate(KindTableLeaf); err == nil {
		t.Fatal("Allocate outside txn must error")
	}
	if err := p.Commit(); err == nil {
		t.Fatal("Commit outside txn must error")
	}
}
