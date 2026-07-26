// The pure heart of the X-ray: fold engine events into a render model.
// Pure so that replay-scrubbing can rebuild state from any point, and so
// it's trivially unit-testable.

import type { EngineEvent, Snapshot } from '../types';

export interface PageCell {
  id: number;
  kind: string;
  lastOp?: 'read' | 'write';
  src?: string; // cache | wal | file — where the read was served from
  flashSeq?: number; // retriggers the CSS flash animation
}

export interface WalFrame {
  frame: number;
  page: number;
  commit: boolean;
  recovered?: boolean;
}

export interface XrayModel {
  pages: Record<number, PageCell>;
  wal: { frames: WalFrame[]; committedFrames: number; checkpoints: number };
  cache: {
    entries: number[]; // most recently used first
    cap: number;
    hits: number;
    misses: number;
    evictions: number;
    spills: number;
  };
  counters: Record<string, number>;
  splitFlash: { page: number; seq: number }[];
  recovery: { active: boolean; framesReplayed: number; dropped: number } | null;
  inTxn: boolean;
}

export function initialModel(cap = 256): XrayModel {
  return {
    pages: {},
    wal: { frames: [], committedFrames: 0, checkpoints: 0 },
    cache: { entries: [], cap, hits: 0, misses: 0, evictions: 0, spills: 0 },
    counters: {},
    splitFlash: [],
    recovery: null,
    inTxn: false,
  };
}

// applySnapshot seeds/corrects the model from GET /snapshot (live mode).
export function applySnapshot(m: XrayModel, snap: Snapshot): XrayModel {
  const pages: Record<number, PageCell> = {};
  for (const p of snap.pages) {
    pages[p.id] = { ...m.pages[p.id], id: p.id, kind: p.kind };
  }
  return {
    ...m,
    pages,
    cache: {
      ...m.cache,
      cap: snap.cache.cap,
      entries: snap.cache.entries ?? [],
    },
    inTxn: snap.inTxn,
  };
}

function num(ev: EngineEvent, field: string): number {
  const v = ev.fields?.[field];
  return typeof v === 'number' ? v : 0;
}

function str(ev: EngineEvent, field: string): string {
  const v = ev.fields?.[field];
  return typeof v === 'string' ? v : '';
}

const SPLIT_FLASH_KEEP = 8;

// applyEvent folds one event into the model, returning a new model object.
// Unknown event types only bump the counter — forward compatible.
export function applyEvent(m: XrayModel, ev: EngineEvent): XrayModel {
  const next: XrayModel = {
    ...m,
    counters: { ...m.counters, [ev.type]: (m.counters[ev.type] ?? 0) + 1 },
  };

  const touchPage = (op: 'read' | 'write') => {
    const id = num(ev, 'page');
    const kind = str(ev, 'kind') || m.pages[id]?.kind || 'free';
    next.pages = {
      ...m.pages,
      [id]: { id, kind, lastOp: op, src: str(ev, 'src') || undefined, flashSeq: ev.seq },
    };
  };

  switch (ev.type) {
    case 'page.read': {
      touchPage('read');
      const id = num(ev, 'page');
      const src = str(ev, 'src');
      const entries = m.cache.entries.filter((e) => e !== id);
      entries.unshift(id);
      next.cache = { ...m.cache, entries };
      if (src === 'cache') next.cache.hits = m.cache.hits; // counted by cache.hit
      break;
    }
    case 'page.write':
      touchPage('write');
      break;

    case 'cache.hit':
      next.cache = { ...m.cache, hits: m.cache.hits + 1 };
      break;
    case 'cache.miss':
      next.cache = { ...m.cache, misses: m.cache.misses + 1 };
      break;
    case 'cache.evict': {
      const id = num(ev, 'page');
      next.cache = {
        ...m.cache,
        evictions: m.cache.evictions + 1,
        entries: m.cache.entries.filter((e) => e !== id),
      };
      break;
    }
    case 'cache.spill':
      next.cache = { ...m.cache, spills: m.cache.spills + 1 };
      break;

    case 'wal.append': {
      const frame: WalFrame = {
        frame: num(ev, 'frame'),
        page: num(ev, 'page'),
        commit: ev.fields?.commit === true,
      };
      next.wal = { ...m.wal, frames: [...m.wal.frames, frame] };
      break;
    }
    case 'wal.commit':
      next.wal = { ...m.wal, committedFrames: m.wal.frames.length };
      break;
    case 'wal.checkpoint':
      next.wal = { frames: [], committedFrames: 0, checkpoints: m.wal.checkpoints + 1 };
      break;

    case 'wal.recover.begin':
      next.recovery = { active: true, framesReplayed: 0, dropped: 0 };
      next.wal = { ...m.wal, frames: [], committedFrames: 0 };
      break;
    case 'wal.recover.frame': {
      const frame: WalFrame = {
        frame: num(ev, 'frame'),
        page: num(ev, 'page'),
        commit: ev.fields?.commit === true,
        recovered: true,
      };
      next.wal = { ...m.wal, frames: [...m.wal.frames, frame] };
      next.recovery = m.recovery
        ? { ...m.recovery, framesReplayed: m.recovery.framesReplayed + 1 }
        : { active: true, framesReplayed: 1, dropped: 0 };
      break;
    }
    case 'wal.recover.end': {
      const committed = num(ev, 'committed');
      next.recovery = { active: false, framesReplayed: committed, dropped: num(ev, 'dropped') };
      next.wal = {
        ...m.wal,
        frames: m.wal.frames.slice(0, committed),
        committedFrames: committed,
      };
      break;
    }

    case 'btree.split': {
      const flashes = [
        ...m.splitFlash,
        { page: num(ev, 'page'), seq: ev.seq },
        { page: num(ev, 'newPage'), seq: ev.seq },
      ];
      next.splitFlash = flashes.slice(-SPLIT_FLASH_KEEP);
      break;
    }

    case 'txn.begin':
      next.inTxn = true;
      break;
    case 'txn.commit':
      next.inTxn = false;
      break;
    case 'txn.rollback':
      next.inTxn = false;
      // Uncommitted frames vanish from the log on rollback.
      next.wal = { ...m.wal, frames: m.wal.frames.slice(0, m.wal.committedFrames) };
      break;

    default:
      break; // counter already bumped
  }
  return next;
}

export function applyEvents(m: XrayModel, evs: EngineEvent[]): XrayModel {
  let cur = m;
  for (const ev of evs) cur = applyEvent(cur, ev);
  return cur;
}
