import Graph from 'https://esm.sh/graphology@0.25.4';
import Sigma from 'https://esm.sh/sigma@3.0.2';
import forceAtlas2 from 'https://esm.sh/graphology-layout-forceatlas2@0.10.1';
import noverlap from 'https://esm.sh/graphology-layout-noverlap@0.4.2';

// -- Configuration --
const KIND_STYLES = {
  file:      { color: '#607D8B', label: 'File' },
  package:   { color: '#bb9af7', label: 'Package' },
  function:  { color: '#7aa2f7', label: 'Function' },
  method:    { color: '#7dcfff', label: 'Method' },
  type:      { color: '#9ece6a', label: 'Type' },
  interface: { color: '#73daca', label: 'Interface' },
  variable:  { color: '#ff9e64', label: 'Variable' },
  import:    { color: '#795548', label: 'Import' },
};

const EDGE_COLORS = {
  calls:        '#7aa2f7',
  imports:      '#565f89',
  defines:      '#414868',
  implements:   '#9ece6a',
  extends:      '#bb9af7',
  references:   '#3b4261',
  member_of:    '#3b4261',
  instantiates: '#e0af68',
};

const MIN_NODE_SIZE = 3;
const MAX_NODE_SIZE = 20;

let sigmaGraph;   // graphology instance
let renderer;     // sigma renderer
let fa2Handle;    // layout animation handle
let activeKinds = new Set(Object.keys(KIND_STYLES));
let searchQuery = '';
let hideTests = false;
let highlightedNode = null;
let highlightedNeighbors = null;

function isTestNode(attrs) {
  const fp = attrs.file_path || '';
  return fp.includes('_test.go') ||
    fp.includes('_test.ts') || fp.includes('_test.tsx') ||
    fp.includes('.test.') || fp.includes('.spec.') ||
    fp.includes('__tests__/') || fp.includes('/test/');
}

// -- Graph helpers --

function computeNodeSizes(graph) {
  let maxDeg = 1;
  graph.forEachNode((id) => {
    const d = graph.degree(id);
    if (d > maxDeg) maxDeg = d;
  });

  graph.forEachNode((id) => {
    const d = graph.degree(id);
    const t = Math.log(1 + d) / Math.log(1 + maxDeg);
    const size = MIN_NODE_SIZE + t * (MAX_NODE_SIZE - MIN_NODE_SIZE);
    graph.setNodeAttribute(id, 'size', size);
  });
}

// -- Build graphology graph from API data --

function populateGraph(graph, nodes, edges) {
  const nodeIds = new Set();

  for (const n of nodes) {
    nodeIds.add(n.id);
    if (!graph.hasNode(n.id)) {
      graph.addNode(n.id, {
        label: n.name,
        kind: n.kind,
        file_path: n.file_path,
        start_line: n.start_line,
        language: n.language,
        color: (KIND_STYLES[n.kind] || {}).color || '#565f89',
        x: Math.random() * 100,
        y: Math.random() * 100,
        size: MIN_NODE_SIZE,
      });
    }
  }

  for (const e of edges) {
    if (nodeIds.has(e.from) && nodeIds.has(e.to)) {
      const key = e.from + '|' + e.to + '|' + e.kind;
      if (!graph.hasEdge(key)) {
        try {
          graph.addEdgeWithKey(key, e.from, e.to, {
            kind: e.kind,
            color: EDGE_COLORS[e.kind] || '#3b4261',
            size: 0.5,
          });
        } catch (_) {
          // skip duplicate edges
        }
      }
    }
  }

  computeNodeSizes(graph);
}

// -- ForceAtlas2 layout --

