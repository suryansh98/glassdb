import { useXray } from '../state/store';

// BufferPool shows the page cache: which pages are resident (most recently
// used first) and how the hit/miss/eviction counters evolve.
export function BufferPool() {
  const cache = useXray((s) => s.model.cache);
  const total = cache.hits + cache.misses;
  const hitRate = total === 0 ? 0 : Math.round((cache.hits / total) * 100);

  return (
    <div className="panel">
      <header>
        <h2>buffer pool</h2>
        <span className="muted">
          {cache.entries.length}/{cache.cap} pages
        </span>
      </header>
      <div className="stat-row">
        <div className="stat">
          <b>{hitRate}%</b>
          <span>hit rate</span>
        </div>
        <div className="stat">
          <b>{cache.hits}</b>
          <span>hits</span>
        </div>
        <div className="stat">
          <b>{cache.misses}</b>
          <span>misses</span>
        </div>
        <div className="stat">
          <b>{cache.evictions}</b>
          <span>evictions</span>
        </div>
        <div className="stat">
          <b>{cache.spills}</b>
          <span>spills</span>
        </div>
      </div>
      <div className="pool-strip" title="resident pages, most recently used first">
        {cache.entries.slice(0, 64).map((id, i) => (
          <span key={id} className="pool-slot" style={{ opacity: 1 - i * 0.012 }}>
            {id}
          </span>
        ))}
        {cache.entries.length === 0 && <span className="muted">cold cache</span>}
      </div>
    </div>
  );
}
