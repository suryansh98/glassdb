// Package wal implements a SQLite-style page-image write-ahead log.
//
// Committing a transaction appends every dirty page as a frame, marks the
// last frame with the database size (the commit marker), and fsyncs once.
// The main database file is only touched at checkpoint time, when committed
// frames are copied back. Recovery is therefore trivial: scan the log,
// keep everything up to the last valid commit marker, ignore the rest.
package wal

import (
	"encoding/binary"
	"fmt"
	"hash/crc32"
	"math/rand"

	"github.com/suryansh98/glassdb/events"
	"github.com/suryansh98/glassdb/vfs"
)

const (
	headerSize = 16 // magic(4) version(2) pageSize(2) salt(4) reserved(4)
	frameHdr   = 16 // pgno(4) commitSize(4) salt(4) crc(4)
)

var magic = [4]byte{'G', 'L', 'W', 'L'}

// WAL is a write-ahead log for one database file. Not safe for concurrent
// use; the pager serializes access.
type WAL struct {
	fs       vfs.VFS
	file     vfs.File
	path     string
	bus      *events.Bus
	pageSize int
	salt     uint32

	nFrames    int            // valid frames in the file
	nCommitted int            // frames covered by the last commit marker
	committed  map[uint32]int // pgno -> latest committed frame index
	pending    map[uint32]int // frames appended since the last commit marker
}

// Open opens (or creates) the WAL at path and runs recovery.
func Open(fs vfs.VFS, path string, pageSize int, bus *events.Bus) (*WAL, error) {
	file, err := fs.Open(path)
	if err != nil {
		return nil, err
	}
	w := &WAL{
		fs:        fs,
		file:      file,
		path:      path,
		bus:       bus,
		pageSize:  pageSize,
		committed: make(map[uint32]int),
		pending:   make(map[uint32]int),
	}
	size, err := file.Size()
	if err != nil {
		return nil, err
	}
	if size < headerSize {
		if err := w.reset(); err != nil {
			return nil, err
		}
		return w, nil
	}
	if err := w.recover(size); err != nil {
		return nil, err
	}
	return w, nil
}

// reset writes a fresh header with a new salt and truncates all frames.
func (w *WAL) reset() error {
	w.salt = rand.Uint32()
	hdr := make([]byte, headerSize)
	copy(hdr[0:4], magic[:])
	binary.BigEndian.PutUint16(hdr[4:6], 1)
	binary.BigEndian.PutUint16(hdr[6:8], uint16(w.pageSize))
	binary.BigEndian.PutUint32(hdr[8:12], w.salt)
	if err := w.file.Truncate(0); err != nil {
		return err
	}
	if _, err := w.file.WriteAt(hdr, 0); err != nil {
		return err
	}
	if err := w.file.Sync(); err != nil {
		return err
	}
	w.nFrames, w.nCommitted = 0, 0
	w.committed = make(map[uint32]int)
	w.pending = make(map[uint32]int)
	return nil
}

// recover scans the log, accepting frames up to the last valid commit
// marker and truncating everything after it.
func (w *WAL) recover(size int64) error {
	hdr := make([]byte, headerSize)
	if _, err := w.file.ReadAt(hdr, 0); err != nil {
		return err
	}
	if [4]byte(hdr[0:4]) != magic {
		return fmt.Errorf("wal: bad magic in %s", w.path)
	}
	if int(binary.BigEndian.Uint16(hdr[6:8])) != w.pageSize {
		return fmt.Errorf("wal: page size mismatch in %s", w.path)
	}
	w.salt = binary.BigEndian.Uint32(hdr[8:12])

	frameSize := int64(frameHdr + w.pageSize)
	total := int((size - headerSize) / frameSize)
	w.bus.Emit(events.EvWalRecoverBegin, events.F{"frames": total})

	buf := make([]byte, frameSize)
	scanned := 0
	for i := 0; i < total; i++ {
		if _, err := w.file.ReadAt(buf, headerSize+int64(i)*frameSize); err != nil {
			break
		}
		pgno := binary.BigEndian.Uint32(buf[0:4])
		commitSize := binary.BigEndian.Uint32(buf[4:8])
		salt := binary.BigEndian.Uint32(buf[8:12])
		crc := binary.BigEndian.Uint32(buf[12:16])
		if salt != w.salt || crc != frameCRC(buf[0:12], buf[frameHdr:]) {
			break // torn or stale frame: stop scanning
		}
		scanned++
		w.pending[pgno] = i
		w.bus.Emit(events.EvWalRecoverFrame, events.F{
			"frame": i, "page": pgno, "commit": commitSize > 0,
		})
		if commitSize > 0 {
			for k, v := range w.pending {
				w.committed[k] = v
			}
			w.pending = make(map[uint32]int)
			w.nCommitted = i + 1
		}
	}
	// Drop the uncommitted (or torn) tail.
	w.pending = make(map[uint32]int)
	w.nFrames = w.nCommitted
	if err := w.file.Truncate(headerSize + int64(w.nCommitted)*frameSize); err != nil {
		return err
	}
	w.bus.Emit(events.EvWalRecoverEnd, events.F{
		"committed": w.nCommitted, "dropped": total - w.nCommitted,
	})
	return nil
}

