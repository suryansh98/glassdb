// REST + SSE client for a live `glassdb serve`, plus trace-file loading.

import type { EngineEvent, Snapshot, SqlResult, TreeDump } from './types';

export async function fetchSnapshot(baseUrl: string): Promise<Snapshot> {
  const res = await fetch(`${baseUrl}/snapshot`);
  if (!res.ok) throw new Error(`snapshot: HTTP ${res.status}`);
  return res.json();
}

export async function fetchTree(baseUrl: string, name: string): Promise<TreeDump> {
  const res = await fetch(`${baseUrl}/tree?name=${encodeURIComponent(name)}`);
  if (!res.ok) throw new Error(`tree ${name}: HTTP ${res.status}`);
  return res.json();
}

export async function postSql(baseUrl: string, sql: string): Promise<SqlResult[]> {
  const res = await fetch(`${baseUrl}/sql`, {
    method: 'POST',
    headers: { 'Content-Type': 'application/json' },
    body: JSON.stringify({ sql }),
  });
  const body = await res.json();
  if (!res.ok) throw new Error(body.error ?? `HTTP ${res.status}`);
  return body.results ?? [];
}

export async function postControl(
  baseUrl: string,
  action: 'pause' | 'resume' | 'step' | '',
  speedMs?: number,
): Promise<void> {
  await fetch(`${baseUrl}/control`, {
    method: 'POST',
    headers: { 'Content-Type': 'application/json' },
    body: JSON.stringify(speedMs === undefined ? { action } : { action, speedMs }),
  });
}

export async function postCrash(baseUrl: string): Promise<void> {
  await fetch(`${baseUrl}/crash`, { method: 'POST', body: '{}' }).catch(() => {
    // The process exits mid-response; a network error here is success.
  });
}

// openEvents subscribes to the SSE stream; the returned function closes it.
export function openEvents(
  baseUrl: string,
  onEvent: (ev: EngineEvent) => void,
  onStateChange: (connected: boolean) => void,
): () => void {
  const source = new EventSource(`${baseUrl}/events`);
  source.onopen = () => onStateChange(true);
  source.onerror = () => onStateChange(false);
  source.onmessage = (msg) => {
    try {
      onEvent(JSON.parse(msg.data) as EngineEvent);
    } catch {
      // ignore malformed lines
    }
  };
  return () => source.close();
}

// parseTrace parses a .jsonl trace recorded with `glassdb --record`.
export function parseTrace(text: string): EngineEvent[] {
  const events: EngineEvent[] = [];
  for (const line of text.split('\n')) {
    const trimmed = line.trim();
    if (!trimmed) continue;
    try {
      const ev = JSON.parse(trimmed) as EngineEvent;
      if (typeof ev.seq === 'number' && typeof ev.type === 'string') events.push(ev);
    } catch {
      // skip malformed lines
    }
  }
  return events;
}
