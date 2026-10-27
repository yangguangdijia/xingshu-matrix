(function (root) {
  "use strict";

  const NODE_WIDTH = 224;
  const NODE_HEIGHT = 84;
  const MOBILE_NODE_WIDTH = 304;
  const MOBILE_NODE_HEIGHT = 88;
  const LAYER_GAP = 144;
  const MIN_WIDTH = 1024;
  const ROW_GAP = 124;
  const PAD_X = 56;
  const PAD_Y = 46;

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
    const pairs = [...new Map((edges || [])
      .filter((edge) => edge && allowed.has(String(edge.from)) && allowed.has(String(edge.to)) && String(edge.from) !== String(edge.to))
      .map((edge) => [`${edge.from}\u0000${edge.to}`, { from: String(edge.from), to: String(edge.to) }])).values()];
    const outgoing = new Map(ids.map((id) => [id, []]));
    const incoming = new Map(ids.map((id) => [id, []]));
    pairs.forEach((edge) => {
      outgoing.get(edge.from).push(edge.to);
      incoming.get(edge.to).push(edge.from);
    });
    const label = new Map(list.map((node) => [String(node.key), String(node.label || node.key)]));
    const compareLabel = (a, b) => {
      const left = label.get(a), right = label.get(b);
      return left < right ? -1 : left > right ? 1 : a < b ? -1 : a > b ? 1 : 0;
    };

    // Collapse strongly connected services first so cycles cannot pull an
    // otherwise downstream service back into an earlier flow layer.
    const sortedIds = ids.slice().sort(compareLabel);
    const sortedOutgoing = new Map(ids.map((id) => [id, outgoing.get(id).slice().sort(compareLabel)]));
    const sortedIncoming = new Map(ids.map((id) => [id, incoming.get(id).slice().sort(compareLabel)]));
    const visited = new Set(), finished = [];
    sortedIds.forEach((rootId) => {
      if (visited.has(rootId)) return;
      visited.add(rootId);
      const frames = [{ id: rootId, next: 0 }];
      while (frames.length) {
        const frame = frames[frames.length - 1];
        const children = sortedOutgoing.get(frame.id);
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
        sortedIncoming.get(id).forEach((parent) => {
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
    components.forEach((component, componentIndex) => {
      component.forEach((id) => componentOf.set(id, componentIndex));
    });
    const componentLabel = components.map((component) => label.get(component[0]));
    const componentOutgoing = components.map(() => new Set());
    const componentIncoming = components.map(() => new Set());
    pairs.forEach((edge) => {
      const from = componentOf.get(edge.from), to = componentOf.get(edge.to);
      if (from === to || componentOutgoing[from].has(to)) return;
      componentOutgoing[from].add(to);
      componentIncoming[to].add(from);
    });

    const componentRank = components.map(() => 0);
    const componentIndegree = componentIncoming.map((parents) => parents.size);
    const componentCompare = (a, b) => {
      const left = componentLabel[a], right = componentLabel[b];
      return left < right ? -1 : left > right ? 1 : a - b;
    };
    const queue = components.map((_, componentIndex) => componentIndex)
      .filter((componentIndex) => componentIndegree[componentIndex] === 0)
      .sort(componentCompare);
    for (let cursor = 0; cursor < queue.length; cursor++) {
      const from = queue[cursor];
      [...componentOutgoing[from]].sort(componentCompare).forEach((to) => {
        componentRank[to] = Math.max(componentRank[to], componentRank[from] + 1);
        componentIndegree[to]--;
        if (componentIndegree[to] === 0) {
          queue.push(to);
        }
      });
    }

    const rank = new Map();
    components.forEach((component, componentIndex) => {
      component.forEach((id) => rank.set(id, componentRank[componentIndex]));
    });
    const maxRank = componentRank.reduce((max, value) => Math.max(max, value), 0);
    const layers = Array.from({ length: maxRank + 1 }, () => []);
    ids.forEach((id) => layers[rank.get(id)].push(id));
    layers.forEach((layer) => layer.sort(compareLabel));
    for (let sweep = 0; sweep < 4; sweep++) {
      for (let depth = 1; depth < layers.length; depth++) {
        const previousOrder = new Map(layers[depth - 1].map((id, index) => [id, index]));
        layers[depth].sort((a, b) => {
          const score = (id) => {
            const parents = incoming.get(id).filter((parent) => rank.get(parent) === depth - 1 && previousOrder.has(parent));
            return parents.length ? parents.reduce((sum, parent) => sum + previousOrder.get(parent), 0) / parents.length : Number.POSITIVE_INFINITY;
          };
          const delta = score(a) - score(b);
          return Number.isNaN(delta) || delta === 0 ? compareLabel(a, b) : delta;
        });
      }
      for (let depth = layers.length - 2; depth >= 0; depth--) {
        const nextOrder = new Map(layers[depth + 1].map((id, index) => [id, index]));
        layers[depth].sort((a, b) => {
          const score = (id) => {
            const children = outgoing.get(id).filter((child) => rank.get(child) === depth + 1 && nextOrder.has(child));
            return children.length ? children.reduce((sum, child) => sum + nextOrder.get(child), 0) / children.length : Number.POSITIVE_INFINITY;
          };
          const delta = score(a) - score(b);
          return Number.isNaN(delta) || delta === 0 ? compareLabel(a, b) : delta;
        });
      }
    }

    if (vertical) {
      const width = 360;
      const positions = {};
      const rowGap = 172;
      let y = PAD_Y + nodeHeight / 2;
      layers.forEach((layer) => {
        layer.forEach((id, index) => {
          positions[id] = { x: width / 2, y: y + index * rowGap };
        });
        y += Math.max(1, layer.length) * rowGap + 18;
      });
      return {
        width,
        height: Math.max(190, y - rowGap + PAD_Y + nodeHeight / 2 + 32),
        nodeWidth,
        nodeHeight,
        positions,
        edges: pairs,
        direction: "vertical"
      };
    }

    const rows = layers.reduce((max, layer) => Math.max(max, layer.length), 1);
    const layerGap = maxRank ? Math.max(LAYER_GAP, (MIN_WIDTH - PAD_X * 2 - nodeWidth) / maxRank) : LAYER_GAP;
    const contentWidth = PAD_X * 2 + nodeWidth + maxRank * layerGap;
    const width = Math.max(MIN_WIDTH, contentWidth);
    const offsetX = (width - contentWidth) / 2;
    const height = Math.max(168, PAD_Y * 2 + nodeHeight + (rows - 1) * ROW_GAP);
    const positions = {};
    layers.forEach((layer, depth) => {
      const startY = height / 2 - ((layer.length - 1) * ROW_GAP) / 2;
      layer.forEach((id, index) => {
        positions[id] = { x: PAD_X + offsetX + nodeWidth / 2 + depth * layerGap, y: startY + index * ROW_GAP };
      });
    });
    return { width, height, nodeWidth, nodeHeight, positions, edges: pairs, direction: "horizontal" };
  }

  function midpoint(start, control1, control2, end) {
    return {
      x: (start.x + 3 * control1.x + 3 * control2.x + end.x) / 8,
      y: (start.y + 3 * control1.y + 3 * control2.y + end.y) / 8
    };
  }

  function edgeGeometry(from, to, nodeWidth, nodeHeight) {
    if (!from || !to) return { path: "", midpoint: null, vertical: false };
    nodeWidth = Number(nodeWidth) || NODE_WIDTH;
    nodeHeight = Number(nodeHeight) || NODE_HEIGHT;
    const dx = to.x - from.x, dy = to.y - from.y;
    if (Math.abs(dx) >= nodeWidth && Math.abs(dx) >= Math.abs(dy)) {
      const direction = dx >= 0 ? 1 : -1;
      const start = { x: from.x + direction * nodeWidth / 2, y: from.y };
      const end = { x: to.x - direction * nodeWidth / 2, y: to.y };
      const bend = Math.max(30, Math.abs(end.x - start.x) * 0.42);
      const control1 = { x: start.x + direction * bend, y: start.y };
      const control2 = { x: end.x - direction * bend, y: end.y };
      return {
        path: `M ${start.x} ${start.y} C ${control1.x} ${control1.y}, ${control2.x} ${control2.y}, ${end.x} ${end.y}`,
        midpoint: midpoint(start, control1, control2, end),
        vertical: false
      };
    }
    const direction = dy >= 0 ? 1 : -1;
    const start = { x: from.x, y: from.y + direction * nodeHeight / 2 };
    const end = { x: to.x, y: to.y - direction * nodeHeight / 2 };
    const bend = Math.max(28, Math.abs(end.y - start.y) * 0.36);
    const diagonal = (to.x - from.x) * 0.16;
    const control1 = { x: start.x + diagonal, y: start.y + direction * bend };
    const control2 = { x: end.x - diagonal, y: end.y - direction * bend };
    return {
      path: `M ${start.x} ${start.y} C ${control1.x} ${control1.y}, ${control2.x} ${control2.y}, ${end.x} ${end.y}`,
      midpoint: midpoint(start, control1, control2, end),
      vertical: true
    };
  }

  function edgePath(from, to, nodeWidth, nodeHeight) {
    return edgeGeometry(from, to, nodeWidth, nodeHeight).path;
  }

  function edgeLabelPoint(from, to, nodeWidth, nodeHeight, viewWidth) {
    const geometry = edgeGeometry(from, to, nodeWidth, nodeHeight);
    if (!geometry.midpoint) return null;
    if (geometry.vertical) {
      const width = Number(viewWidth) || 0;
      const compact = width <= (Number(nodeWidth) || NODE_WIDTH) + 100;
      const side = geometry.midpoint.x < width / 2 ? 1 : -1;
      const labelGap = compact ? 68 : Math.max((Number(nodeWidth) || NODE_WIDTH) / 2 + 12, 68);
      return { x: geometry.midpoint.x + side * labelGap, y: geometry.midpoint.y };
    }
    return { x: geometry.midpoint.x, y: geometry.midpoint.y - ((Number(nodeHeight) || NODE_HEIGHT) / 2 + 12) };
  }

  root.XSTrafficTopology = Object.freeze({ layout, edgePath, edgeLabelPoint, nodeWidth: NODE_WIDTH, nodeHeight: NODE_HEIGHT });
})(typeof window === "undefined" ? globalThis : window);
