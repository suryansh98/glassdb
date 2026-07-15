package vfs

import (
	"errors"
	"io/fs"
	"os"
)

// OS is the production VFS backed by the operating system's filesystem.
type OS struct{}

type osFile struct {
	f *os.File
}

// Open opens name read-write, creating it if missing.
func (OS) Open(name string) (File, error) {
	f, err := os.OpenFile(name, os.O_RDWR|os.O_CREATE, 0o644)
	if err != nil {
		return nil, err
	}
	return &osFile{f: f}, nil
}

func (OS) Remove(name string) error {
	err := os.Remove(name)
	if errors.Is(err, fs.ErrNotExist) {
		return nil
	}
	return err
}

func (OS) Exists(name string) (bool, error) {
	_, err := os.Stat(name)
	if err == nil {
		return true, nil
	}
	if errors.Is(err, fs.ErrNotExist) {
		return false, nil
	}
	return false, err
}

func (o *osFile) ReadAt(p []byte, off int64) (int, error)  { return o.f.ReadAt(p, off) }
func (o *osFile) WriteAt(p []byte, off int64) (int, error) { return o.f.WriteAt(p, off) }
func (o *osFile) Truncate(size int64) error                { return o.f.Truncate(size) }
func (o *osFile) Sync() error                              { return o.f.Sync() }
func (o *osFile) Close() error                             { return o.f.Close() }

func (o *osFile) Size() (int64, error) {
	st, err := o.f.Stat()
	if err != nil {
		return 0, err
	}
	return st.Size(), nil
}
