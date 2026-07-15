// Package vfs abstracts the storage layer under the engine, in the spirit of
// SQLite's VFS. The engine only ever talks to these interfaces, which is what
// makes fault-injection crash tests (and a future WASM/OPFS backend)
// possible.
package vfs

import "io"

// File is a random-access file.
type File interface {
	io.ReaderAt
	io.WriterAt
	Truncate(size int64) error
	Sync() error
	Close() error
	Size() (int64, error)
}

// VFS opens files by name.
type VFS interface {
	Open(name string) (File, error)
	Remove(name string) error
	Exists(name string) (bool, error)
}