func frameCRC(hdr12, data []byte) uint32 {
	c := crc32.ChecksumIEEE(hdr12)
	return crc32.Update(c, crc32.IEEETable, data)
}

// Append writes one page image as a frame. commitSize > 0 marks it as a
// commit frame recording the database size in pages. Not synced until
// Commit.
func (w *WAL) Append(pgno uint32, data []byte, commitSize uint32) error {
	if len(data) != w.pageSize {
		return fmt.Errorf("wal: page data must be %d bytes", w.pageSize)
	}
	buf := make([]byte, frameHdr+w.pageSize)
	binary.BigEndian.PutUint32(buf[0:4], pgno)
	binary.BigEndian.PutUint32(buf[4:8], commitSize)
	binary.BigEndian.PutUint32(buf[8:12], w.salt)
	copy(buf[frameHdr:], data)
	binary.BigEndian.PutUint32(buf[12:16], frameCRC(buf[0:12], data))

	frameSize := int64(frameHdr + w.pageSize)
	if _, err := w.file.WriteAt(buf, headerSize+int64(w.nFrames)*frameSize); err != nil {
		return err
	}
	w.pending[pgno] = w.nFrames
	w.nFrames++
	w.bus.Emit(events.EvWalAppend, events.F{
		"frame": w.nFrames - 1, "page": pgno, "commit": commitSize > 0,
	})
	return nil
}

// Commit fsyncs the log and promotes all pending frames to committed.
func (w *WAL) Commit() error {
	if err := w.file.Sync(); err != nil {
		return err
	}
	for k, v := range w.pending {
		w.committed[k] = v
	}
	w.pending = make(map[uint32]int)
	w.nCommitted = w.nFrames
	w.bus.Emit(events.EvWalCommit, events.F{"frames": w.nFrames})
	return nil
}

// Rollback discards all frames appended since the last commit.
func (w *WAL) Rollback() error {
	w.pending = make(map[uint32]int)
	w.nFrames = w.nCommitted
	frameSize := int64(frameHdr + w.pageSize)
	return w.file.Truncate(headerSize + int64(w.nCommitted)*frameSize)
}

// ReadPage returns the newest image of pgno visible to the current
// transaction: its own pending frames first, then committed ones.
func (w *WAL) ReadPage(pgno uint32) ([]byte, bool, error) {
	idx, ok := w.pending[pgno]
	if !ok {
		idx, ok = w.committed[pgno]
	}
	if !ok {
		return nil, false, nil
	}
	frameSize := int64(frameHdr + w.pageSize)
	data := make([]byte, w.pageSize)
	if _, err := w.file.ReadAt(data, headerSize+int64(idx)*frameSize+frameHdr); err != nil {
		return nil, false, err
	}
	return data, true, nil
}

// Checkpoint copies every committed page image into dst (the main database
// file), fsyncs it, and resets the log. Must not run mid-transaction.
func (w *WAL) Checkpoint(dst vfs.File) error {
	if len(w.pending) > 0 {
		return fmt.Errorf("wal: checkpoint during transaction")
	}
	pages := len(w.committed)
	for pgno := range w.committed {
		data, ok, err := w.ReadPage(pgno)
		if err != nil {
			return err
		}
		if !ok {
			continue
		}
		if _, err := dst.WriteAt(data, int64(pgno)*int64(w.pageSize)); err != nil {
			return err
		}
	}
	if err := dst.Sync(); err != nil {
		return err
	}
	if err := w.reset(); err != nil {
		return err
	}
	w.bus.Emit(events.EvWalCheckpoint, events.F{"pages": pages})
	return nil
}

// Frames returns the number of valid frames in the log.
func (w *WAL) Frames() int { return w.nFrames }

// CommittedFrames returns the number of frames covered by commit markers.
func (w *WAL) CommittedFrames() int { return w.nCommitted }

// Close closes the underlying file without checkpointing.
func (w *WAL) Close() error { return w.file.Close() }
