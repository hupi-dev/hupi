// Pure, DOM-free functions for the memory map: merging the two
// memory-map API responses into one cytoscape element list, the
// stylesheet, and the fcose layout config. Kept pure and separate from
// any component specifically so this logic is unit-testable without
// jsdom's poor canvas support — see useCytoscape.ts for the only place
// that actually touches a live cytoscape instance.
// Type-only default import, qualified-name access (cytoscape.X) rather
// than named imports: this package's `export = cytoscape` + internal
// namespace pattern doesn't resolve every named type the same way under
// this project's isolatedModules setting — Stylesheet (a type alias,
// unlike the interface-based Core/ElementDefinition) specifically fails
// to resolve as a named import here even though it exists.
import type cytoscape from "cytoscape";
import { MemoryMapEdge, MemoryMapGraph, MemoryMapNode, MemoryMapTopicsResponse } from "./api";

// kindColor is a small, fixed palette for entities.kind — same spirit
// as the existing ember/fog/navy tokens (tailwind.config.js), just
// giving each of the 7 known kinds its own hue so the graph reads at a
// glance without needing a legend for every node.
const kindColor: Record<string, string> = {
  person: "#ff8a4c", // ember-500
  project: "#60a5fa", // blue-400
  preference: "#c084fc", // purple-400
  skill: "#34d399", // emerald-400
  place: "#fbbf24", // amber-400
  organization: "#f472b6", // pink-400
  self_model: "#94a3b8", // slate-400
};

function entityColor(kind: string): string {
  return kindColor[kind] ?? "#8991a8"; // fog-500 fallback for an unrecognized kind
}

/**
 * mergeMemoryMapElements combines the cheap graph response and the
 * (optional — may not have loaded yet, or be disabled) topics overlay
 * into one cytoscape element list. Entity/conversation nodes and
 * relationship/mention edges always come from `graph`; topic nodes and
 * tagged_with edges come from `topics` only when present and enabled.
 */
export function mergeMemoryMapElements(graph: MemoryMapGraph, topics: MemoryMapTopicsResponse | null): cytoscape.ElementDefinition[] {
  const allEdges = [...graph.edges, ...(topics?.enabled ? topics.edges ?? [] : [])];

  // Entity node size reflects how many edges touch it (relationships +
  // mentions) — the backend doesn't compute or send a mention count
  // itself, so it's derived client-side from the already-fetched edge
  // list rather than adding another round trip.
  const degree = new Map<string, number>();
  for (const edge of allEdges) {
    degree.set(edge.source, (degree.get(edge.source) ?? 0) + 1);
    degree.set(edge.target, (degree.get(edge.target) ?? 0) + 1);
  }

  const elements: cytoscape.ElementDefinition[] = [];
  for (const node of graph.nodes) {
    elements.push(nodeToElement(node, degree.get(node.id) ?? 0));
  }
  for (const edge of graph.edges) {
    elements.push(edgeToElement(edge));
  }
  if (topics?.enabled) {
    for (const node of topics.topics ?? []) {
      elements.push(nodeToElement(node, degree.get(node.id) ?? 0));
    }
    for (const edge of topics.edges ?? []) {
      elements.push(edgeToElement(edge));
    }
  }
  return elements;
}

function nodeToElement(node: MemoryMapNode, degree: number): cytoscape.ElementDefinition {
  return {
    data: {
      id: node.id,
      label: node.label,
      type: node.type,
      mentionCount: degree,
      weight: node.topic?.count ?? degree,
    },
    classes: node.type + (node.entity ? ` kind-${node.entity.kind}` : ""),
  };
}

function edgeToElement(edge: MemoryMapEdge): cytoscape.ElementDefinition {
  return {
    data: {
      id: edge.id,
      source: edge.source,
      target: edge.target,
      type: edge.type,
      label: edge.relationship?.predicate ?? "",
    },
    classes: edge.type,
  };
}

/** elementIDs is a small helper for useCytoscape's diff-on-update logic. */
export function elementIDs(elements: cytoscape.ElementDefinition[]): Set<string> {
  return new Set(elements.map((e) => e.data.id as string));
}

// CyStyleRule is a minimal local stand-in for cytoscape.Stylesheet: that
// type is a union-type alias inside cytoscape's `export =`-namespaced
// declarations, which doesn't resolve reliably here even via a
// qualified (cytoscape.Stylesheet) reference under this project's
// isolatedModules setting — useCytoscape.ts casts this array once, at
// the one real cytoscape({ style: ... }) call site, rather than fighting
// that resolution for an internal-only array shape.
export interface CyStyleRule {
  selector: string;
  style: Record<string, string | number>;
}

