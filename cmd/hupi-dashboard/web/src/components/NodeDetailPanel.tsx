import { X } from "lucide-react";
import { MemoryMapGraph, MemoryMapNode, MemoryMapTopicsResponse } from "../lib/api";

interface Props {
  nodeId: string;
  graph: MemoryMapGraph;
  topics: MemoryMapTopicsResponse | null;
  onClose: () => void;
  onSelectNode: (nodeId: string) => void;
}

function buildNodeIndex(graph: MemoryMapGraph, topics: MemoryMapTopicsResponse | null): Map<string, MemoryMapNode> {
  const index = new Map<string, MemoryMapNode>();
  for (const n of graph.nodes) index.set(n.id, n);
  if (topics?.enabled) for (const n of topics.topics ?? []) index.set(n.id, n);
  return index;
}

// NodeDetailPanel slides in from the right on node tap (MemoryMap.tsx
// wires the selection state). Does no extra fetch — the full
// MemoryMapGraph/MemoryMapTopicsResponse are already in memory by the
// time a node can be clicked, so this is a pure lookup/render.
export function NodeDetailPanel({ nodeId, graph, topics, onClose, onSelectNode }: Props) {
  const nodes = buildNodeIndex(graph, topics);
  const node = nodes.get(nodeId);
  if (!node) return null;

  const label = (id: string) => nodes.get(id)?.label ?? id;

  return (
    <div className="absolute right-0 top-0 h-full w-96 overflow-y-auto rounded-xl border border-white/10 bg-navy-900/95 p-5 shadow-2xl backdrop-blur">
      <div className="mb-4 flex items-center justify-between">
        <h3 className="text-base font-semibold text-fog-100">{node.label}</h3>
        <button onClick={onClose} className="text-fog-500 transition-colors hover:text-fog-100" aria-label="Close">
          <X size={18} />
        </button>
      </div>

      {node.type === "entity" && node.entity && (
        <EntityDetail node={node} graph={graph} label={label} onSelectNode={onSelectNode} />
      )}
      {node.type === "conversation" && node.conversation && (
        <ConversationDetail node={node} graph={graph} topics={topics} label={label} onSelectNode={onSelectNode} />
      )}
      {node.type === "topic" && node.topic && (
        <TopicDetail nodeId={nodeId} node={node} graph={graph} topics={topics} label={label} onSelectNode={onSelectNode} />
      )}
    </div>
  );
}

function Chip({ onClick, children }: { onClick: () => void; children: React.ReactNode }) {
  return (
    <button
      onClick={onClick}
      className="rounded-full border border-white/10 bg-navy-800 px-2.5 py-1 text-xs text-fog-300 transition-colors hover:border-ember-500/30 hover:text-fog-100"
    >
      {children}
    </button>
  );
}

function EntityDetail({
  node,
  graph,
  label,
  onSelectNode,
}: {
  node: MemoryMapNode;
  graph: MemoryMapGraph;
  label: (id: string) => string;
  onSelectNode: (id: string) => void;
}) {
  const relationships = graph.edges.filter(
    (e) => e.type === "relationship" && (e.source === node.id || e.target === node.id),
  );
  return (
    <div className="space-y-3 text-sm">
      <dl className="grid grid-cols-2 gap-y-1">
        <dt className="text-fog-500">Kind</dt>
        <dd className="text-fog-100">{node.entity?.kind}</dd>
      </dl>
      <div>
        <p className="mb-1.5 text-xs uppercase tracking-wide text-fog-700">Relationships</p>
        {relationships.length === 0 ? (
          <p className="text-fog-500">None in this range.</p>
        ) : (
          <ul className="space-y-1.5">
            {relationships.map((r) => (
              <li key={r.id} className="flex items-baseline justify-between gap-2">
                <span>
                  <button onClick={() => onSelectNode(r.source)} className="text-fog-100 hover:underline">
                    {label(r.source)}
                  </button>{" "}
                  <span className="font-mono-tight text-ember-500">{r.relationship?.predicate}</span>{" "}
                  <button onClick={() => onSelectNode(r.target)} className="text-fog-100 hover:underline">
                    {label(r.target)}
                  </button>
                </span>
                <span className="shrink-0 font-mono-tight text-xs text-fog-700">
                  {r.relationship?.valid_from || r.relationship?.valid_until
                    ? `${r.relationship?.valid_from ?? "…"} – ${r.relationship?.valid_until ?? "now"}`
                    : "dates unknown"}
                </span>
              </li>
            ))}
          </ul>
        )}
      </div>
    </div>
  );
}

