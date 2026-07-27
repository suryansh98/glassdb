import { useXray } from '../state/store';
import type { TreeDump } from '../types';

interface LaidNode {
  x: number;
  y: number;
  w: number;
  dump: TreeDump;
}

interface Edge {
  x1: number;
  y1: number;
  x2: number;
  y2: number;
}

const NODE_H = 34;
const LEVEL_GAP = 72;
const SIBLING_GAP = 14;

function nodeLabel(d: TreeDump): string {
  const keys = d.keys ?? [];
  const shown = keys.slice(0, 4).join(' · ');
  const extra = d.nCells > 4 ? ` +${d.nCells - 4}` : '';
  return shown === '' ? '(empty)' : shown + extra;
}

// layout places nodes with a classic post-order walk: leaves advance a
// cursor, parents center over their children.
function layout(root: TreeDump): { nodes: LaidNode[]; edges: Edge[]; width: number; height: number } {
  const nodes: LaidNode[] = [];
  const edges: Edge[] = [];
  let cursorX = 10;
  let maxDepth = 0;

  const place = (d: TreeDump, depth: number): LaidNode => {
    maxDepth = Math.max(maxDepth, depth);
    const w = Math.max(56, nodeLabel(d).length * 7.2 + 18);
    const y = 10 + depth * LEVEL_GAP;
    let x: number;
    if (!d.children || d.children.length === 0) {
      x = cursorX;
      cursorX += w + SIBLING_GAP;
    } else {
      const kids = d.children.map((c) => place(c, depth + 1));
      const first = kids[0];
      const last = kids[kids.length - 1];
      x = (first.x + last.x + last.w) / 2 - w / 2;
      for (const kid of kids) {
        edges.push({
          x1: x + w / 2,
          y1: y + NODE_H,
          x2: kid.x + kid.w / 2,
          y2: kid.y,
        });
      }
      // A skinny parent must still clear the sibling cursor.
      cursorX = Math.max(cursorX, x + w + SIBLING_GAP);
    }
    const laid = { x, y, w, dump: d };
    nodes.push(laid);
    return laid;
  };
  place(root, 0);

  // Edges were computed with child positions from recursion; fix parent
  // anchors after centering (they used the final x already, so no fix
  // needed — the recursion computes x before pushing edges).
  const width = Math.max(cursorX + 10, 300);
  const height = 20 + (maxDepth + 1) * LEVEL_GAP;
  return { nodes, edges, width, height };
}

// BTreeView renders the live structure of a table or index B+tree as SVG.
// Nodes that split recently pulse. Live mode only — trees are fetched from
// the running engine after each statement.
export function BTreeView() {
  const trees = useXray((s) => s.trees);
  const selected = useXray((s) => s.selectedTree);
  const selectTree = useXray((s) => s.selectTree);
  const splitFlash = useXray((s) => s.model.splitFlash);
  const mode = useXray((s) => s.mode);

  const names = Object.keys(trees).sort();
  const dump = trees[selected];
  const splitSet = new Set(splitFlash.map((f) => f.page));

  return (
    <div className="panel">
      <header>
        <h2>b+tree</h2>
        {names.length > 0 && (
          <select value={selected} onChange={(e) => selectTree(e.target.value)}>
            {names.map((n) => (
              <option key={n} value={n}>
                {n}
              </option>
            ))}
          </select>
        )}
      </header>
      {mode === 'replay' && (
        <p className="muted pad">
          Tree structure is fetched live from a running engine, so it is not available during trace
          replay. Watch the page map and WAL instead.
        </p>
      )}
      {mode === 'live' && !dump && <p className="muted pad">No tables yet — CREATE one.</p>}
      {mode === 'live' && dump && <TreeSvg dump={dump} splitSet={splitSet} />}
      {mode === 'idle' && <p className="muted pad">Connect to a running `glassdb serve`.</p>}
    </div>
  );
}

function TreeSvg({ dump, splitSet }: { dump: TreeDump; splitSet: Set<number> }) {
  const { nodes, edges, width, height } = layout(dump);
  return (
    <div className="tree-scroll">
      <svg width={width} height={height}>
        {edges.map((e, i) => (
          <path
            key={i}
            className="tree-edge"
            d={`M ${e.x1} ${e.y1} C ${e.x1} ${e.y1 + 24}, ${e.x2} ${e.y2 - 24}, ${e.x2} ${e.y2}`}
          />
        ))}
        {nodes.map((n) => (
          <g key={n.dump.page} transform={`translate(${n.x}, ${n.y})`}>
            <rect
              className={[
                'tree-node',
                `pk-${n.dump.kind}`,
                splitSet.has(n.dump.page) ? 'split-flash' : '',
              ].join(' ')}
              width={n.w}
              height={NODE_H}
              rx={6}
            />
            <text className="tree-page" x={6} y={12}>
              p{n.dump.page}
            </text>
            <text className="tree-keys" x={n.w / 2} y={26} textAnchor="middle">
              {nodeLabel(n.dump)}
            </text>
          </g>
        ))}
      </svg>
    </div>
  );
}