// memoryMapStylesheet renders three node shapes (entity/conversation/
// topic) and three edge styles (relationship/mentions/tagged_with) —
// see Dashboard.tsx's existing "Entity relationships" card for the same
// color/weight vocabulary this reuses (ember accent for relationships).
export const memoryMapStylesheet: CyStyleRule[] = [
  {
    selector: "node",
    style: {
      label: "data(label)",
      "font-size": 9,
      color: "#f5f6fa", // fog-100
      "text-wrap": "ellipsis",
      "text-max-width": "80px",
      "text-valign": "bottom",
      "text-margin-y": 4,
    },
  },
  {
    selector: "node.entity",
    style: {
      shape: "ellipse",
      width: "mapData(mentionCount, 0, 20, 18, 46)",
      height: "mapData(mentionCount, 0, 20, 18, 46)",
      "background-color": "#8991a8",
    },
  },
  ...Object.keys(kindColor).map((kind) => ({
    selector: `node.kind-${kind}`,
    style: { "background-color": entityColor(kind) },
  })),
  {
    selector: "node.conversation",
    style: {
      shape: "round-rectangle",
      width: 14,
      height: 14,
      "background-color": "#ff8a4c", // ember-500
      "background-opacity": 0.85,
    },
  },
  {
    selector: "node.topic",
    style: {
      shape: "diamond",
      width: "mapData(weight, 0, 30, 16, 40)",
      height: "mapData(weight, 0, 30, 16, 40)",
      "background-color": "#2dd4bf", // teal-400 — distinct from both entity and conversation palettes
    },
  },
  {
    selector: "edge",
    style: {
      "curve-style": "bezier",
      width: 1,
    },
  },
  {
    selector: "edge.relationship",
    style: {
      "line-color": "#ff8a4c", // ember-500
      "target-arrow-color": "#ff8a4c",
      "target-arrow-shape": "triangle",
      opacity: 0.8,
    },
  },
  {
    selector: "edge.mentions",
    style: {
      "line-color": "#5b637a", // fog-700
      "line-style": "dashed",
      opacity: 0.35,
    },
  },
  {
    selector: "edge.tagged_with",
    style: {
      "line-color": "#2dd4bf",
      "line-style": "dotted",
      opacity: 0.35,
    },
  },
  {
    // Applied by useCytoscape's "highlight connections" interaction.
    selector: ".faded",
    style: { opacity: 0.08 },
  },
];

export type MemoryMapLayoutMode = "network" | "timeline" | "circular";

// concentricLayoutConfig: cytoscape's own built-in "concentric" layout
// (part of @types/cytoscape's official LayoutOptions union, unlike
// fcose — no cast needed at the cy.layout() call site). Rings nodes by
// degree (most-connected toward the center), which directly visualizes
// the same "the hub node is central" story the force layout produces
// implicitly — concentric makes it explicit and, for a small/medium
// graph, noticeably denser/more compact than letting fcose spread nodes
// out by repulsion alone.
export function concentricLayoutConfig(): cytoscape.ConcentricLayoutOptions {
  return {
    name: "concentric",
    animate: true,
    concentric: (node) => node.degree(false),
    levelWidth: () => 2,
    minNodeSpacing: 28,
    equidistant: true,
  };
}

// fcoseLayoutConfig builds the layout options for either mode.
// "timeline" pins conversation nodes to an x position derived from
// their timestamp (oldest-left/newest-right) via fcose's own
// fixedNodeConstraint, leaving entities/topics free to cluster around
// whichever conversations pull them — entities can span the whole
// window, so only conversation nodes get a meaningful time axis.
// randomize:false on every call so re-running the layout after an
// incremental element diff keeps surviving nodes near their previous
// positions instead of a full, jarring re-layout.
//
// FcoseLayoutOptions is a local, deliberately loose type rather than
// cytoscape.LayoutOptions: fcose is a third-party extension (registered
// via cytoscape.use in useCytoscape.ts) whose own options —
// fixedNodeConstraint included — aren't part of @types/cytoscape's
// official LayoutOptions union at all. useCytoscape.ts passes this
// straight to cy.layout() with an explicit cast at that one call site.
export interface FcoseLayoutOptions {
  name: "fcose";
  randomize?: boolean;
  animate?: boolean;
  animationDuration?: number;
  nodeRepulsion?: number;
  idealEdgeLength?: number;
  gravity?: number;
  // {nodeId, position: {x, y}} is fcose's own real shape (cytoscape-fcose's
  // README/source, not @types/cytoscape, since fcose's options aren't
  // covered by the official types at all) — a prior version of this
  // function invented a {nodeId, value: [x, y]} shape instead, which
  // fcose's constraint code reads as `.position.x`/`.position.y` on an
  // object with no `position` field, throwing mid-layout and leaving the
  // canvas blank. Fixed after being caught by real browser testing.
  fixedNodeConstraint?: { nodeId: string; position: { x: number; y: number } }[];
}

export function fcoseLayoutConfig(
  mode: MemoryMapLayoutMode,
  conversationXPositions?: { nodeId: string; x: number }[],
): FcoseLayoutOptions {
  const base: FcoseLayoutOptions = {
    name: "fcose",
    randomize: false,
    animate: true,
    animationDuration: 300,
    // Tighter than fcose's own defaults (repulsion ~4500, ideal edge
    // ~50 scaled by node count) — the initial pass at these values left
    // visible empty space for small/medium graphs; pulled both down for
    // a denser default layout.
    nodeRepulsion: 2200,
    idealEdgeLength: 35,
    gravity: 0.6, // stronger pull toward center than fcose's default (0.25) — keeps loosely-connected nodes from drifting to the edges
  };

  if (mode === "timeline" && conversationXPositions && conversationXPositions.length > 0) {
    return {
      ...base,
      fixedNodeConstraint: conversationXPositions.map((p) => ({ nodeId: p.nodeId, position: { x: p.x, y: 0 } })),
    };
  }
  return base;
}
