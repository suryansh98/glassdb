import { useEffect, useRef, useState } from 'react';
import { useXray } from '../state/store';
import type { SqlResult } from '../types';

const SAMPLES: [string, string][] = [
  ['create', "CREATE TABLE users (id INTEGER PRIMARY KEY, name TEXT, score REAL);"],
  [
    'insert',
    "INSERT INTO users (name, score) VALUES ('ada', 99.5), ('bob', 42), ('eve', 7);",
  ],
  ['bulk ×200', 'BULK'],
  ['index', 'CREATE INDEX idx_score ON users(score);'],
  ['query', 'SELECT * FROM users WHERE score > 20 ORDER BY score DESC LIMIT 10;'],
  ['explain', 'EXPLAIN SELECT * FROM users WHERE score > 20;'],
  ['txn+rollback', "BEGIN; DELETE FROM users WHERE score < 50; ROLLBACK;"],
  ['checkpoint', 'CHECKPOINT;'],
];

function bulkInsert(): string {
  const rows: string[] = [];
  for (let i = 0; i < 200; i++) {
    const name = `user-${String(i).padStart(3, '0')}`;
    rows.push(`('${name}', ${(i * 7) % 100})`);
  }
  return `INSERT INTO users (name, score) VALUES ${rows.join(', ')};`;
}

export function SqlConsole() {
  const mode = useXray((s) => s.mode);
  const entries = useXray((s) => s.console);
  const runSql = useXray((s) => s.runSql);
  const [sql, setSql] = useState('');
  const scrollRef = useRef<HTMLDivElement>(null);

  useEffect(() => {
    scrollRef.current?.scrollTo({ top: scrollRef.current.scrollHeight });
  }, [entries]);

  const run = async (text: string) => {
    const src = text.trim();
    if (!src) return;
    setSql('');
    await runSql(src);
  };

  return (
    <div className="panel console">
      <header>
        <h2>sql console</h2>
      </header>
      {mode !== 'live' ? (
        <p className="muted pad">
          The console needs a live engine: run <code>glassdb serve mydb.db</code> and connect.
        </p>
      ) : (
        <>
          <div className="console-scroll" ref={scrollRef}>
            {entries.map((e, i) => (
              <div key={i} className="console-entry">
                <pre className="console-sql">{e.sql}</pre>
                {e.error && <div className="console-error">{e.error}</div>}
                {e.results?.map((r, j) => <ResultBlock key={j} result={r} />)}
              </div>
            ))}
          </div>
          <div className="chip-row">
            {SAMPLES.map(([label, text]) => (
              <button
                key={label}
                className="chip"
                onClick={() => run(text === 'BULK' ? bulkInsert() : text)}
              >
                {label}
              </button>
            ))}
          </div>
          <textarea
            value={sql}
            onChange={(e) => setSql(e.target.value)}
            onKeyDown={(e) => {
              if ((e.ctrlKey || e.metaKey) && e.key === 'Enter') run(sql);
            }}
            placeholder="SELECT * FROM users;   (Ctrl+Enter to run)"
            rows={3}
          />
          <button className="run-btn" onClick={() => run(sql)}>
            run
          </button>
        </>
      )}
    </div>
  );
}

function ResultBlock({ result }: { result: SqlResult }) {
  if (!result.columns || result.columns.length === 0) {
    return (
      <div className="console-ok">
        OK{result.rowsAffected > 0 ? `, ${result.rowsAffected} row(s) affected` : ''}
      </div>
    );
  }
  const rows = result.rows ?? [];
  return (
    <div className="result-wrap">
      <table>
        <thead>
          <tr>
            {result.columns.map((c) => (
              <th key={c}>{c}</th>
            ))}
          </tr>
        </thead>
        <tbody>
          {rows.slice(0, 50).map((row, i) => (
            <tr key={i}>
              {row.map((v, j) => (
                <td key={j}>{v === null ? <i className="null">NULL</i> : String(v)}</td>
              ))}
            </tr>
          ))}
        </tbody>
      </table>
      {rows.length > 50 && <div className="muted">… {rows.length - 50} more rows</div>}
    </div>
  );
}