function prePositionByDegree(graph) {
  // 1. Find hubs (top degree nodes) and spread them evenly in a ring
  const degrees = [];
  graph.forEachNode((id) => {
    degrees.push({ id, deg: graph.degree(id) });
  });
  degrees.sort((a, b) => b.deg - a.deg);

  const maxDeg = degrees[0]?.deg || 1;
  // Top ~5% or at least top 10 nodes are "hubs"
  const hubCount = Math.max(10, Math.floor(degrees.length * 0.05));
  const hubs = new Set(degrees.slice(0, hubCount).map(d => d.id));

  // Place hubs evenly spaced in a wide ring so they don't clump
  const hubRadius = 150;
  let hubIdx = 0;
  const hubPositions = new Map();
  for (const { id } of degrees.slice(0, hubCount)) {
    const angle = (hubIdx / hubCount) * 2 * Math.PI;
    const x = Math.cos(angle) * hubRadius;
    const y = Math.sin(angle) * hubRadius;
    graph.setNodeAttribute(id, 'x', x);
    graph.setNodeAttribute(id, 'y', y);
    hubPositions.set(id, { x, y });
    hubIdx++;
  }

  // 2. Place non-hub nodes near their highest-degree neighbor (cluster seeding)
  graph.forEachNode((id) => {
    if (hubs.has(id)) return;

    // Find the neighbor with the highest degree
    let bestNeighbor = null;
    let bestDeg = -1;
    graph.forEachNeighbor(id, (neighbor) => {
      const nd = graph.degree(neighbor);
      if (nd > bestDeg) {
        bestDeg = nd;
        bestNeighbor = neighbor;
      }
    });

    let cx, cy;
    if (bestNeighbor && hubPositions.has(bestNeighbor)) {
      // Place near the hub neighbor
      const hp = hubPositions.get(bestNeighbor);
      cx = hp.x;
      cy = hp.y;
    } else if (bestNeighbor) {
      // Place near the best neighbor (which was already placed near a hub)
      cx = graph.getNodeAttribute(bestNeighbor, 'x') || 0;
      cy = graph.getNodeAttribute(bestNeighbor, 'y') || 0;
    } else {
      // Orphan: place far out
      const angle = Math.random() * 2 * Math.PI;
      cx = Math.cos(angle) * 300;
      cy = Math.sin(angle) * 300;
    }

    // Offset from cluster center: low-degree nodes further out
    const d = graph.degree(id);
    const spread = 40 + (1 - d / maxDeg) * 80;
    const angle = Math.random() * 2 * Math.PI;
    graph.setNodeAttribute(id, 'x', cx + Math.cos(angle) * spread * Math.random());
    graph.setNodeAttribute(id, 'y', cy + Math.sin(angle) * spread * Math.random());
  });
}

function startLayout() {
  stopLayout();

  prePositionByDegree(sigmaGraph);

  const settings = forceAtlas2.inferSettings(sigmaGraph);
  // No strongGravityMode — it creates uniform radial pull (circle).
  // Instead rely on linLogMode + low gravity to let clusters form organically.
  settings.strongGravityMode = false;
  settings.gravity = 0.0005;
  settings.scalingRatio = 20;
  settings.barnesHutOptimize = sigmaGraph.order > 500;
  settings.barnesHutTheta = 0.5;

  settings.adjustSizes = true;
  settings.slowDown = 1;

  settings.outboundAttractionDistribution = true;
  settings.edgeWeightInfluence = 15;
  // linLogMode: tighter clusters around hubs, more separation between clusters
  settings.linLogMode = true;

  const state = { running: true, settings };

  const iterate = () => {
    if (!state.running) return;
    forceAtlas2.assign(sigmaGraph, { iterations: 5, settings: state.settings });
    state.frame = requestAnimationFrame(iterate);
  };
  state.frame = requestAnimationFrame(iterate);
  // After FA2 converges, run noverlap to remove remaining overlaps
  state.timer = setTimeout(() => {
    stopLayout();
    noverlap.assign(sigmaGraph, {
      maxIterations: 10000,
      ratio: 2,
      margin: 10,
      speed: 5,
    });
    renderer.refresh();
  }, 60000);

  fa2Handle = state;
}

function stopLayout() {
  if (fa2Handle) {
    fa2Handle.running = false;
    if (fa2Handle.frame) cancelAnimationFrame(fa2Handle.frame);
    if (fa2Handle.timer) clearTimeout(fa2Handle.timer);
    fa2Handle = null;
  }
}

