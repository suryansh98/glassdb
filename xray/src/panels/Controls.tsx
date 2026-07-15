import { useXray } from '../state/store';

// Controls drives the engine's event gate (live) or the replay clock.
export function Controls() {
  const mode = useXray((s) => s.mode);
  if (mode === 'live') return <LiveControls />;
  if (mode === 'replay') return <ReplayControls />;
  return null;
}

function LiveControls() {
  const paused = useXray((s) => s.paused);
  const speedMs = useXray((s) => s.speedMs);
  const connected = useXray((s) => s.connected);
  const inTxn = useXray((s) => s.model.inTxn);
  const { setPaused, step, setSpeed, crash } = useXray.getState();

  return (
    <div className="controls">
      <button onClick={() => setPaused(!paused)}>{paused ? '▶ resume' : '⏸ pause engine'}</button>
      <button onClick={step} disabled={!paused} title="release one event while paused">
        step
      </button>
      <label>
        slow-mo
        <input
          type="range"
          min={0}
          max={100}
          value={speedMs}
          onChange={(e) => setSpeed(Number(e.target.value))}
        />
        {speedMs}ms/event
      </label>
      <span className={`txn-badge ${inTxn ? 'on' : ''}`}>{inTxn ? 'IN TXN' : 'autocommit'}</span>
      <button
        className="danger"
        onClick={() => {
          if (window.confirm('kill -9 the engine mid-flight? Restart serve to watch recovery.')) {
            crash();
          }
        }}
      >
        💥 crash
      </button>
      {!connected && <span className="disconnected">disconnected — is `glassdb serve` running?</span>}
    </div>
  );
}

function ReplayControls() {
  const replaying = useXray((s) => s.replaying);
  const pos = useXray((s) => s.replayPos);
  const trace = useXray((s) => s.trace);
  const rate = useXray((s) => s.replayRate);
  const { playReplay, pauseReplay, seekReplay, setReplayRate } = useXray.getState();

  return (
    <div className="controls">
      <button onClick={() => (replaying ? pauseReplay() : playReplay())}>
        {replaying ? '⏸ pause' : '▶ play'}
      </button>
      <label className="scrubber">
        <input
          type="range"
          min={0}
          max={trace.length}
          value={pos}
          onChange={(e) => seekReplay(Number(e.target.value))}
        />
        {pos}/{trace.length} events
      </label>
      <label>
        speed
        <input
          type="range"
          min={1}
          max={200}
          value={rate}
          onChange={(e) => setReplayRate(Number(e.target.value))}
        />
        ×{rate}
      </label>
    </div>
  );
}
