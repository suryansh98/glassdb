import { useXray } from '../state/store';

// WalTape renders the write-ahead log as a strip of frames appended
// left-to-right. Commit markers seal transactions; a checkpoint sweeps the
// tape empty; recovery replays frames back onto it.
export function WalTape() {
  const wal = useXray((s) => s.model.wal);
  const recovery = useXray((s) => s.model.recovery);

  return (
    <div className="panel">
      <header>
        <h2>write-ahead log</h2>
        <span className="muted">
          {wal.frames.length} frames · {wal.committedFrames} committed · {wal.checkpoints}{' '}
          checkpoints
        </span>
      </header>
      {recovery && (
        <div className={`recovery-banner ${recovery.active ? 'active' : ''}`}>
          {recovery.active
            ? `recovering… ${recovery.framesReplayed} frames replayed`
            : `recovery done: ${recovery.framesReplayed} frames kept, ${recovery.dropped} uncommitted dropped`}
        </div>
      )}
      <div className="wal-tape">
        {wal.frames.length === 0 && <span className="muted">empty — commit something</span>}
        {wal.frames.map((f, i) => {
          const classes = ['wal-frame'];
          if (f.commit) classes.push('commit');
          if (i >= wal.committedFrames) classes.push('pending');
          if (f.recovered) classes.push('recovered');
          return (
            <div
              key={`${f.frame}:${i}`}
              className={classes.join(' ')}
              title={`frame ${f.frame} — page ${f.page}${f.commit ? ' · COMMIT marker' : ''}${
                i >= wal.committedFrames ? ' · not yet committed' : ''
              }${f.recovered ? ' · replayed by recovery' : ''}`}
            >
              {f.page}
            </div>
          );
        })}
      </div>
      <footer className="legend">
        <span>
          <i className="swatch wal-committed" /> committed
        </span>
        <span>
          <i className="swatch wal-pending" /> pending
        </span>
        <span>
          <i className="swatch wal-commit-marker" /> commit marker
        </span>
      </footer>
    </div>
  );
}