// -- Sigma renderer --

function initRenderer() {
  const container = document.getElementById('sigma-container');

  sigmaGraph = new Graph({ multi: true, type: 'directed' });

  renderer = new Sigma(sigmaGraph, container, {
    allowInvalidContainer: true,
    renderEdgeLabels: false,
    enableEdgeEvents: false,
    defaultEdgeType: 'arrow',
    labelFont: 'SF Mono, Fira Code, JetBrains Mono, monospace',
    labelSize: 11,
    labelColor: { color: '#c0caf5' },
    labelRenderedSizeThreshold: 6,
    stagePadding: 40,
    nodeReducer: reduceNode,
    edgeReducer: reduceEdge,
  });

  renderer.on('clickNode', ({ node }) => {
    if (highlightedNode === node) {
      highlightedNode = null;
      highlightedNeighbors = null;
    } else {
      highlightedNode = node;
      highlightedNeighbors = new Set(sigmaGraph.neighbors(node));
    }
    showNodeDetail(node);
    renderer.refresh();
  });

  renderer.on('clickStage', () => {
    highlightedNode = null;
    highlightedNeighbors = null;
    document.getElementById('node-detail').textContent = 'Click a node to inspect';
    renderer.refresh();
  });

  renderer.on('enterNode', () => {
    container.style.cursor = 'pointer';
  });
  renderer.on('leaveNode', () => {
    container.style.cursor = 'default';
  });
}

// -- Node/Edge reducers --

function reduceNode(node, attrs) {
  const res = { ...attrs };

  if (!activeKinds.has(attrs.kind)) {
    res.hidden = true;
    return res;
  }

  if (hideTests && isTestNode(attrs)) {
    res.hidden = true;
    return res;
  }

  if (searchQuery && !(attrs.label || '').toLowerCase().includes(searchQuery)) {
    res.hidden = true;
    return res;
  }

  if (highlightedNode) {
    if (node === highlightedNode) {
      res.highlighted = true;
      res.zIndex = 2;
    } else if (highlightedNeighbors && highlightedNeighbors.has(node)) {
      res.zIndex = 1;
    } else {
      res.color = '#292e42';
      res.label = '';
      res.zIndex = 0;
    }
  }

  return res;
}

function reduceEdge(edge, attrs) {
  const res = { ...attrs };

  if (highlightedNode) {
    const src = sigmaGraph.source(edge);
    const tgt = sigmaGraph.target(edge);
    if (src !== highlightedNode && tgt !== highlightedNode) {
      res.hidden = true;
    }
  }

  return res;
}

// -- UI updates --

function updateStats(stats) {
  document.getElementById('stat-nodes').textContent = stats.total_nodes;
  document.getElementById('stat-edges').textContent = stats.total_edges;
  document.getElementById('stat-files').textContent = stats.by_kind?.file || 0;
}

function showNodeDetail(nodeId) {
  if (!sigmaGraph.hasNode(nodeId)) return;
  const attrs = sigmaGraph.getNodeAttributes(nodeId);
  const el = document.getElementById('node-detail');
  const deg = sigmaGraph.degree(nodeId);
  el.textContent = `${attrs.kind}  ${nodeId}  ${attrs.file_path}:${attrs.start_line}  (${deg} connections)`;
}

function setConnection(connected) {
  const dot = document.getElementById('conn-dot');
  const label = document.getElementById('conn-label');
  if (connected) {
    dot.classList.add('connected');
    label.textContent = 'live';
  } else {
    dot.classList.remove('connected');
    label.textContent = 'disconnected';
  }
}

function addRecentChange(ev) {
  const ul = document.getElementById('recent-changes');
  const li = document.createElement('li');
  const shortPath = ev.file_path.split('/').slice(-2).join('/');
  li.innerHTML = `<span class="kind ${ev.kind}">${ev.kind}</span> ${shortPath}`;
  ul.prepend(li);
  while (ul.children.length > 20) {
    ul.removeChild(ul.lastChild);
  }
}

