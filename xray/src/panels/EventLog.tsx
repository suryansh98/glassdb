import { useState } from 'react';
import { useXray } from '../state/store';

const GROUPS = ['stmt', 'plan', 'txn', 'btree', 'page', 'cache', 'wal'];

// EventLog is the raw feed: every event the engine emitted, filterable by
// subsystem prefix.
export function EventLog() {
  const log = useXray((s) => s.log);
  const [enabled, setEnabled] = useState<Record<string, boolean>>(
    Object.fromEntries(GROUPS.map((g) => [g, true])),
  );

  const visible = log
    .filter((ev) => enabled[ev.type.split('.')[0]] ?? true)
    .slice(-250)
    .reverse();

  return (
    <div className="panel eventlog">
      <header>
        <h2>event log</h2>
        <span className="muted">{log.length ? `seq ${log[log.length - 1].seq}` : 'quiet'}</span>
      </header>
      <div className="chip-row">
        {GROUPS.map((g) => (
          <button
            key={g}
            className={`chip ${enabled[g] ? 'on' : 'off'}`}
            onClick={() => setEnabled({ ...enabled, [g]: !enabled[g] })}
          >
            {g}
          </button>
        ))}
      </div>
      <div className="log-scroll">
        {visible.map((ev) => (
          <div key={ev.seq} className="log-line">
            <span className="log-seq">{ev.seq}</span>
            <span className={`log-type lt-${ev.type.split('.')[0]}`}>{ev.type}</span>
            <span className="log-fields">
              {ev.fields
                ? Object.entries(ev.fields)
                    .map(([k, v]) => `${k}=${v}`)
                    .join(' ')
                : ''}
            </span>
          </div>
        ))}
      </div>
    </div>
  );
}
