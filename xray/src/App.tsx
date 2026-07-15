import { useRef, useState } from 'react';
import { BTreeView } from './panels/BTreeView';
import { BufferPool } from './panels/BufferPool';
import { Controls } from './panels/Controls';
import { EventLog } from './panels/EventLog';
import { PageMap } from './panels/PageMap';
import { SqlConsole } from './panels/SqlConsole';
import { WalTape } from './panels/WalTape';
import { useXray } from './state/store';

export default function App() {
  const mode = useXray((s) => s.mode);
  const connected = useXray((s) => s.connected);
  const connect = useXray((s) => s.connect);
  const disconnect = useXray((s) => s.disconnect);
  const loadTrace = useXray((s) => s.loadTrace);
  const [url, setUrl] = useState('http://127.0.0.1:4980');
  const [connectError, setConnectError] = useState('');
  const fileRef = useRef<HTMLInputElement>(null);

  const doConnect = async () => {
    setConnectError('');
    try {
      await connect(url);
    } catch (err) {
      setConnectError(`cannot connect — run: glassdb serve mydb.db  (${String(err)})`);
    }
  };

  const onFile = async (f: File | undefined) => {
    if (!f) return;
    loadTrace(await f.text());
  };

  const loadDemo = async () => {
    setConnectError('');
    try {
      const res = await fetch('demo-trace.jsonl');
      if (!res.ok) throw new Error(`HTTP ${res.status}`);
      loadTrace(await res.text());
    } catch (err) {
      setConnectError(`demo trace unavailable (${String(err)})`);
    }
  };

  return (
    <div className="app">
      <header className="topbar">
        <h1>
          glassdb <span>x-ray</span>
        </h1>
        <div className="conn">
          {mode === 'idle' && (
            <>
              <input
                value={url}
                onChange={(e) => setUrl(e.target.value)}
                spellCheck={false}
                aria-label="engine url"
              />
              <button onClick={doConnect}>connect</button>
              <span className="muted">or</span>
              <button onClick={() => fileRef.current?.click()}>load trace</button>
              <button onClick={loadDemo}>demo trace</button>
              <input
                ref={fileRef}
                type="file"
                accept=".jsonl,.txt"
                hidden
                onChange={(e) => onFile(e.target.files?.[0])}
              />
            </>
          )}
          {mode !== 'idle' && (
            <>
              <span className={`dot ${mode === 'live' ? (connected ? 'ok' : 'bad') : 'replay'}`} />
              <span className="muted">
                {mode === 'live' ? `live · ${url}` : 'trace replay'}
              </span>
              <button onClick={disconnect}>disconnect</button>
            </>
          )}
        </div>
      </header>
      {connectError && <div className="connect-error">{connectError}</div>}
      <Controls />

      {mode === 'idle' ? (
        <Landing onConnect={doConnect} onDemo={loadDemo} />
      ) : (
        <main className="grid">
          <div className="col">
            <SqlConsole />
          </div>
          <div className="col">
            <PageMap />
            <BTreeView />
          </div>
          <div className="col">
            <WalTape />
            <BufferPool />
            <EventLog />
          </div>
        </main>
      )}
    </div>
  );
}

function Landing({ onConnect, onDemo }: { onConnect: () => void; onDemo: () => void }) {
  return (
    <main className="landing">
      <h2>a small database you can see inside</h2>
      <p>
        glassdb is a real embedded SQL database — pages, B+tree, write-ahead log, crash recovery —
        that streams every internal action as events. This page is the X-ray.
      </p>
      <ol>
        <li>
          <code>go run github.com/suryansh98/glassdb/cmd/glassdb@latest serve mydb.db --cache 32</code>
        </li>
        <li>
          <button className="inline" onClick={onConnect}>
            connect
          </button>{' '}
          and run SQL — watch pages flash, the B+tree split, the WAL grow.
        </li>
        <li>
          Press <b>💥 crash</b> mid-transaction, restart serve, reconnect — and watch recovery
          replay the log.
        </li>
      </ol>
      <p>
        No engine handy? <button className="inline" onClick={onDemo}>replay the demo trace</button>{' '}
        recorded from a real session.
      </p>
    </main>
  );
}
