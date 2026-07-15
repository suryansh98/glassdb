package vfs

import (
	"errors"
	"path/filepath"
	"testing"
)

func TestOSRoundTrip(t *testing.T) {
	path := filepath.Join(t.TempDir(), "t.bin")
	var v OS

	f, err := v.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.WriteAt([]byte("hello"), 10); err != nil {
		t.Fatal(err)
	}
	if err := f.Sync(); err != nil {
		t.Fatal(err)
	}
	size, err := f.Size()
	if err != nil || size != 15 {
		t.Fatalf("size=%d err=%v, want 15", size, err)
	}
	buf := make([]byte, 5)
	if _, err := f.ReadAt(buf, 10); err != nil {
		t.Fatal(err)
	}
	if string(buf) != "hello" {
		t.Fatalf("got %q", buf)
	}
	if err := f.Close(); err != nil {
		t.Fatal(err)
	}

	ok, err := v.Exists(path)
	if err != nil || !ok {
		t.Fatalf("exists=%v err=%v", ok, err)
	}
	if err := v.Remove(path); err != nil {
		t.Fatal(err)
	}
	ok, _ = v.Exists(path)
	if ok {
		t.Fatal("file still exists after remove")
	}
	if err := v.Remove(path); err != nil {
		t.Fatalf("remove of missing file should be nil, got %v", err)
	}
}

func TestFaultStopsPersisting(t *testing.T) {
	path := filepath.Join(t.TempDir(), "t.bin")
	fault := NewFault(OS{})

	f, err := fault.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	fault.FailAfter(2)

	if _, err := f.WriteAt([]byte("aa"), 0); err != nil {
		t.Fatal(err)
	}
	if _, err := f.WriteAt([]byte("bb"), 2); err != nil {
		t.Fatal(err)
	}
	// Budget exhausted: this write must fail and not persist.
	if _, err := f.WriteAt([]byte("cc"), 4); !errors.Is(err, ErrInjected) {
		t.Fatalf("want ErrInjected, got %v", err)
	}
	if err := f.Sync(); !errors.Is(err, ErrInjected) {
		t.Fatalf("sync after fault: want ErrInjected, got %v", err)
	}
	if !fault.Failed() {
		t.Fatal("Failed() should be true")
	}
	f.Close()

	// Reopen through a clean VFS: only the first two writes reached disk.
	var v OS
	g, err := v.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer g.Close()
	size, _ := g.Size()
	if size != 4 {
		t.Fatalf("persisted size=%d, want 4", size)
	}
}
