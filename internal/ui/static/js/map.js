import { html, useState, useRef, useEffect, fmtUnit } from './lib.js';

// A map panel draws nodes in columns (tiers), the links between them, and
// a card with a node's labels and details on hover. Each query of the
// panel is nodes, links or details (see Query in dashboards.go); its
// series come in as rows of {metric, value, q} like a table's.

const NODE_H = 24, GAP = 6, HEAD = 20, PAD = 8, MAX_NODES = 300;
const RANK = { '': 0, 'lvl-ok': 1, 'lvl-warn': 2, 'lvl-bad': 3 };

// Yellow and red by a query's thresholds; with red below yellow, low is
// bad. The same rule as a table cell's.
export function level(q, v) {
  if (v == null || isNaN(v) || (q.warn == null && q.crit == null)) return '';
  const low = q.warn != null && q.crit != null && q.crit < q.warn;
  const past = t => t != null && (low ? v <= t : v >= t);
  return past(q.crit) ? 'lvl-bad' : past(q.warn) ? 'lvl-warn' : 'lvl-ok';
}

const worse = (a, b) => (RANK[b] > RANK[a] ? b : a);

// build turns the rows into tiers of nodes, links and details. A node's
// key is its label's name and value, so ids under two labels never meet.
export function build(panel, rows) {
  const qs = panel.queries;
  const nodes = new Map(), tiers = [], links = new Map();
  for (const r of rows) {
    const q = qs[r.q || 0];
    if (!q.node) continue;
    const id = r.metric[q.node];
    if (id == null || id === '') continue;
    const key = q.node + '=' + id;
    if (!tiers.includes(q.tier)) tiers.push(q.tier);
    const n = nodes.get(key) || { key, id, tier: q.tier, labels: {}, details: [], level: '' };
    n.name = (q.name && r.metric[q.name]) || n.name || id;
    n.group = (q.group && r.metric[q.group]) || n.group || '';
    Object.assign(n.labels, r.metric);
    n.value = r.value;
    n.level = worse(n.level, level(q, r.value));
    nodes.set(key, n);
  }
  for (const r of rows) {
    const q = qs[r.q || 0];
    if (q.of) {
      const n = nodes.get(q.of + '=' + r.metric[q.of]);
      if (!n) continue;
      const lv = level(q, r.value);
      n.details.push({ name: q.legend || 'Value', value: r.value, unit: q.unit || panel.unit, level: lv });
      n.level = worse(n.level, lv);
    } else if (q.from) {
      const a = q.from + '=' + r.metric[q.from], b = q.to + '=' + r.metric[q.to];
      if (!nodes.has(a) || !nodes.has(b) || a === b) continue;
      const k = a + '>' + b;
      const l = links.get(k) || { a, b, up: false, live: 0 };
      if (r.value > 0) l.up = true;
      if (q.live && r.value > 0) l.live += r.value;
      links.set(k, l);
    }
  }
  const byTier = tiers.map(t => [...nodes.values()].filter(n => n.tier === t)
    .sort((x, y) => (x.group < y.group ? -1 : x.group > y.group ? 1 : x.name < y.name ? -1 : x.name > y.name ? 1 : 0)));
  return { tiers, byTier, nodes, links: [...links.values()] };
}

