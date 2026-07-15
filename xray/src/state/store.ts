import { create } from 'zustand';
import {
  fetchSnapshot,
  fetchTree,
  openEvents,
  parseTrace,
  postControl,
  postCrash,
  postSql,
} from '../api';
import type { ConsoleEntry, EngineEvent, Snapshot, TreeDump } from '../types';
import { applyEvents, applySnapshot, initialModel, type XrayModel } from './reducer';

const LOG_KEEP = 400;
const REPLAY_TICK_MS = 60;

export type Mode = 'idle' | 'live' | 'replay';

interface XrayStore {
  mode: Mode;
  baseUrl: string;
  connected: boolean;
  paused: boolean;
  speedMs: number;

  model: XrayModel;
  snapshot: Snapshot | null;
  log: EngineEvent[];
  console: ConsoleEntry[];
  trees: Record<string, TreeDump>;
  selectedTree: string;

  trace: EngineEvent[];
  replayPos: number;
  replaying: boolean;
  replayRate: number; // events per tick

  connect: (url: string) => Promise<void>;
  disconnect: () => void;
  runSql: (sql: string) => Promise<void>;
  refresh: () => Promise<void>;
  selectTree: (name: string) => void;
  setPaused: (paused: boolean) => Promise<void>;
  step: () => Promise<void>;
  setSpeed: (ms: number) => Promise<void>;
  crash: () => Promise<void>;

  loadTrace: (text: string) => void;
  playReplay: () => void;
  pauseReplay: () => void;
  seekReplay: (pos: number) => void;
  setReplayRate: (rate: number) => void;
}

let closeStream: (() => void) | null = null;
let pendingEvents: EngineEvent[] = [];
let flushTimer: ReturnType<typeof setInterval> | null = null;
let replayTimer: ReturnType<typeof setInterval> | null = null;

export const useXray = create<XrayStore>((set, get) => {
  // Incoming SSE events are buffered and folded into the model on a short
  // interval — one statement can emit hundreds of events, and re-rendering
  // per event would melt the tab.
  const startFlusher = () => {
    if (flushTimer) return;
    flushTimer = setInterval(() => {
      if (pendingEvents.length === 0) return;
      const batch = pendingEvents;
      pendingEvents = [];
      set((s) => ({
        model: applyEvents(s.model, batch),
        log: [...s.log, ...batch].slice(-LOG_KEEP),
      }));
    }, 50);
  };

  const stopTimers = () => {
    if (flushTimer) clearInterval(flushTimer);
    flushTimer = null;
    if (replayTimer) clearInterval(replayTimer);
    replayTimer = null;
  };

  return {
    mode: 'idle',
    baseUrl: 'http://127.0.0.1:4980',
    connected: false,
    paused: false,
    speedMs: 0,

    model: initialModel(),
    snapshot: null,
    log: [],
    console: [],
    trees: {},
    selectedTree: '',

    trace: [],
    replayPos: 0,
    replaying: false,
    replayRate: 25,

    connect: async (url) => {
      get().disconnect();
      const snapshot = await fetchSnapshot(url);
      set({
        mode: 'live',
        baseUrl: url,
        snapshot,
        model: applySnapshot(initialModel(snapshot.cache.cap), snapshot),
        log: [],
        console: [],
        trees: {},
        selectedTree: snapshot.tables?.[0]?.name ?? '',
      });
      closeStream = openEvents(
        url,
        (ev) => pendingEvents.push(ev),
        (connected) => set({ connected }),
      );
      startFlusher();
      await get().refresh();
    },

    disconnect: () => {
      closeStream?.();
      closeStream = null;
      stopTimers();
      pendingEvents = [];
      set({ mode: 'idle', connected: false, replaying: false });
    },

    runSql: async (sql) => {
      const { baseUrl } = get();
      try {
        const results = await postSql(baseUrl, sql);
        set((s) => ({ console: [...s.console, { sql, results }] }));
      } catch (err) {
        set((s) => ({ console: [...s.console, { sql, error: String(err) }] }));
      }
      await get().refresh();
    },

    refresh: async () => {
      const { baseUrl, mode } = get();
      if (mode !== 'live') return;
      try {
        const snapshot = await fetchSnapshot(baseUrl);
        const trees: Record<string, TreeDump> = {};
        for (const t of snapshot.tables ?? []) {
          trees[t.name] = await fetchTree(baseUrl, t.name);
          for (const idx of t.indexes) {
            trees[idx.name] = await fetchTree(baseUrl, idx.name);
          }
        }
        set((s) => ({
          snapshot,
          trees,
          model: applySnapshot(s.model, snapshot),
          selectedTree:
            s.selectedTree && trees[s.selectedTree]
              ? s.selectedTree
              : (snapshot.tables?.[0]?.name ?? ''),
        }));
      } catch {
        set({ connected: false });
      }
    },

    selectTree: (name) => set({ selectedTree: name }),

    setPaused: async (paused) => {
      await postControl(get().baseUrl, paused ? 'pause' : 'resume');
      set({ paused });
    },

    step: async () => {
      await postControl(get().baseUrl, 'step');
    },

    setSpeed: async (ms) => {
      await postControl(get().baseUrl, '', ms);
      set({ speedMs: ms });
    },

    crash: async () => {
      await postCrash(get().baseUrl);
      set({ connected: false });
    },

    loadTrace: (text) => {
      get().disconnect();
      const trace = parseTrace(text);
      set({
        mode: 'replay',
        trace,
        replayPos: 0,
        replaying: false,
        model: initialModel(),
        log: [],
        console: [],
        trees: {},
        snapshot: null,
      });
    },

    playReplay: () => {
      if (replayTimer || get().trace.length === 0) return;
      set({ replaying: true });
      replayTimer = setInterval(() => {
        const { trace, replayPos, replayRate, model, log } = get();
        if (replayPos >= trace.length) {
          get().pauseReplay();
          return;
        }
        const batch = trace.slice(replayPos, replayPos + replayRate);
        set({
          model: applyEvents(model, batch),
          log: [...log, ...batch].slice(-LOG_KEEP),
          replayPos: replayPos + batch.length,
        });
      }, REPLAY_TICK_MS);
    },

    pauseReplay: () => {
      if (replayTimer) clearInterval(replayTimer);
      replayTimer = null;
      set({ replaying: false });
    },

    seekReplay: (pos) => {
      // The reducer is pure: rebuild from zero to the target position.
      const { trace } = get();
      const clamped = Math.max(0, Math.min(pos, trace.length));
      set({
        model: applyEvents(initialModel(), trace.slice(0, clamped)),
        log: trace.slice(Math.max(0, clamped - LOG_KEEP), clamped),
        replayPos: clamped,
      });
    },

    setReplayRate: (rate) => set({ replayRate: Math.max(1, rate) }),
  };
});
