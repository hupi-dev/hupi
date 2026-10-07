import { useEffect, useMemo, useRef, useState } from "react";
import { api, MemoryMapGraph, MemoryMapTopicsResponse } from "../lib/api";
import { mergeMemoryMapElements, MemoryMapLayoutMode } from "../lib/memoryMapGraph";
import { MemoryGraph } from "../components/MemoryGraph";
import { NodeDetailPanel } from "../components/NodeDetailPanel";

const DAY_PRESETS = [
  { label: "Last 7 days", days: 7 },
  { label: "Last 30 days", days: 30 },
  { label: "Last 90 days", days: 90 },
  { label: "Last 6 months", days: 180 },
  { label: "Last year", days: 365 },
  // ~100 years — simplest way to express "no lower bound" given the
  // API's days-based filter; no dedicated "all time" wire value exists
  // (or is needed) since the server already caps result size regardless.
  { label: "All time", days: 36_500 },
];

// Above this many combined elements, skip running fcose client-side and
// show the same "narrow your filter" message the server's own
// truncated flag triggers — in-browser force layouts get visibly slow
// and jittery well before thousands of nodes.
const CLIENT_ELEMENT_CEILING = 1500;

// MemoryMap is the dashboard's graph view: conversations, entities,
// relationships, and topics for the current scope, filtered by date
// range. Fetches the cheap always-on graph and the decrypt-gated
// topics overlay in parallel (memory_map.go's own two-endpoint split)
// and merges them client-side (lib/memoryMapGraph.ts) so the graph
// renders immediately and topics progressively enhance it.
export function MemoryMap() {
  const [days, setDays] = useState(30);
  const [layoutMode, setLayoutMode] = useState<MemoryMapLayoutMode>("network");
  const [graph, setGraph] = useState<MemoryMapGraph | null>(null);
  const [topics, setTopics] = useState<MemoryMapTopicsResponse | null>(null);
  const [selectedNodeId, setSelectedNodeId] = useState<string | null>(null);
  // A monotonic sequence number, not a real fetch abort — same
  // "ignore a stale response" convention Dashboard.tsx's own useLoaded
  // hook already uses (a boolean cancelled flag there; a counter here
  // since two separate fetches share one filter change and both need
  // to agree on which round is current).
  const requestSeq = useRef(0);

  useEffect(() => {
    const seq = ++requestSeq.current;
    setGraph(null);
    setTopics(null);
    setSelectedNodeId(null);

    api.memoryMap({ days }).then((g) => {
      if (requestSeq.current === seq) setGraph(g);
    });
    api.memoryMapTopics({ days }).then((t) => {
      if (requestSeq.current === seq) setTopics(t);
    });
  }, [days]);

  const elements = useMemo(() => (graph ? mergeMemoryMapElements(graph, topics) : []), [graph, topics]);

  // Timeline mode pins conversation nodes to an x position derived from
  // their own timestamp — see fcoseLayoutConfig's own doc comment for
  // why only conversations (not entities/topics) get this treatment.
  const conversationXPositions = useMemo(() => {
    if (layoutMode !== "timeline" || !graph) return undefined;
    const convNodes = graph.nodes.filter((n) => n.type === "conversation" && n.conversation);
    if (convNodes.length === 0) return undefined;
    const times = convNodes.map((n) => new Date(n.conversation!.ts).getTime());
    const min = Math.min(...times);
    const span = Math.max(1, Math.max(...times) - min);
    const layoutWidth = 2000; // layout-space units, not pixels — fcose/cytoscape fit the container regardless of this scale
    return convNodes.map((n) => ({
      nodeId: n.id,
      x: ((new Date(n.conversation!.ts).getTime() - min) / span) * layoutWidth,
    }));
  }, [layoutMode, graph]);

  const overCeiling = elements.length > CLIENT_ELEMENT_CEILING;
  const truncated = Boolean(graph?.truncated || topics?.truncated);

  return (
    <div className="relative flex h-[calc(100vh-14rem)] flex-col gap-3">
      <div className="flex flex-wrap items-center gap-3">
        <select
          className="rounded-lg border border-white/15 bg-navy-900 px-2 py-1.5 text-sm text-fog-100 transition-colors hover:border-white/30"
          value={days}
          onChange={(e) => setDays(Number(e.target.value))}
        >
          {DAY_PRESETS.map((p) => (
            <option key={p.days} value={p.days}>
              {p.label}
            </option>
          ))}
        </select>
        <div className="flex overflow-hidden rounded-lg border border-white/15 text-sm">
          {(["network", "timeline", "circular"] as MemoryMapLayoutMode[]).map((mode) => (
            <button
              key={mode}
              onClick={() => setLayoutMode(mode)}
              className={`px-3 py-1.5 capitalize transition-colors ${
                layoutMode === mode ? "bg-ember-500/20 text-ember-500" : "text-fog-500 hover:bg-white/5"
              }`}
            >
              {mode}
            </button>
          ))}
        </div>
        {graph && truncated && (
          <span className="rounded-full border border-ember-500/30 bg-ember-500/10 px-3 py-1 text-xs text-ember-500">
            Showing a partial view — narrow the date range to see the rest.
          </span>
        )}
      </div>

      <div className="relative min-h-0 flex-1 rounded-xl border border-white/10 bg-navy-900/60">
        {graph === null ? (
          <p className="p-5 text-sm text-fog-500">Loading…</p>
        ) : elements.length === 0 ? (
          <p className="p-5 text-sm text-fog-500">Nothing in this range yet.</p>
        ) : overCeiling ? (
          <div className="flex h-full items-center justify-center p-5 text-center text-sm text-fog-500">
            This range has {elements.length} elements — too many to lay out smoothly. Narrow the date filter.
          </div>
        ) : (
          <MemoryGraph
            elements={elements}
            layoutMode={layoutMode}
            conversationXPositions={conversationXPositions}
            selectedNodeId={selectedNodeId}
            onSelectNode={(id) => setSelectedNodeId(id || null)}
          />
        )}
        {selectedNodeId && graph && (
          <NodeDetailPanel
            nodeId={selectedNodeId}
            graph={graph}
            topics={topics}
            onClose={() => setSelectedNodeId(null)}
            onSelectNode={(id) => setSelectedNodeId(id)}
          />
        )}
      </div>
    </div>
  );
}
