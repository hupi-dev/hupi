import { useCallback, useEffect, useRef } from "react";
import cytoscape from "cytoscape";
// @ts-expect-error — cytoscape-fcose ships no types of its own; @types/cytoscape covers cytoscape core only.
import fcose from "cytoscape-fcose";
import { concentricLayoutConfig, fcoseLayoutConfig, elementIDs, MemoryMapLayoutMode, memoryMapStylesheet } from "../lib/memoryMapGraph";

let fcoseRegistered = false;
function ensureFcoseRegistered() {
  if (fcoseRegistered) return;
  cytoscape.use(fcose);
  fcoseRegistered = true;
}

// useCytoscape owns cytoscape's imperative lifecycle so MemoryGraph.tsx
// can stay a thin, declarative wrapper around it — raw cytoscape core,
// not react-cytoscapejs: an incremental, position-preserving update on
// every filter change (not a full re-render/relayout) needs direct
// control over exactly which elements are added/removed and when
// layout.run() fires, which a generic React-diffing wrapper abstracts
// away.
export function useCytoscape(
  containerRef: React.RefObject<HTMLDivElement>,
  onNodeTap: (nodeId: string, nodeType: string) => void,
) {
  const cyRef = useRef<cytoscape.Core | null>(null);
  const onNodeTapRef = useRef(onNodeTap);
  onNodeTapRef.current = onNodeTap;

  useEffect(() => {
    ensureFcoseRegistered();
    if (!containerRef.current) return;
    const cy = cytoscape({
      container: containerRef.current,
      // CyStyleRule is a local stand-in type (memoryMapGraph.ts's own
      // doc comment) for cytoscape.Stylesheet — @types/cytoscape's own
      // Stylesheet type alias doesn't resolve by name anywhere in this
      // project (named import or qualified cytoscape.Stylesheet access,
      // confirmed both ways), a real quirk of the installed types
      // package, not a shape mismatch. `as any` is the narrowest escape
      // for that one unresolvable name, scoped to this single field.
      // eslint-disable-next-line @typescript-eslint/no-explicit-any
      style: memoryMapStylesheet as any,
      elements: [],
      wheelSensitivity: 0.2,
      // Caps how far cy.fit() (below) is allowed to zoom IN for a small
      // graph — without this, fitting 2-3 nodes to the container zooms
      // in absurdly close. minZoom is the opposite backstop for a very
      // large graph fitting zoomed far out; both just bound fit()'s own
      // auto-scaling, not a fixed zoom level.
      maxZoom: 2.5,
      minZoom: 0.1,
    });
    cy.on("tap", "node", (evt) => {
      const n = evt.target;
      onNodeTapRef.current(n.id(), n.data("type"));
    });
    cy.on("tap", (evt) => {
      if (evt.target === cy) onNodeTapRef.current("", ""); // tapped the background — treated as "deselect" by the caller
    });
    cyRef.current = cy;
    return () => {
      cy.destroy();
      cyRef.current = null;
    };
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, []);

  // updateElements diffs the live instance's element-id set against the
  // new response: fades out and removes anything no longer present,
  // adds anything new, refreshes data on survivors (a node's own
  // mention count/weight can change between fetches even if the node
  // itself persists), then re-runs fcose with randomize:false so
  // surviving nodes keep roughly their existing positions instead of a
  // jarring full relayout on every filter tweak.
  const updateElements = useCallback(
    (elements: cytoscape.ElementDefinition[], layoutMode: MemoryMapLayoutMode, conversationXPositions?: { nodeId: string; x: number }[]) => {
      const cy = cyRef.current;
      if (!cy) return;

      const newIDs = elementIDs(elements);
      const current = cy.elements();
      const toRemove = current.filter((el) => !newIDs.has(el.id()));
      const currentIDs = new Set(current.map((el) => el.id()));
      const toAdd = elements.filter((el) => !currentIDs.has(el.data.id as string));

      for (const el of elements) {
        if (currentIDs.has(el.data.id as string)) {
          cy.getElementById(el.data.id as string).data(el.data);
        }
      }

      if (toRemove.length > 0) {
        toRemove.animate({ style: { opacity: 0 } }, { duration: 150 });
        setTimeout(() => toRemove.remove(), 150);
      }
      if (toAdd.length > 0) {
        cy.add(toAdd);
      }
      // The viewport fit is triggered once the layout actually settles
      // (its own "layoutstop" event), not immediately after .run() —
      // fcose animates asynchronously, so fitting right after .run()
      // would frame the pre-layout (often still-default) positions, not
      // the final ones. This is what makes zoom depend on the current
      // element count rather than a fixed level: fit computes whatever
      // zoom fills the container with all current elements — bigger/
      // zoomed-in for a handful of nodes, zoomed-out for a large graph,
      // capped by maxZoom/minZoom set at construction above.
      //
      // cy.animate({fit: ...}) instead of the plain cy.fit() this
      // started with: fit() is an instant jump, no transition at all —
      // the node positions were already animating smoothly (fcose's own
      // animate:true), but the camera itself snapped straight to the
      // new zoom/pan the moment layout finished, which is what actually
      // read as "sudden" even though the layout itself wasn't.
      const layout =
        layoutMode === "circular"
          ? // concentric is a real, officially-typed cytoscape layout — no
            // cast needed, unlike fcose below.
            cy.layout(concentricLayoutConfig())
          : // fcose's own options (fixedNodeConstraint included) aren't
            // part of @types/cytoscape's official LayoutOptions union —
            // see FcoseLayoutOptions' own doc comment in
            // memoryMapGraph.ts for why this cast is the one place that
            // gap surfaces.
            cy.layout(fcoseLayoutConfig(layoutMode, conversationXPositions) as unknown as cytoscape.LayoutOptions);
      layout.one("layoutstop", () => {
        cy.animate({ fit: { eles: cy.elements(), padding: 40 } }, { duration: 400, easing: "ease-in-out-cubic" });
      });
      layout.run();
    },
    [],
  );

  const highlightNeighborhood = useCallback((nodeId: string | null) => {
    const cy = cyRef.current;
    if (!cy) return;
    cy.elements().removeClass("faded");
    if (!nodeId) return;
    const node = cy.getElementById(nodeId);
    if (node.length === 0) return;
    const neighborhood = node.closedNeighborhood();
    cy.elements().difference(neighborhood).addClass("faded");
  }, []);

  return { updateElements, highlightNeighborhood };
}
