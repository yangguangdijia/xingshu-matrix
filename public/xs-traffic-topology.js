(function (root) {
  "use strict";

  const NODE_WIDTH = 224;
  const NODE_HEIGHT = 84;
  const MOBILE_NODE_WIDTH = 304;
  const MOBILE_NODE_HEIGHT = 88;
  const MIN_WIDTH = 1024;
  const ROW_GAP = 168;
  const LAYER_GAP = 392;
  const MOBILE_ROW_GAP = 208;
  const PAD_X = 56;
  const PAD_Y = 46;
  const AUX_GAP = 36;
  const PORT_INSET = 8;
  const PARALLEL_GAP = 32;

  function edgeKey(edge) {
    return `${String(edge.from)}\u0000${String(edge.to)}`;
  }

  function normalizeEdges(edges, allowed, preserveSemantics) {
    const unique = new Map();
    (edges || []).forEach((edge) => {
      if (!edge || edge.from == null || edge.to == null) return;
      const from = String(edge.from), to = String(edge.to);
      if (from === to || !allowed.has(from) || !allowed.has(to)) return;
      const normalized = Object.assign({}, edge, { from, to });
      const key = edgeKey(normalized) + (preserveSemantics ? `\u0000${edgeSemanticKey(normalized)}` : "");
      if (!unique.has(key)) unique.set(key, normalized);
    });
    return [...unique.values()];
  }

  function nodeKeySet(input, edges) {
    if (input instanceof Set) return input;
    if (Array.isArray(input)) return new Set(input.map(String));
    return new Set((edges || []).flatMap((edge) => [String(edge.from), String(edge.to)]));
  }

  function edgeSemantics(edge) {
    const kind = String(edge && edge.kind || "").toLowerCase();
    const source = String(edge && (edge.source || edge.type) || "").toLowerCase();
    const policy = edge && (edge.networkPolicy === true || edge.declared === true) ||
      ["policy", "declared", "networkpolicy", "network-policy"].includes(kind) ||
      ["networkpolicy", "network-policy"].includes(source);
    if (policy) return {
      kind: "policy", solid: false, dashed: true, secondary: true,
      strokeStyle: "dashed", className: "declared secondary"
    };
    const measured = edge && (edge.measured === true || edge.observed === true) ||
      ["trace", "observed", "measured"].includes(kind) ||
      ["trace", "tracing"].includes(source);
    return {
      kind: measured ? "observed" : "primary",
      solid: true,
      dashed: false,
      secondary: false,
      strokeStyle: "solid",
      className: measured ? "observed" : "primary"
    };
  }

  function edgeSemanticKey(edge) {
    return edgeSemantics(edge).kind;
  }

  function stableEdgeValue(value) {
    if (value == null) return "";
    if (typeof value === "string" || typeof value === "number" || typeof value === "boolean") {
      return String(value);
    }
    if (Array.isArray(value)) return `[${value.map(stableEdgeValue).join(",")}]`;
    if (typeof value === "object") {
      return `{${Object.keys(value).sort().map((key) => `${key}:${stableEdgeValue(value[key])}`).join(",")}}`;
    }
    return String(value);
  }

  function edgeStableKey(edge) {
    const priority = { primary: "0", observed: "1", policy: "2" };
    return [
      String(edge && edge.from),
      String(edge && edge.to),
      priority[edgeSemanticKey(edge)] || "3",
      stableEdgeValue(edge && edge.id),
      stableEdgeValue(edge && edge.key),
      stableEdgeValue(edge && edge.source),
      stableEdgeValue(edge && edge.label),
      stableEdgeValue(edge && edge.path)
    ].join("\u0000");
  }

  function edgeLane(edge, index, total) {
    const count = Number.isFinite(Number(total)) ? Math.max(1, Math.floor(Number(total))) : 1;
    const position = Number.isFinite(Number(index)) ? Math.max(0, Math.min(count - 1, Math.floor(Number(index)))) : 0;
    if (count % 2) return position === 0 ? 0 :
      (position % 2 ? -1 : 1) * Math.ceil(position / 2) * PARALLEL_GAP;
    return (position % 2 ? 1 : -1) * (Math.floor(position / 2) + 0.5) * PARALLEL_GAP;
  }

  function parallelOffsets(edges) {
    const result = new Array((edges || []).length).fill(0);
    const groups = new Map();
    (edges || []).forEach((edge, index) => {
      if (!edge || edge.from == null || edge.to == null) return;
      const key = edgeKey(edge);
      const group = groups.get(key) || [];
      group.push({ edge, index });
      groups.set(key, group);
    });
    groups.forEach((group) => {
      group.sort((left, right) => edgeStableKey(left.edge).localeCompare(edgeStableKey(right.edge)) || left.index - right.index);
      group.forEach((entry, index) => {
        result[entry.index] = edgeLane(entry.edge, index, group.length);
      });
    });
    return result;
  }

  function composeEdges(input) {
    input = input || {};
    const entryEdges = (input.entryEdges || []).map((edge) =>
      Object.assign({}, edge, { kind: edge.kind || "primary" }));
    const endpointEdges = (input.serviceEndpoints || []).map((edge) => {
      if (!edge || edge.from == null || edge.to == null) return null;
      return Object.assign({}, edge, { kind: "primary", source: edge.source || "service-endpoint" });
    }).filter(Boolean);
    const traceEdges = (input.traceEdges || []).map((edge) =>
      Object.assign({}, edge, { kind: "observed", measured: true }));
    const policyEdges = (input.policyEdges || []).map((edge) =>
      Object.assign({}, edge, { kind: "policy", declared: true }));
    const primaryInput = entryEdges.concat(endpointEdges);
    const overlayInput = traceEdges.concat(policyEdges);
    const primaryEdges = normalizeEdges(primaryInput, nodeKeySet(input.nodeKeys, primaryInput));
    const overlays = normalizeEdges(overlayInput, nodeKeySet(input.nodeKeys, overlayInput), true);
    return { primaryEdges, traceEdges: overlays.filter((edge) => edgeSemantics(edge).kind === "observed"),
      policyEdges: overlays.filter((edge) => edgeSemantics(edge).kind === "policy"), edges: primaryEdges.concat(overlays) };
  }

  function layout(nodes, edges, options) {
    options = options || {};
    const vertical = options.direction === "vertical";
    const nodeWidth = vertical ? MOBILE_NODE_WIDTH : NODE_WIDTH;
    const nodeHeight = vertical ? MOBILE_NODE_HEIGHT : NODE_HEIGHT;
    const unique = new Map();
    (nodes || []).forEach((node) => {
      if (node && node.key != null && !unique.has(String(node.key))) unique.set(String(node.key), node);
    });
    const list = [...unique.values()];
    const ids = list.map((node) => String(node.key));
    const allowed = new Set(ids);
    const allEdges = normalizeEdges(edges, allowed);
    const explicitPrimary = Array.isArray(options.primaryEdges);
    const rankEdges = normalizeEdges(explicitPrimary ? options.primaryEdges : allEdges, allowed);
    const mainIds = new Set();
    if (Array.isArray(options.primaryNodeKeys)) {
      options.primaryNodeKeys.forEach((id) => { if (allowed.has(String(id))) mainIds.add(String(id)); });
    }
    rankEdges.forEach((edge) => { mainIds.add(edge.from); mainIds.add(edge.to); });
    // Without an explicit primary graph, only nodes participating in an edge
    // belong to the main flow. Isolated nodes remain auxiliary and are laid
    // out in the lower grid instead of being stretched into one vertical lane.

    const label = new Map(list.map((node) => [String(node.key), String(node.label || node.key)]));
    const laneByKey = options.laneByKey || {};
    const lane = (id) => laneByKey[id] != null ? String(laneByKey[id]) :
      String(list.find((node) => String(node.key) === id)?.lane || "main");
    const compareLabel = (a, b) => {
      const laneDelta = lane(a).localeCompare(lane(b));
      if (laneDelta) return laneDelta;
      const left = label.get(a), right = label.get(b);
      return left < right ? -1 : left > right ? 1 : a < b ? -1 : a > b ? 1 : 0;
    };
    const mainList = ids.filter((id) => mainIds.has(id)).sort(compareLabel);
    const mainSet = new Set(mainList);
    const outgoing = new Map(mainList.map((id) => [id, []]));
    const incoming = new Map(mainList.map((id) => [id, []]));
    rankEdges.forEach((edge) => {
      if (!mainSet.has(edge.from) || !mainSet.has(edge.to)) return;
      outgoing.get(edge.from).push(edge.to);
      incoming.get(edge.to).push(edge.from);
    });
    mainList.forEach((id) => {
      outgoing.get(id).sort(compareLabel);
      incoming.get(id).sort(compareLabel);
    });

    // Collapse cycles before ranking so feedback links cannot reverse the main flow.
    const visited = new Set(), finished = [];
    mainList.forEach((rootId) => {
      if (visited.has(rootId)) return;
      visited.add(rootId);
      const frames = [{ id: rootId, next: 0 }];
      while (frames.length) {
        const frame = frames[frames.length - 1], children = outgoing.get(frame.id);
        if (frame.next < children.length) {
          const child = children[frame.next++];
          if (!visited.has(child)) {
            visited.add(child);
            frames.push({ id: child, next: 0 });
          }
        } else {
          finished.push(frame.id);
          frames.pop();
        }
      }
    });
    visited.clear();
    const components = [];
    for (let i = finished.length - 1; i >= 0; i--) {
      const rootId = finished[i];
      if (visited.has(rootId)) continue;
      const component = [], pending = [rootId];
      visited.add(rootId);
      while (pending.length) {
        const id = pending.pop();
        component.push(id);
        incoming.get(id).forEach((parent) => {
          if (!visited.has(parent)) {
            visited.add(parent);
            pending.push(parent);
          }
        });
      }
      component.sort(compareLabel);
      components.push(component);
    }
    components.sort((a, b) => compareLabel(a[0], b[0]));

    const componentOf = new Map();
    components.forEach((component, index) => component.forEach((id) => componentOf.set(id, index)));
    const componentOutgoing = components.map(() => new Set());
    const componentIncoming = components.map(() => new Set());
    rankEdges.forEach((edge) => {
      if (!mainSet.has(edge.from) || !mainSet.has(edge.to)) return;
      const from = componentOf.get(edge.from), to = componentOf.get(edge.to);
      if (from === to) return;
      componentOutgoing[from].add(to);
      componentIncoming[to].add(from);
    });
    const componentRank = components.map(() => 0);
    const indegree = componentIncoming.map((parents) => parents.size);
    const componentLabel = components.map((component) => label.get(component[0]));
    const compareComponent = (a, b) => componentLabel[a] < componentLabel[b] ? -1 :
      componentLabel[a] > componentLabel[b] ? 1 : a - b;
    const queue = components.map((_, index) => index).filter((index) => indegree[index] === 0).sort(compareComponent);
    for (let cursor = 0; cursor < queue.length; cursor++) {
      const from = queue[cursor];
      [...componentOutgoing[from]].sort(compareComponent).forEach((to) => {
        componentRank[to] = Math.max(componentRank[to], componentRank[from] + 1);
        indegree[to]--;
        if (indegree[to] === 0) queue.push(to);
      });
    }

    const ranks = new Map();
    components.forEach((component, index) => component.forEach((id) => ranks.set(id, componentRank[index])));
    const maxRank = mainList.length ? Math.max(...mainList.map((id) => ranks.get(id))) : -1;
    const layers = Array.from({ length: maxRank + 1 }, () => []);
    mainList.forEach((id) => layers[ranks.get(id)].push(id));
    layers.forEach((layer) => layer.sort(compareLabel));
    for (let sweep = 0; sweep < 4; sweep++) {
      for (let depth = 1; depth < layers.length; depth++) {
        const order = new Map(layers[depth - 1].map((id, index) => [id, index]));
        layers[depth].sort((a, b) => {
          const score = (id) => {
            const parents = incoming.get(id).filter((parent) => ranks.get(parent) === depth - 1 && order.has(parent));
            return parents.length ? parents.reduce((sum, parent) => sum + order.get(parent), 0) / parents.length : Infinity;
          };
          const delta = score(a) - score(b);
          return Number.isFinite(delta) && delta !== 0 ? delta : compareLabel(a, b);
        });
      }
      for (let depth = layers.length - 2; depth >= 0; depth--) {
        const order = new Map(layers[depth + 1].map((id, index) => [id, index]));
        layers[depth].sort((a, b) => {
          const score = (id) => {
            const children = outgoing.get(id).filter((child) => ranks.get(child) === depth + 1 && order.has(child));
            return children.length ? children.reduce((sum, child) => sum + order.get(child), 0) / children.length : Infinity;
          };
          const delta = score(a) - score(b);
          return Number.isFinite(delta) && delta !== 0 ? delta : compareLabel(a, b);
        });
      }
    }

    const auxIds = ids.filter((id) => !mainSet.has(id)).sort(compareLabel);
    const attachments = new Map();
    auxIds.forEach((id) => attachments.set(id, new Set()));
    allEdges.forEach((edge) => {
      if (!mainSet.has(edge.from) && mainSet.has(edge.to)) attachments.get(edge.from)?.add(edge.to);
      if (mainSet.has(edge.from) && !mainSet.has(edge.to)) attachments.get(edge.to)?.add(edge.from);
    });
    const auxGroups = new Map();
    auxIds.forEach((id) => {
      const anchors = [...(attachments.get(id) || [])].sort((a, b) => ranks.get(a) - ranks.get(b) || compareLabel(a, b));
      const anchor = anchors[0] || "";
      const group = auxGroups.get(anchor) || [];
      group.push(id);
      auxGroups.set(anchor, group);
    });
    auxGroups.forEach((group) => group.sort(compareLabel));

    const positions = {};
    let width, height;
    if (vertical) {
      width = 360;
      const rowGap = MOBILE_ROW_GAP;
      let y = PAD_Y + nodeHeight / 2;
      layers.forEach((layer) => {
        layer.forEach((id, index) => { positions[id] = { x: width / 2, y: y + index * rowGap }; });
        y += Math.max(1, layer.length) * rowGap + 18;
      });
      if (auxIds.length) {
        y += AUX_GAP;
        auxIds.forEach((id) => {
          positions[id] = { x: width / 2, y };
          y += rowGap;
        });
      }
      height = Math.max(190, y - (auxIds.length ? rowGap : 0) + PAD_Y + nodeHeight / 2 + 32);
    } else {
      const layerGap = LAYER_GAP;
      const contentWidth = PAD_X * 2 + nodeWidth + Math.max(0, maxRank) * layerGap;
      width = Math.max(MIN_WIDTH, contentWidth);
      const graphHeight = layers.length ? Math.max(168, PAD_Y * 2 + nodeHeight + (Math.max(1, ...layers.map((layer) => layer.length)) - 1) * ROW_GAP) : 0;
      layers.forEach((layer, depth) => {
        const startY = graphHeight / 2 - ((layer.length - 1) * ROW_GAP) / 2;
        layer.forEach((id, index) => {
          positions[id] = { x: PAD_X + nodeWidth / 2 + depth * layerGap, y: startY + index * ROW_GAP };
        });
      });
      let auxRow = 0;
      auxGroups.forEach((group, anchor) => {
        if (!anchor) return;
        const anchorX = positions[anchor] ? positions[anchor].x : width / 2;
        group.forEach((id) => {
          positions[id] = { x: anchorX, y: graphHeight + AUX_GAP + auxRow * ROW_GAP + nodeHeight / 2 };
          auxRow++;
        });
        auxRow++;
      });
      const unattached = auxGroups.get("") || [];
      if (unattached.length) {
        const gap = 32;
        const columns = Math.max(1, Math.min(unattached.length, Math.floor((width - PAD_X * 2 + gap) / (nodeWidth + gap))));
        const rows = Math.ceil(unattached.length / columns);
        const startY = graphHeight + AUX_GAP + auxRow * ROW_GAP + nodeHeight / 2;
        unattached.forEach((id, index) => {
          const row = Math.floor(index / columns);
          const inRow = Math.min(columns, unattached.length - row * columns);
          const rowWidth = inRow * nodeWidth + Math.max(0, inRow - 1) * gap;
          const startX = (width - rowWidth) / 2 + nodeWidth / 2;
          positions[id] = {
            x: startX + (index % columns) * (nodeWidth + gap),
            y: startY + row * ROW_GAP
          };
        });
        auxRow += (rows - 1) + 1;
      }
      height = auxRow
        ? graphHeight + AUX_GAP + (auxRow - 1) * ROW_GAP + nodeHeight + PAD_Y
        : Math.max(176, graphHeight);
    }
    auxIds.forEach((id) => ranks.set(id, -1));
    return {
      width, height, nodeWidth, nodeHeight, positions,
      edges: allEdges.map(({ from, to }) => ({ from, to })),
      direction: vertical ? "vertical" : "horizontal",
      ranks: Object.fromEntries(ranks),
      lanes: Object.fromEntries(ids.map((id) => [id, lane(id)])),
      primaryKeys: mainList,
      auxiliaryKeys: auxIds
    };
  }

  function midpoint(start, control1, control2, end) {
    return {
      x: (start.x + 3 * control1.x + 3 * control2.x + end.x) / 8,
      y: (start.y + 3 * control1.y + 3 * control2.y + end.y) / 8
    };
  }

  function midpointTangent(start, control1, control2, end) {
    return {
      x: 0.75 * (control1.x - start.x + end.x - control2.x) + 1.5 * (control2.x - control1.x),
      y: 0.75 * (control1.y - start.y + end.y - control2.y) + 1.5 * (control2.y - control1.y)
    };
  }

  function portOffset(index, count, span) {
    const total = Number.isFinite(Number(count)) ? Math.max(1, Math.floor(Number(count))) : 1;
    if (total === 1) return 0;
    const position = Number.isFinite(Number(index)) ? Math.max(0, Math.min(total - 1, Math.floor(Number(index)))) : 0;
    const available = Math.max(0, span - PORT_INSET * 2);
    const step = available / (total - 1);
    return (position - (total - 1) / 2) * step;
  }

  function boundaryOffset(value, span) {
    const limit = Math.max(0, span / 2 - PORT_INSET);
    return Math.max(-limit, Math.min(limit, value));
  }

  function geometryOptions(options) {
    options = options || {};
    const hasPort = options.portIndex != null || options.portCount != null ||
      options.fromPortIndex != null || options.toPortIndex != null ||
      options.fromPortCount != null || options.toPortCount != null;
    const portIndex = options.portIndex == null ? 0 : Number(options.portIndex);
    const portCount = options.portCount == null ? 1 : Number(options.portCount);
    return {
      offset: Number.isFinite(Number(options.offset)) ? Number(options.offset) : 0,
      fromPortIndex: options.fromPortIndex == null ? portIndex : Number(options.fromPortIndex),
      toPortIndex: options.toPortIndex == null ? portIndex : Number(options.toPortIndex),
      fromPortCount: options.fromPortCount == null ? portCount : Number(options.fromPortCount),
      toPortCount: options.toPortCount == null ? portCount : Number(options.toPortCount),
      hasPort
    };
  }

  function edgeGeometry(from, to, nodeWidth, nodeHeight, options) {
    if (!from || !to) return { path: "", midpoint: null, vertical: false };
    nodeWidth = Number(nodeWidth) || NODE_WIDTH;
    nodeHeight = Number(nodeHeight) || NODE_HEIGHT;
    const geometry = geometryOptions(options);
    const dx = to.x - from.x, dy = to.y - from.y;
    if (Math.abs(dx) >= nodeWidth && Math.abs(dx) >= Math.abs(dy)) {
      const direction = dx >= 0 ? 1 : -1;
      const fromOffset = (geometry.hasPort ? portOffset(geometry.fromPortIndex, geometry.fromPortCount, nodeHeight) : 0) + geometry.offset;
      const toOffset = (geometry.hasPort ? portOffset(geometry.toPortIndex, geometry.toPortCount, nodeHeight) : 0) + geometry.offset;
      const fromBoundary = boundaryOffset(fromOffset, nodeHeight);
      const toBoundary = boundaryOffset(toOffset, nodeHeight);
      const start = {
        x: from.x + direction * nodeWidth / 2,
        y: from.y + fromBoundary
      };
      const end = {
        x: to.x - direction * nodeWidth / 2,
        y: to.y + toBoundary
      };
      const bend = Math.max(30, Math.abs(end.x - start.x) * 0.42);
      const control1 = { x: start.x + direction * bend, y: start.y + fromOffset - fromBoundary };
      const control2 = { x: end.x - direction * bend, y: end.y + toOffset - toBoundary };
      return {
        path: `M ${start.x} ${start.y} C ${control1.x} ${control1.y}, ${control2.x} ${control2.y}, ${end.x} ${end.y}`,
        midpoint: midpoint(start, control1, control2, end),
        tangent: midpointTangent(start, control1, control2, end), vertical: false
      };
    }
    const direction = dy >= 0 ? 1 : -1;
    const fromOffset = (geometry.hasPort ? portOffset(geometry.fromPortIndex, geometry.fromPortCount, nodeWidth) : 0) + geometry.offset;
    const toOffset = (geometry.hasPort ? portOffset(geometry.toPortIndex, geometry.toPortCount, nodeWidth) : 0) + geometry.offset;
    const fromBoundary = boundaryOffset(fromOffset, nodeWidth);
    const toBoundary = boundaryOffset(toOffset, nodeWidth);
    const start = {
      x: from.x + fromBoundary,
      y: from.y + direction * nodeHeight / 2
    };
    const end = {
      x: to.x + toBoundary,
      y: to.y - direction * nodeHeight / 2
    };
    const bend = Math.max(28, Math.abs(end.y - start.y) * 0.36);
    const diagonal = (to.x - from.x) * 0.16;
    const control1 = { x: start.x + diagonal + fromOffset - fromBoundary, y: start.y + direction * bend };
    const control2 = { x: end.x - diagonal + toOffset - toBoundary, y: end.y - direction * bend };
    return {
      path: `M ${start.x} ${start.y} C ${control1.x} ${control1.y}, ${control2.x} ${control2.y}, ${end.x} ${end.y}`,
      midpoint: midpoint(start, control1, control2, end),
      tangent: midpointTangent(start, control1, control2, end), vertical: true
    };
  }

  function edgePath(from, to, nodeWidth, nodeHeight, options) {
    return edgeGeometry(from, to, nodeWidth, nodeHeight, options).path;
  }

  function edgeLabelPoint(from, to, nodeWidth, nodeHeight, viewWidth, options) {
    options = options || {};
    const geometry = edgeGeometry(from, to, nodeWidth, nodeHeight, options);
    if (!geometry.midpoint) return null;
    const labelHeight = Number(options.labelHeight);
    const sizedLabel = Number.isFinite(labelHeight) && labelHeight > 0;
    const clearance = Math.max(18, Number(options.labelClearance) || 18);
    const tangent = geometry.tangent;
    const length = Math.hypot(tangent.x, tangent.y) || 1;
    const normal = { x: -tangent.y / length, y: tangent.x / length };
    if (geometry.vertical) {
      const width = Number(viewWidth) || 0;
      const compact = width <= (Number(nodeWidth) || NODE_WIDTH) + 100;
      const side = geometry.midpoint.x < width / 2 ? 1 : -1;
      const labelGap = compact ? 68 :
        Math.max((Number(nodeWidth) || NODE_WIDTH) / 2 + 12, 68, sizedLabel ? labelHeight / 2 + clearance : 0);
      if (sizedLabel) {
        const sign = normal.x * side >= 0 ? 1 : -1;
        return { x: geometry.midpoint.x + sign * normal.x * labelGap,
          y: geometry.midpoint.y + sign * normal.y * labelGap };
      }
      return { x: geometry.midpoint.x + side * labelGap, y: geometry.midpoint.y };
    }
    const labelGap = sizedLabel
      ? labelHeight / 2 + clearance
      : (Number(nodeHeight) || NODE_HEIGHT) / 2 + 12;
    if (sizedLabel) {
      const sign = normal.y <= 0 ? 1 : -1;
      return { x: geometry.midpoint.x + sign * normal.x * labelGap,
        y: geometry.midpoint.y + sign * normal.y * labelGap };
    }
    return { x: geometry.midpoint.x, y: geometry.midpoint.y - labelGap };
  }

  root.XSTrafficTopology = Object.freeze({
    layout, edgePath, edgeLabelPoint, edgeSemantics, composeEdges, edgeLane, parallelOffsets,
    nodeWidth: NODE_WIDTH, nodeHeight: NODE_HEIGHT
  });
})(typeof window === "undefined" ? globalThis : window);
