package vfs

import (
	"errors"
	"sync"
)

// ErrInjected is returned by a Fault VFS once its write budget is exhausted.
var ErrInjected = errors.New("vfs: injected fault")

// Fault wraps another VFS and simulates a crash: after FailAfter(n), the
// n+1-th write/sync/truncate — across all files opened through it — fails
// and nothing is persisted from then on. Reads keep working, so tests can
// "reopen" the database and verify what actually reached disk.
type Fault struct {
	Inner VFS

	mu     sync.Mutex
	budget int64 // remaining successful writes; -1 = unlimited
	failed bool
}

// NewFault returns a Fault with an unlimited budget.
func NewFault(inner VFS) *Fault {
	return &Fault{Inner: inner, budget: -1}
}

// FailAfter allows n more successful write operations, then injects faults.
func (f *Fault) FailAfter(n int) {
	f.mu.Lock()
	f.budget = int64(n)
	f.failed = false
	f.mu.Unlock()
}

// Failed reports whether a fault has been injected.
func (f *Fault) Failed() bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.failed
}

// spend consumes one write from the budget; returns false once exhausted.
func (f *Fault) spend() bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.failed {
		return false
	}
	if f.budget < 0 {
		return true
	}
	if f.budget == 0 {
		f.failed = true
		return false
	}
	f.budget--
	return true
}

func (f *Fault) Open(name string) (File, error) {
	inner, err := f.Inner.Open(name)
	if err != nil {
		return nil, err
	}
	return &faultFile{f: inner, fault: f}, nil
}

func (f *Fault) Remove(name string) error {
	if !f.spend() {
		return ErrInjected
	}
	return f.Inner.Remove(name)
}

func (f *Fault) Exists(name string) (bool, error) { return f.Inner.Exists(name) }

type faultFile struct {
	f     File
	fault *Fault
}

func (ff *faultFile) ReadAt(p []byte, off int64) (int, error) { return ff.f.ReadAt(p, off) }

func (ff *faultFile) WriteAt(p []byte, off int64) (int, error) {
	if !ff.fault.spend() {
		return 0, ErrInjected
	}
	return ff.f.WriteAt(p, off)
}

func (ff *faultFile) Truncate(size int64) error {
	if !ff.fault.spend() {
		return ErrInjected
	}
	return ff.f.Truncate(size)
}

func (ff *faultFile) Sync() error {
	if !ff.fault.spend() {
		return ErrInjected
	}
	return ff.f.Sync()
}

func (ff *faultFile) Close() error         { return ff.f.Close() }
func (ff *faultFile) Size() (int64, error) { return ff.f.Size() }