// -- Filters --

function buildKindFilters() {
  const container = document.getElementById('kind-filters');
  for (const [kind, cfg] of Object.entries(KIND_STYLES)) {
    const label = document.createElement('label');
    label.className = 'kind-filter';

    const cb = document.createElement('input');
    cb.type = 'checkbox';
    cb.checked = true;
    cb.dataset.kind = kind;
    cb.addEventListener('change', () => {
      if (cb.checked) {
        activeKinds.add(kind);
      } else {
        activeKinds.delete(kind);
      }
      renderer.refresh();
    });

    const dot = document.createElement('span');
    dot.className = 'kind-dot';
    dot.style.background = cfg.color;

    label.appendChild(cb);
    label.appendChild(dot);
    label.appendChild(document.createTextNode(' ' + cfg.label));
    container.appendChild(label);
  }
}

function buildLegend() {
  const container = document.getElementById('legend');
  for (const [kind, cfg] of Object.entries(KIND_STYLES)) {
    const item = document.createElement('span');
    item.className = 'legend-item';
    item.innerHTML = `<span class="kind-dot" style="background:${cfg.color};width:8px;height:8px"></span>${cfg.label}`;
    container.appendChild(item);
  }
}

// -- SSE real-time updates --

function connectSSE() {
  const evtSource = new EventSource('/api/events');

  evtSource.addEventListener('graph_change', async (e) => {
    const change = JSON.parse(e.data);
    addRecentChange(change);

    if (change.kind === 'deleted') {
      const toRemove = [];
      sigmaGraph.forEachNode((id, attrs) => {
        if (attrs.file_path === change.file_path) toRemove.push(id);
      });
      toRemove.forEach(id => sigmaGraph.dropNode(id));
    } else {
      try {
        const resp = await fetch(`/api/graph/file?path=${encodeURIComponent(change.file_path)}`);
        const sub = await resp.json();

        const toRemove = [];
        sigmaGraph.forEachNode((id, attrs) => {
          if (attrs.file_path === change.file_path) toRemove.push(id);
        });
        toRemove.forEach(id => sigmaGraph.dropNode(id));

        populateGraph(sigmaGraph, sub.nodes || [], sub.edges || []);
        startLayout();
      } catch (err) {
        console.error('Failed to fetch file subgraph:', err);
      }
    }

    try {
      const statsResp = await fetch('/api/graph/stats');
      const stats = await statsResp.json();
      updateStats(stats);
    } catch (_) {}

    computeNodeSizes(sigmaGraph);
    renderer.refresh();
  });

  evtSource.addEventListener('keepalive', () => {});
  evtSource.onopen = () => setConnection(true);
  evtSource.onerror = () => setConnection(false);
}

// -- Init --

async function init() {
  initRenderer();
  buildKindFilters();
  buildLegend();

  document.getElementById('search').addEventListener('input', (e) => {
    searchQuery = e.target.value.toLowerCase();
    renderer.refresh();
  });

  document.getElementById('hide-tests').addEventListener('change', (e) => {
    hideTests = e.target.checked;
    renderer.refresh();
  });

  document.getElementById('btn-fit').addEventListener('click', () => {
    renderer.getCamera().animatedReset({ duration: 300 });
  });
  document.getElementById('btn-relayout').addEventListener('click', () => {
    startLayout();
  });

  try {
    const resp = await fetch('/api/graph');
    const data = await resp.json();
    populateGraph(sigmaGraph, data.nodes || [], data.edges || []);
    updateStats(data.stats || {
      total_nodes: (data.nodes || []).length,
      total_edges: (data.edges || []).length,
      by_kind: {},
    });
    startLayout();
  } catch (err) {
    console.error('Failed to load graph:', err);
  }

  document.getElementById('loading').classList.add('hidden');
  connectSSE();
}

if (document.readyState === 'loading') {
  document.addEventListener('DOMContentLoaded', init);
} else {
  init();
}
