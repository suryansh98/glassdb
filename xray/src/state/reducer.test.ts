import { describe, expect, it } from 'vitest';
import type { EngineEvent, Snapshot } from '../types';
import { applyEvent, applyEvents, applySnapshot, initialModel } from './reducer';

let seq = 0;
function ev(type: string, fields?: Record<string, unknown>): EngineEvent {
  return { seq: ++seq, type, fields };
}

describe('reducer', () => {
  it('counts every event type, including unknown ones', () => {
    const m = applyEvents(initialModel(), [ev('page.read'), ev('page.read'), ev('mystery.event')]);
    expect(m.counters['page.read']).toBe(2);
    expect(m.counters['mystery.event']).toBe(1);
  });

  it('tracks pages and MRU cache order from reads', () => {
    const m = applyEvents(initialModel(), [
      ev('page.read', { page: 1, kind: 'table-leaf', src: 'file' }),
      ev('page.read', { page: 2, kind: 'meta', src: 'wal' }),
      ev('page.read', { page: 1, kind: 'table-leaf', src: 'cache' }),
    ]);
    expect(m.pages[1].kind).toBe('table-leaf');
    expect(m.pages[1].src).toBe('cache');
    expect(m.pages[2].lastOp).toBe('read');
    expect(m.cache.entries).toEqual([1, 2]); // page 1 moved back to front
  });

  it('handles cache counters and evictions', () => {
    const m = applyEvents(initialModel(), [
      ev('page.read', { page: 3, kind: 'table-leaf', src: 'file' }),
      ev('cache.hit', { page: 3 }),
      ev('cache.miss', { page: 4 }),
      ev('cache.spill', { page: 3 }),
      ev('cache.evict', { page: 3 }),
    ]);
    expect(m.cache.hits).toBe(1);
    expect(m.cache.misses).toBe(1);
    expect(m.cache.spills).toBe(1);
    expect(m.cache.evictions).toBe(1);
    expect(m.cache.entries).toEqual([]);
  });

  it('builds the WAL tape and truncates on rollback', () => {
    let m = applyEvents(initialModel(), [
      ev('wal.append', { frame: 0, page: 1, commit: false }),
      ev('wal.append', { frame: 1, page: 0, commit: true }),
      ev('wal.commit', { frames: 2 }),
      ev('txn.begin', {}),
      ev('wal.append', { frame: 2, page: 1, commit: false }),
    ]);
    expect(m.wal.frames).toHaveLength(3);
    expect(m.wal.committedFrames).toBe(2);
    expect(m.inTxn).toBe(true);

    m = applyEvent(m, ev('txn.rollback', {}));
    expect(m.wal.frames).toHaveLength(2); // uncommitted frame gone
    expect(m.inTxn).toBe(false);

    m = applyEvent(m, ev('wal.checkpoint', { pages: 2 }));
    expect(m.wal.frames).toHaveLength(0);
    expect(m.wal.checkpoints).toBe(1);
  });

  it('replays recovery onto the tape', () => {
    const m = applyEvents(initialModel(), [
      ev('wal.recover.begin', { frames: 3 }),
      ev('wal.recover.frame', { frame: 0, page: 1, commit: false }),
      ev('wal.recover.frame', { frame: 1, page: 0, commit: true }),
      ev('wal.recover.frame', { frame: 2, page: 1, commit: false }),
      ev('wal.recover.end', { committed: 2, dropped: 1 }),
    ]);
    expect(m.recovery).toEqual({ active: false, framesReplayed: 2, dropped: 1 });
    expect(m.wal.frames).toHaveLength(2); // dropped tail removed
    expect(m.wal.frames.every((f) => f.recovered)).toBe(true);
    expect(m.wal.committedFrames).toBe(2);
  });

  it('keeps only recent split flashes', () => {
    const events: EngineEvent[] = [];
    for (let i = 0; i < 10; i++) {
      events.push(ev('btree.split', { page: i, newPage: 100 + i }));
    }
    const m = applyEvents(initialModel(), events);
    expect(m.splitFlash.length).toBeLessThanOrEqual(8);
    expect(m.splitFlash[m.splitFlash.length - 1].page).toBe(109);
  });

  it('merges snapshots without losing flash state', () => {
    const snap: Snapshot = {
      pages: [
        { id: 0, kind: 'meta' },
        { id: 1, kind: 'table-leaf' },
      ],
      tables: [],
      wal: { frames: 0, committed: 0 },
      cache: { cap: 32, entries: [1] },
      inTxn: false,
    };
    let m = applyEvent(initialModel(), ev('page.read', { page: 1, kind: 'table-leaf', src: 'file' }));
    const flashSeq = m.pages[1].flashSeq;
    m = applySnapshot(m, snap);
    expect(m.cache.cap).toBe(32);
    expect(m.pages[0].kind).toBe('meta');
    expect(m.pages[1].flashSeq).toBe(flashSeq);
  });
});
