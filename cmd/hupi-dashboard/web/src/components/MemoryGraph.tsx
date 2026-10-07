import { useEffect, useRef } from "react";
import { ElementDefinition } from "cytoscape";
import { useCytoscape } from "../hooks/useCytoscape";
import { MemoryMapLayoutMode } from "../lib/memoryMapGraph";

interface Props {
  elements: ElementDefinition[];
  layoutMode: MemoryMapLayoutMode;
  conversationXPositions?: { nodeId: string; x: number }[];
  selectedNodeId: string | null;
  onSelectNode: (nodeId: string, nodeType: string) => void;
}

// MemoryGraph is the thin, declarative wrapper around useCytoscape's
// imperative lifecycle — a <div ref> container plus effects that push
// new data/selection state into the live cytoscape instance. Needs an
// explicit height from its parent (MemoryMap.tsx): cytoscape requires a
// concrete pixel height to lay out into, unlike a plain <ul> that
// auto-sizes to its content.
export function MemoryGraph({ elements, layoutMode, conversationXPositions, selectedNodeId, onSelectNode }: Props) {
  const containerRef = useRef<HTMLDivElement>(null);
  const { updateElements, highlightNeighborhood } = useCytoscape(containerRef, onSelectNode);

  useEffect(() => {
    updateElements(elements, layoutMode, conversationXPositions);
    // elements is a new array reference on every fetch; conversationXPositions
    // likewise — both are the actual change signal here, not something to
    // memoize away, since a filter change always means genuinely new data.
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [elements, layoutMode, conversationXPositions, updateElements]);

  useEffect(() => {
    highlightNeighborhood(selectedNodeId || null);
  }, [selectedNodeId, highlightNeighborhood]);

  return <div ref={containerRef} className="h-full w-full" />;
}
