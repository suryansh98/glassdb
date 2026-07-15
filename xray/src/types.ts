// Wire types shared with the Go engine (see engine/db and engine/events).

export interface EngineEvent {
  seq: number;
  type: string;
  fields?: Record<string, unknown>;
}

export interface PageInfo {
  id: number;
  kind: string;
}

export interface ColumnInfo {
  name: string;
  type: string;
  pk?: boolean;
}

export interface IndexInfo {
  name: string;
  column: string;
  root: number;
}

export interface TableInfo {
  name: string;
  columns: ColumnInfo[];
  root: number;
  indexes: IndexInfo[];
}

export interface Snapshot {
  pages: PageInfo[];
  tables: TableInfo[] | null;
  wal: { frames: number; committed: number };
  cache: { cap: number; entries: number[] | null };
  inTxn: boolean;
}

export interface TreeDump {
  page: number;
  kind: string;
  nCells: number;
  keys?: string[];
  children?: TreeDump[];
  nextLeaf?: number;
  truncated?: boolean;
}

export interface SqlResult {
  columns?: string[];
  rows?: unknown[][];
  rowsAffected: number;
}

export interface ConsoleEntry {
  sql: string;
  results?: SqlResult[];
  error?: string;
}