export function MapView({ panel, data, height }) {
  const box = useRef(null);
  const [width, setWidth] = useState(600);
  const [hover, setHover] = useState(null); // {key, x, y}
  useEffect(() => {
    const el = box.current;
    if (!el) return;
    const ro = new ResizeObserver(() => setWidth(el.clientWidth || 600));
    ro.observe(el);
    return () => ro.disconnect();
  }, []);
  const m = build(panel, data.values);
  if (!m.nodes.size) return html`<div class="empty">No nodes in this time range.</div>`;

  // Lay out: one column per tier, nodes stacked in their groups.
  const colW = width / m.tiers.length, boxW = Math.max(60, Math.min(220, colW - 2 * 28));
  const pos = new Map();
  let total = 0, tallest = 0;
  const heads = [];
  m.byTier.forEach((list, t) => {
    const x = t * colW + (colW - boxW) / 2;
    let y = HEAD + PAD, group = null;
    for (const n of list) {
      if (total >= MAX_NODES) break;
      if (n.group !== group) {
        group = n.group;
        if (group) { heads.push({ x, y: y + 12, text: group }); y += HEAD; }
      }
      pos.set(n.key, { x, y, n });
      y += NODE_H + GAP;
      total++;
    }
    tallest = Math.max(tallest, y);
  });
  const svgH = Math.max(height, tallest + PAD);
  const near = hover && new Set(m.links.filter(l => l.a === hover.key || l.b === hover.key).flatMap(l => [l.a, l.b]));
  const card = hover && m.nodes.get(hover.key);
  const shown = (panel.columns && panel.columns.length ? panel.columns : [])
    .filter(c => card && card.labels[c] != null && card.labels[c] !== '');

  return html`<div class="scroll map" ref=${box} onMouseLeave=${() => setHover(null)}>
    <svg width=${width} height=${svgH} role="img" aria-label=${panel.title}>
      ${m.tiers.map((t, i) => html`<text class="map-tier" x=${i * colW + colW / 2} y="14" text-anchor="middle">${t}</text>`)}
      ${heads.map(h => html`<text class="map-group" x=${h.x} y=${h.y}>${h.text}</text>`)}
      ${m.links.map(l => {
        const a = pos.get(l.a), b = pos.get(l.b);
        if (!a || !b) return null;
        const fwd = b.x >= a.x;
        const x1 = fwd ? a.x + boxW : a.x, x2 = fwd ? b.x : b.x + boxW;
        const y1 = a.y + NODE_H / 2, y2 = b.y + NODE_H / 2, dx = Math.max(30, Math.abs(x2 - x1) / 2) * (fwd ? 1 : -1);
        const lit = hover && (l.a === hover.key || l.b === hover.key);
        return html`<path class="map-link ${l.up ? 'up' : 'down'} ${l.live ? 'live' : ''} ${lit ? 'lit' : ''} ${hover && !lit ? 'dim' : ''}"
          d="M${x1},${y1} C${x1 + dx},${y1} ${x2 - dx},${y2} ${x2},${y2}"
          style=${l.live ? `stroke-width: ${Math.min(5, 1.5 + Math.log2(1 + l.live))}px` : ''}><title>${l.live ? l.live + ' open now' : ''}</title></path>`;
      })}
      ${[...pos.values()].map(({ x, y, n }) => html`<g class="map-node ${n.level} ${hover && hover.key !== n.key && !near.has(n.key) ? 'dim' : ''}"
          transform="translate(${x},${y})" tabindex="0"
          onMouseEnter=${() => setHover({ key: n.key, x: x + boxW + 8, y })} onFocus=${() => setHover({ key: n.key, x: x + boxW + 8, y })}>
        <rect width=${boxW} height=${NODE_H} rx="5" />
        <rect class="map-state" width="4" height=${NODE_H} rx="2" />
        <text x="10" y=${NODE_H / 2 + 4}>${n.name.length > boxW / 7 ? n.name.slice(0, Math.floor(boxW / 7) - 1) + '…' : n.name}</text>
      </g>`)}
    </svg>
    ${total < m.nodes.size && html`<div class="muted" style="font-size: 12px; padding: 0 12px">Showing ${total} of ${m.nodes.size} nodes.</div>`}
    ${card && html`<div class="map-card" style="left: ${Math.min(hover.x, width - 250)}px; top: ${hover.y}px">
      <div class="map-card-title"><span class="dot ${card.level}"></span>${card.name}<span class="muted"> · ${card.tier}</span></div>
      ${card.name !== card.id && html`<div class="kv"><span>id</span><span class="mono">${card.id}</span></div>`}
      ${shown.map(c => html`<div class="kv"><span>${c}</span><span class="mono">${card.labels[c]}</span></div>`)}
      ${card.details.map(d => html`<div class="kv ${d.level}"><span>${d.name}</span><span class="mono">${fmtUnit(d.value, d.unit)}</span></div>`)}
    </div>`}
  </div>`;
}
