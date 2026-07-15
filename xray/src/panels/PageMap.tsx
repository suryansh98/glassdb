import { useXray } from '../state/store';

const KIND_LABELS: Record<string, string> = {
  meta: 'meta',
  'table-leaf': 'table leaf',
  'table-interior': 'table interior',
  'index-leaf': 'index leaf',
  'index-interior': 'index interior',
  free: 'free',
};

// PageMap renders the database file as a grid of pages, colored by page
// kind, flashing on reads/writes with the read source (cache/WAL/file).
export function PageMap() {
  const pages = useXray((s) => s.model.pages);
  const splitFlash = useXray((s) => s.model.splitFlash);

  const ids = Object.keys(pages)
    .map(Number)
    .sort((a, b) => a - b);
  const splitSet = new Set(splitFlash.map((f) => f.page));

  return (
    <div className="panel">
      <header>
        <h2>file pages</h2>
        <span className="muted">{ids.length} pages × 4 KiB</span>
      </header>
      <div className="page-grid">
        {ids.map((id) => {
          const p = pages[id];
          const classes = ['page-cell', `pk-${p.kind}`];
          if (splitSet.has(id)) classes.push('split-flash');
          return (
            <div
              key={`${id}:${p.flashSeq ?? 0}`}
              className={classes.join(' ')}
              data-op={p.lastOp}
              data-src={p.src}
              title={`page ${id} — ${KIND_LABELS[p.kind] ?? p.kind}${
                p.lastOp ? ` · last ${p.lastOp}${p.src ? ` from ${p.src}` : ''}` : ''
              }`}
            >
              {id}
            </div>
          );
        })}
      </div>
      <footer className="legend">
        {Object.entries(KIND_LABELS).map(([kind, label]) => (
          <span key={kind}>
            <i className={`swatch pk-${kind}`} /> {label}
          </span>
        ))}
        <span className="muted">reads flash by source: cache / wal / file</span>
      </footer>
    </div>
  );
}