function ConversationDetail({
  node,
  graph,
  topics,
  label,
  onSelectNode,
}: {
  node: MemoryMapNode;
  graph: MemoryMapGraph;
  topics: MemoryMapTopicsResponse | null;
  label: (id: string) => string;
  onSelectNode: (id: string) => void;
}) {
  const mentions = graph.edges.filter((e) => e.type === "mentions" && e.source === node.id);
  const topicsEntry = topics?.conversations?.find((c) => `conversation:${c.episode_id}` === node.id);
  const taggedEdges = topics?.edges?.filter((e) => e.type === "tagged_with" && e.source === node.id) ?? [];

  return (
    <div className="space-y-3 text-sm">
      <p className="font-mono-tight text-xs text-fog-500">{node.conversation && new Date(node.conversation.ts).toLocaleString()}</p>

      {topicsEntry ? (
        <p className="leading-relaxed text-fog-300">{topicsEntry.excerpt || "No excerpt."}</p>
      ) : (
        <p className="text-fog-500">{topics?.enabled === false ? "Decrypt-on-view is disabled for this scope." : "Loading excerpt…"}</p>
      )}

      {taggedEdges.length > 0 && (
        <div>
          <p className="mb-1.5 text-xs uppercase tracking-wide text-fog-700">Topics</p>
          <div className="flex flex-wrap gap-1.5">
            {taggedEdges.map((e) => (
              <Chip key={e.id} onClick={() => onSelectNode(e.target)}>
                {label(e.target)}
              </Chip>
            ))}
          </div>
        </div>
      )}

      <div>
        <p className="mb-1.5 text-xs uppercase tracking-wide text-fog-700">Entities mentioned</p>
        {mentions.length === 0 ? (
          <p className="text-fog-500">None.</p>
        ) : (
          <div className="flex flex-wrap gap-1.5">
            {mentions.map((e) => (
              <Chip key={e.id} onClick={() => onSelectNode(e.target)}>
                {label(e.target)}
              </Chip>
            ))}
          </div>
        )}
      </div>
    </div>
  );
}

function TopicDetail({
  nodeId,
  node,
  graph,
  topics,
  label,
  onSelectNode,
}: {
  nodeId: string;
  node: MemoryMapNode;
  graph: MemoryMapGraph;
  topics: MemoryMapTopicsResponse | null;
  label: (id: string) => string;
  onSelectNode: (id: string) => void;
}) {
  const taggedConversations = (topics?.edges ?? []).filter((e) => e.type === "tagged_with" && e.target === nodeId);
  const conversationIDs = new Set(taggedConversations.map((e) => e.source));
  const coOccurringEntities = new Map<string, string>();
  for (const m of graph.edges) {
    if (m.type === "mentions" && conversationIDs.has(m.source)) {
      coOccurringEntities.set(m.target, label(m.target));
    }
  }

  return (
    <div className="space-y-3 text-sm">
      <dl className="grid grid-cols-2 gap-y-1">
        <dt className="text-fog-500">Mentions</dt>
        <dd className="text-fog-100">{node.topic?.count}</dd>
      </dl>
      <div>
        <p className="mb-1.5 text-xs uppercase tracking-wide text-fog-700">Conversations</p>
        {taggedConversations.length === 0 ? (
          <p className="text-fog-500">None.</p>
        ) : (
          <div className="flex flex-wrap gap-1.5">
            {taggedConversations.map((e) => (
              <Chip key={e.id} onClick={() => onSelectNode(e.source)}>
                {label(e.source)}
              </Chip>
            ))}
          </div>
        )}
      </div>
      <div>
        <p className="mb-1.5 text-xs uppercase tracking-wide text-fog-700">Co-occurring entities</p>
        {coOccurringEntities.size === 0 ? (
          <p className="text-fog-500">None.</p>
        ) : (
          <div className="flex flex-wrap gap-1.5">
            {[...coOccurringEntities.entries()].map(([id, name]) => (
              <Chip key={id} onClick={() => onSelectNode(id)}>
                {name}
              </Chip>
            ))}
          </div>
        )}
      </div>
    </div>
  );
}
