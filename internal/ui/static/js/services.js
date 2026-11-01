import { html, useState, useEffect, useMemo, api, useLoad, setParams, Badge, ErrorBox, fmtNum, fmtUnit } from './lib.js';
import { Chart, useRange, rangeParams, COLORS, parseLines } from './explore.js';
import { TimeRange } from './timerange.js';
import { MapView } from './map.js';

// Services: what applications instrumented with OpenTelemetry report.
// The numbers come from metrics the agents count from every span; the
// traces themselves are in the trace store (VictoriaTraces), read through
// the server: spans as rows with a LogsQL filter, a whole trace by its id.

const ENTRY = 'kind=~"server|consumer"';
const form = o => ({ method: 'POST', body: new URLSearchParams(o) });
const q = s => JSON.stringify(String(s)); // a quoted PromQL / LogsQL string
const ms = v => fmtUnit(Number(v) / 1000, 'seconds');

async function instant(expr, time) {
  const r = await api('/api/v1/query', form({ query: expr, time }));
  return r.data.result;
}
async function matrix(expr, from, to) {
  const step = Math.max(15, Math.ceil((to - from) / 240));
  const r = await api('/api/v1/query_range', form({ query: expr, start: from, end: to, step }));
  return { series: r.data.result, from, to, step };
}
// spans returns stored spans as rows. Fields are as the trace store keeps
// them: name, duration (nanoseconds), status_code (2 is failed), kind
// (2 server, 5 consumer), trace_id, agent, and the attributes under
// "span_attr:…" and "resource_attr:…".
async function spans(query, from, to, limit) {
  const text = await api('/api/v1/traces/query', form({ query, start: from, end: to, limit }));
  return parseLines(text);
}
const nsToMs = v => Number(v) / 1e6;
const usd = v => '$' + (v > 0 && v < 0.01 ? v.toFixed(4) : v.toFixed(2));
// What model calls cost, from the server's estimate over stored spans; null
// when there is nothing to say (no trace store, no model calls).
async function spanCost(query) {
  try {
    const r = await api('/api/v1/costs/spans?' + query);
    return r.calls > 0 ? r : null;
  } catch { return null; }
}
// Timestamps carry nanoseconds; Date keeps milliseconds. Sub-millisecond
// order matters for sibling spans, so keep the fraction.
function timeMs(iso) {
  const m = /^(.*?)(?:\.(\d+))?Z$/.exec(iso);
  if (!m) return Date.parse(iso);
  return Date.parse(m[1] + 'Z') + (m[2] ? Number('0.' + m[2]) * 1000 : 0);
}
const by = (rows, label) => Object.fromEntries(rows.map(r => [r.metric[label] ?? '', Number(r.value[1])]));
const pct = (part, whole) => (whole > 0 ? (100 * (part || 0)) / whole : 0);

function RangePicker({ range, setRange }) {
  return html`<${TimeRange} range=${range} setRange=${setRange} />`;
}

export function Services({ route }) {
  const [, a, b] = route.parts;
  if (a === '~trace' && b) return html`<${TraceView} key=${b} id=${b} params=${route.params} />`;
  if (a) return html`<${ServiceView} key=${a} name=${decodeURIComponent(a)} params=${route.params} />`;
  return html`<${ServiceList} params=${route.params} />`;
}

// loadServices gives each service's request rate, share of errors and
// latency over the window.
async function loadServices(from, to) {
  const w = Math.round(to - from) + 's';
  const [all, entry, failed, p50, p95, ai] = await Promise.all([
    instant(`sum by (service) (increase(traces_spans_total[${w}]))`, to),
    instant(`sum by (service) (increase(traces_spans_total{${ENTRY}}[${w}]))`, to),
    instant(`sum by (service) (increase(traces_spans_total{${ENTRY},status="error"}[${w}]))`, to),
    instant(`histogram_quantile(0.5, sum by (service, le) (increase(traces_span_duration_seconds_bucket{${ENTRY}}[${w}])))`, to),
    instant(`histogram_quantile(0.95, sum by (service, le) (increase(traces_span_duration_seconds_bucket{${ENTRY}}[${w}])))`, to),
    instant(`sum by (owner) (sum_over_time(ai_cost_usd_per_minute{source="application"}[${w}]))`, to).catch(() => []),
  ]);
  const e = by(entry, 'service'), f = by(failed, 'service'), m = by(p50, 'service'), p = by(p95, 'service'), c = by(ai, 'owner');
  return all.map(r => r.metric.service).sort().map(name => ({
    name, rate: (e[name] || 0) / (to - from), errors: pct(f[name], e[name]), p50: m[name], p95: p[name], entry: e[name] > 0, ai: c[name] || 0,
  }));
}

function ServiceList({ params }) {
  const [range, setRange, win] = useRange(params);
  const [view, setView] = useState(params.get('view') === 'map' ? 'map' : 'table');
  useEffect(() => { setParams(['services'], { ...rangeParams(range), ...(view === 'map' ? { view } : {}) }); }, [range, view]);
  const { data, error } = useLoad(() => { const [from, to] = win(); return loadServices(from, to); }, [range], 30000);
  return html`
    <div class="head"><h1>Services</h1><span class="spacer"></span>
      <${RangePicker} range=${range} setRange=${setRange} />
      <a class="btn" href="/docs/apm" target="_blank" rel="noopener">Help</a></div>
    <nav class="tabs">${[['table', 'Table'], ['map', 'Map']].map(([v, label]) => html`
      <a href="#" class=${view === v ? 'on' : ''} onClick=${e => { e.preventDefault(); setView(v); }}>${label}</a>`)}</nav>
    <${ErrorBox} error=${error} />
    ${view === 'map' && data && data.length > 0 ? html`<${ServiceMap} services=${data} range=${range} win=${win} />` : html`
    <div class="panel flush">${!data ? html`<div class="empty">Loading…</div>` : data.length === 0
      ? html`<div class="empty">No traces in this time range. Applications instrumented with OpenTelemetry send them to the agent on their host
          (<code>OTEL_EXPORTER_OTLP_ENDPOINT=http://localhost:4318</code>, protocol <code>http/protobuf</code>), once a profile
          sets <code>otlp: {enabled: true}</code>. <code>vigil tracegen</code> sends demo traces to try it. <a href="/docs/apm" target="_blank" rel="noopener">How</a></div>`
      : html`<table>
        <thead><tr><th>Service</th><th style="text-align: right">Requests</th><th style="text-align: right">Errors</th>
          <th style="text-align: right">Median</th><th style="text-align: right">95th percentile</th>
          ${data.some(s => s.ai > 0) && html`<th style="text-align: right" title="Model calls priced with costs.tokens, in the chosen time">AI cost</th>`}</tr></thead>
        <tbody>${data.map(s => html`<tr class="click" onClick=${() => { location.hash = '#/services/' + encodeURIComponent(s.name) + '?' + new URLSearchParams(rangeParams(range)); }}>
          <td><strong>${s.name}</strong>${!s.entry && html` <span class="muted">only calls out</span>`}</td>
          <td class="num">${s.entry ? fmtUnit(s.rate, 'ops/s') : '–'}</td>
          <td class="num">${s.entry ? html`<${Badge} kind=${s.errors >= 5 ? 'bad' : s.errors > 0 ? 'warn' : 'ok'}>${s.errors.toFixed(s.errors < 10 ? 1 : 0)}%<//>` : '–'}</td>
          <td class="num">${isFinite(s.p50) ? fmtUnit(s.p50, 'seconds') : '–'}</td>
          <td class="num">${isFinite(s.p95) ? fmtUnit(s.p95, 'seconds') : '–'}</td>
          ${data.some(x => x.ai > 0) && html`<td class="num">${s.ai > 0 ? usd(s.ai) : '–'}</td>`}
        </tr>`)}</tbody></table>`}</div>`}
    <p class="muted">Requests are what a service was asked to do: its server and consumer spans. The numbers count every request;
      which traces are stored in full is a separate setting (<code>otlp.traces.sample</code>).</p>`;
}

// mapModel lays services out by how deep in the calls they sit: services
// nobody else calls first, then what they call, and so on; what services
// call that is not itself a service (a database, an outside API) last.
// Edges come from the agents' call counts (every call); calls between two
// services that only stored traces show (the client span named no peer)
// are added from the trace store's dependencies.
export function mapModel(services, calls, failed, deps) {
  const names = new Set(services.map(s => s.name));
  const edges = new Map();
  const edge = (from, to) => {
    const k = from + '\u0000' + to;
    if (!edges.has(k)) edges.set(k, { from, to, calls: 0, errors: 0, sampled: false });
    return edges.get(k);
  };
  for (const r of calls) if (r.metric.peer && r.metric.service !== r.metric.peer) edge(r.metric.service, r.metric.peer).calls += Number(r.value[1]);
  for (const r of failed) if (r.metric.peer && r.metric.service !== r.metric.peer) edge(r.metric.service, r.metric.peer).errors += Number(r.value[1]);
  for (const d of deps || []) {
    if (!d.parent || !d.child || d.parent === d.child || edges.has(d.parent + '\u0000' + d.child)) continue;
    const e = edge(d.parent, d.child);
    e.calls = Number(d.callCount) || 0;
    e.sampled = true;
  }
  const list = [...edges.values()].filter(e => e.calls > 0);
  const depth = new Map();
  const called = new Set(list.filter(e => names.has(e.from) && names.has(e.to)).map(e => e.to));
  let front = [...names].filter(n => !called.has(n));
  if (!front.length) front = [...names]; // every service is called by another: a cycle
  front.forEach(n => depth.set(n, 0));
  while (front.length) {
    const next = [];
    for (const e of list) {
      if (front.includes(e.from) && names.has(e.to) && !depth.has(e.to)) { depth.set(e.to, depth.get(e.from) + 1); next.push(e.to); }
    }
    front = next;
  }
  for (const n of names) if (!depth.has(n)) depth.set(n, 0);
  const tierName = d => (d === 0 ? 'Entry' : d === 1 ? 'Called by them' : `${d} calls deep`);
  const tiers = [...new Set([...depth.values()])].sort((a, b) => a - b).map(tierName);
  const outside = [...new Set(list.map(e => e.to).filter(n => !names.has(n)))];
  if (outside.length) tiers.push('Databases and outside');
  return { names, depth, tierName, tiers, outside, edges: list };
}

function ServiceMap({ services, range, win }) {
  const { data, error } = useLoad(async () => {
    const [from, to] = win();
    const w = Math.round(to - from) + 's';
    const [calls, failed, deps] = await Promise.all([
      instant(`sum by (service, peer) (increase(traces_service_calls_total[${w}]))`, to),
      instant(`sum by (service, peer) (increase(traces_service_calls_total{status="error"}[${w}]))`, to),
      api(`/api/v1/traces/jaeger/api/dependencies?endTs=${Math.round(to * 1000)}&lookback=${Math.round((to - from) * 1000)}`)
        .then(r => r.data).catch(() => null), // no trace store: the counts alone
    ]);
    return { m: mapModel(services, calls, failed, deps), span: to - from };
  }, [range, services], 30000);
  if (error) return html`<${ErrorBox} error=${error} />`;
  if (!data) return html`<div class="panel"><div class="empty">Loading…</div></div>`;
  const { m, span } = data;
  const byName = Object.fromEntries(services.map(s => [s.name, s]));
  const queries = m.tiers.map(t => ({ node: 'service', tier: t, warn: 1, crit: 5 }));
  queries.push(
    { of: 'service', legend: 'Requests', unit: 'ops/s' },
    { of: 'service', legend: 'Errors', unit: 'percent', warn: 1, crit: 5 },
    { of: 'service', legend: '95th percentile', unit: 'seconds' },
    { from: 'service', to: 'peer', as: 'service', weight: true,
      title: r => `${r.metric.service} → ${r.metric.peer}: ${fmtUnit(r.calls / span, 'ops/s')}, ${r.errorPct.toFixed(1)}% failed${r.sampled ? ' (from stored traces)' : ''}` },
    { from: 'service', to: 'peer', as: 'service', warn: 1, crit: 5 });
  const at = name => queries.findIndex(q => q.tier === name);
  const rows = [];
  // The map takes its columns in the order nodes first appear.
  const ordered = [...services].sort((x, y) => m.depth.get(x.name) - m.depth.get(y.name));
  for (const s of ordered) {
    const tier = at(m.tierName(m.depth.get(s.name)));
    rows.push({ q: tier, metric: { service: s.name }, value: s.entry ? s.errors : null });
    if (s.entry) {
      rows.push({ q: m.tiers.length, metric: { service: s.name }, value: s.rate });
      rows.push({ q: m.tiers.length + 1, metric: { service: s.name }, value: s.errors });
    }
    if (isFinite(s.p95)) rows.push({ q: m.tiers.length + 2, metric: { service: s.name }, value: s.p95 });
  }
  for (const n of m.outside) rows.push({ q: at('Databases and outside'), metric: { service: n }, value: null });
  for (const e of m.edges) {
    const errorPct = pct(e.errors, e.calls);
    rows.push({ q: m.tiers.length + 3, metric: { service: e.from, peer: e.to }, value: e.calls / span * 60, calls: e.calls, errorPct, sampled: e.sampled });
    rows.push({ q: m.tiers.length + 4, metric: { service: e.from, peer: e.to }, value: errorPct });
  }
  const panel = { title: 'Service map', queries, columns: [] };
  const height = 60 + 30 * Math.max(...m.tiers.map((_, i) => rows.filter(r => r.q === i).length), 1);
  const open = n => { if (m.names.has(n.id)) location.hash = '#/services/' + encodeURIComponent(n.id) + '?' + new URLSearchParams(rangeParams(range)); };
  return html`<div class="panel flush"><${MapView} panel=${panel} data=${{ values: rows }} height=${height} onPick=${open} /></div>
    <p class="muted">Lines are calls, thicker for more; red and yellow ones fail often (over 5% and 1%). A box is red or yellow by its
      own share of failed requests. Click a service to open it. Calls show between two services when the client span names the
      other one (<code>peer.service</code>) or when stored traces show it.</p>`;
}

// CheckChange asks the server whether the service did better or worse
// after a moment (a deploy, a config change): the same time before and
// after, on requests, failures and the 95th percentile.
const VERDICT = { better: 'ok', worse: 'bad', mixed: 'warn', 'no change': 'info', 'not enough data': 'off' };
function CheckChange({ name }) {
  const local = d => new Date(d.getTime() - d.getTimezoneOffset() * 60000).toISOString().slice(0, 16);
  const [when, setWhen] = useState(local(new Date(Date.now() - 30 * 60000)));
  const [win, setWin] = useState('15m');
  const [res, setRes] = useState(null);
  const [err, setErr] = useState(null);
  const check = async e => {
    e.preventDefault();
    setErr(null);
    try {
      const at = Math.floor(new Date(when).getTime() / 1000);
      setRes(await api(`/api/v1/verify?service=${encodeURIComponent(name)}&at=${at}&window=${win}`));
    } catch (x) { setErr(x.message); setRes(null); }
  };
  const val = (v, unit) => (v == null ? '–' : fmtUnit(v, unit));
  return html`<h2 class="section">Did a change help?</h2>
    <form class="row" style="margin-bottom: 10px" onSubmit=${check}>
      <label style="margin: 0">Changed at <input type="datetime-local" value=${when} onChange=${e => setWhen(e.target.value)} required /></label>
      <label style="margin: 0">compare <select value=${win} onChange=${e => setWin(e.target.value)}>
        ${['5m', '15m', '30m', '1h', '2h'].map(w => html`<option value=${w}>${w}</option>`)}</select> before and after</label>
      <button>Check</button></form>
    <${ErrorBox} error=${err} />
    ${res && html`<div class="panel flush">
      <div style="padding: 12px 16px"><${Badge} kind=${VERDICT[res.verdict] || 'info'}>${res.verdict}<//>
        <span class="muted"> ${res.window} before and after ${new Date(res.at).toLocaleString()}</span></div>
      ${res.signals.length > 0 && html`<table><thead><tr><th>Signal</th><th style="text-align: right">Before</th><th style="text-align: right">After</th><th></th></tr></thead>
        <tbody>${res.signals.map(sg => html`<tr><td>${sg.name}</td><td class="num">${val(sg.before, sg.unit)}</td><td class="num">${val(sg.after, sg.unit)}</td>
          <td>${sg.verdict !== 'no change' && html`<${Badge} kind=${VERDICT[sg.verdict]}>${sg.verdict}<//>`} <span class="muted">${sg.why || ''}</span></td></tr>`)}</tbody></table>`}
      ${res.limits.length > 0 && html`<p class="muted" style="padding: 0 16px">${res.limits.join('. ')}.</p>`}</div>`}`;
}

function ServiceView({ name, params }) {
  const [range, setRange, win] = useRange(params);
  const [errorsOnly, setErrorsOnly] = useState(params.get('errors') === '1');
  const [minMs, setMinMs] = useState(params.get('min') || '');
  const [op, setOp] = useState(params.get('op') || '');
  useEffect(() => { setParams(['services', encodeURIComponent(name)], { ...rangeParams(range), ...(errorsOnly ? { errors: 1 } : {}), ...(minMs ? { min: minMs } : {}), ...(op ? { op } : {}) }); }, [range, errorsOnly, minMs, op]);
  const sel = `service=${q(name)},${ENTRY}`;

  const { data, error } = useLoad(async () => {
    const [from, to] = win();
    const w = Math.round(to - from) + 's';
    const step = Math.max(15, Math.ceil((to - from) / 240));
    const rw = Math.max(60, 4 * step) + 's';
    const [rate, dur, ops, opErr, opP95, opAvg, out, outErr, outAvg, inn, ai] = await Promise.all([
      matrix(`sum by (status) (rate(traces_spans_total{${sel}}[${rw}]))`, from, to),
      matrix(`label_replace(histogram_quantile(0.5, sum by (le) (rate(traces_span_duration_seconds_bucket{${sel}}[${rw}]))), "q", "median", "", "")
        or label_replace(histogram_quantile(0.95, sum by (le) (rate(traces_span_duration_seconds_bucket{${sel}}[${rw}]))), "q", "95th", "", "")
        or label_replace(histogram_quantile(0.99, sum by (le) (rate(traces_span_duration_seconds_bucket{${sel}}[${rw}]))), "q", "99th", "", "")`, from, to),
      instant(`sum by (span_name, kind) (increase(traces_spans_total{service=${q(name)}}[${w}]))`, to),
      instant(`sum by (span_name, kind) (increase(traces_spans_total{service=${q(name)},status="error"}[${w}]))`, to),
      instant(`histogram_quantile(0.95, sum by (span_name, kind, le) (increase(traces_span_duration_seconds_bucket{service=${q(name)}}[${w}])))`, to),
      instant(`sum by (span_name, kind) (increase(traces_span_duration_seconds_sum{service=${q(name)}}[${w}])) / sum by (span_name, kind) (increase(traces_span_duration_seconds_count{service=${q(name)}}[${w}]))`, to),
      instant(`sum by (peer) (increase(traces_service_calls_total{service=${q(name)}}[${w}]))`, to),
      instant(`sum by (peer) (increase(traces_service_calls_total{service=${q(name)},status="error"}[${w}]))`, to),
      instant(`sum by (peer) (increase(traces_service_call_duration_seconds_sum{service=${q(name)}}[${w}])) / sum by (peer) (increase(traces_service_call_duration_seconds_count{service=${q(name)}}[${w}]))`, to),
      instant(`sum by (service) (increase(traces_service_calls_total{peer=${q(name)}}[${w}]))`, to),
      instant(`sum by (model) (sum_over_time(ai_cost_usd_per_minute{source="application",owner=${q(name)}}[${w}]))`, to).catch(() => []),
    ]);
    const key = r => r.metric.span_name + '\u0000' + r.metric.kind;
    const idx = rows => Object.fromEntries(rows.map(r => [key(r), Number(r.value[1])]));
    const e = idx(opErr), p = idx(opP95), a = idx(opAvg);
    const operations = ops.map(r => ({ name: r.metric.span_name, kind: r.metric.kind, count: Number(r.value[1]), rate: Number(r.value[1]) / (to - from),
      errors: pct(e[key(r)], Number(r.value[1])), p95: p[key(r)], avg: a[key(r)] }))
      .filter(o => o.count > 0).sort((x, y) => (x.kind === y.kind ? y.count - x.count : x.kind < y.kind ? 1 : -1));
    const oe = by(outErr, 'peer'), oa = by(outAvg, 'peer');
    const calls = out.map(r => ({ peer: r.metric.peer, rate: Number(r.value[1]) / (to - from), errors: pct(oe[r.metric.peer], Number(r.value[1])), avg: oa[r.metric.peer] }))
      .filter(c => c.rate > 0).sort((x, y) => y.rate - x.rate);
    const callers = inn.filter(r => Number(r.value[1]) > 0).map(r => ({ service: r.metric.service, rate: Number(r.value[1]) / (to - from) }));
    const aiCost = ai.map(r => ({ model: r.metric.model, usd: Number(r.value[1]) })).filter(r => r.usd > 0).sort((x, y) => y.usd - x.usd);
    return { rate, dur, operations, calls, callers, aiCost };
  }, [range, name], 30000);

  const { data: traces, error: tracesError } = useLoad(async () => {
    const [from, to] = win();
    const filter = [`"resource_attr:service.name":=${q(name)}`, `kind:(2 OR 5)`];
    if (errorsOnly) filter.push('status_code:2');
    if (Number(minMs) > 0) filter.push(`duration:>${Math.round(Number(minMs) * 1e6)}`);
    if (op) filter.push(`name:=${q(op)}`);
    const rows = await spans(filter.join(' ') + ' | sort by (_time desc) | limit 60', from, to, 60);
    return rows.sort((x, y) => (x._time < y._time ? 1 : -1));
  }, [range, name, errorsOnly, minMs, op], 30000);

  const zoom = (a, b) => setRange({ from: a, to: b });
  const svc = n => html`<a href=${'#/services/' + encodeURIComponent(n) + '?' + new URLSearchParams(rangeParams(range))}>${n}</a>`;
  return html`
    <div class="head"><a href=${'#/services?' + new URLSearchParams(rangeParams(range))}>← Services</a><h1>${name}</h1><span class="spacer"></span>
      <${RangePicker} range=${range} setRange=${setRange} /></div>
    <${ErrorBox} error=${error} />
    ${data && html`
      <div class="grid">
        <section class="panel tile" style="grid-column: span 6; height: 250px"><header><h2>Requests per second</h2></header>
          <div class="tile-body"><${Chart} result=${data.rate} names=${data.rate.series.map(s => s.metric.status)} height=${190} unit="ops/s" min=${0} fill onZoom=${zoom} sync="svc" /></div></section>
        <section class="panel tile" style="grid-column: span 6; height: 250px"><header><h2>Duration</h2></header>
          <div class="tile-body"><${Chart} result=${data.dur} names=${data.dur.series.map(s => s.metric.q)} height=${190} unit="seconds" min=${0} onZoom=${zoom} sync="svc" /></div></section>
      </div>
      ${data.aiCost.length > 0 && html`<p class="muted">Model calls in this time cost
        about <strong>${usd(data.aiCost.reduce((a, r) => a + r.usd, 0))}</strong>
        (${data.aiCost.map((r, i) => html`${i > 0 && ', '}${r.model} ${usd(r.usd)}`)}), at the prices in <code>costs.tokens</code>.</p>`}
      <h2 class="section">Operations</h2>
      <div class="panel flush"><table>
        <thead><tr><th>Operation</th><th>Kind</th><th style="text-align: right">Per second</th><th style="text-align: right">Errors</th>
          <th style="text-align: right">Average</th><th style="text-align: right">95th percentile</th></tr></thead>
        <tbody>${data.operations.length === 0 && html`<tr><td colspan="6" class="empty">Nothing in this time range.</td></tr>`}
        ${data.operations.map(o => html`<tr class=${o.kind === 'server' || o.kind === 'consumer' ? 'click' : ''}
            onClick=${() => (o.kind === 'server' || o.kind === 'consumer') && setOp(op === o.name ? '' : o.name)}>
          <td class="mono">${o.name}${op === o.name && html` <${Badge} kind="info">traces below<//>`}</td><td class="muted">${o.kind}</td>
          <td class="num">${fmtUnit(o.rate, 'ops/s')}</td>
          <td class="num">${o.errors > 0 ? html`<${Badge} kind=${o.errors >= 5 ? 'bad' : 'warn'}>${o.errors.toFixed(1)}%<//>` : '0%'}</td>
          <td class="num">${isFinite(o.avg) ? fmtUnit(o.avg, 'seconds') : '–'}</td>
          <td class="num">${isFinite(o.p95) ? fmtUnit(o.p95, 'seconds') : '–'}</td></tr>`)}</tbody></table></div>
      <${CheckChange} name=${name} />
      ${(data.calls.length > 0 || data.callers.length > 0) && html`
        <h2 class="section">Depends on</h2>
        <div class="panel flush"><table>
          <thead><tr><th>Calls</th><th style="text-align: right">Per second</th><th style="text-align: right">Errors</th><th style="text-align: right">Average</th></tr></thead>
          <tbody>${data.calls.length === 0 && html`<tr><td colspan="4" class="empty">This service calls nothing that its spans name.</td></tr>`}
          ${data.calls.map(c => html`<tr><td>${svc(c.peer)}</td><td class="num">${fmtUnit(c.rate, 'ops/s')}</td>
            <td class="num">${c.errors > 0 ? html`<${Badge} kind=${c.errors >= 5 ? 'bad' : 'warn'}>${c.errors.toFixed(1)}%<//>` : '0%'}</td>
            <td class="num">${isFinite(c.avg) ? fmtUnit(c.avg, 'seconds') : '–'}</td></tr>`)}</tbody></table></div>
        ${data.callers.length > 0 && html`<p class="muted">Called by: ${data.callers.map((c, i) => html`${i > 0 && ', '}${svc(c.service)} (${fmtUnit(c.rate, 'ops/s')})`)}</p>`}`}`}
    <h2 class="section">Traces</h2>
    <div class="row" style="margin-bottom: 10px">
      <label style="margin: 0"><input type="checkbox" checked=${errorsOnly} onChange=${e => setErrorsOnly(e.target.checked)} /> Failed only</label>
      <label style="margin: 0">Slower than <input type="number" min="0" style="width: 90px" value=${minMs} placeholder="ms"
        onChange=${e => setMinMs(e.target.value)} aria-label="Slower than, in milliseconds" /> ms</label>
      ${op && html`<span class="chip">${op}</span><button class="small" onClick=${() => setOp('')}>All operations</button>`}
    </div>
    <${ErrorBox} error=${tracesError && (/no trace store/.test(tracesError) ? 'This server keeps no traces: it has no trace store (storage.traces_url). The numbers above do not need one.' : tracesError)} />
    <div class="panel flush">${tracesError ? null : !traces ? html`<div class="empty">Loading…</div>` : traces.length === 0
      ? html`<div class="empty">No stored traces match. The numbers above count every request; traces are stored for the share set by <code>otlp.traces.sample</code>, and always when they failed. A trace shows here a few seconds after it ends.</div>`
      : html`<table>
        <thead><tr><th>Started</th><th>Operation</th><th style="text-align: right">Duration</th><th>Outcome</th><th>Host</th></tr></thead>
        <tbody>${traces.map(t => html`<tr class="click" onClick=${() => { location.hash = `#/services/~trace/${t.trace_id}?t=${Math.round(timeMs(t._time) / 1000)}`; }}>
          <td style="white-space: nowrap" title=${t._time}>${new Date(t._time).toLocaleString()}</td>
          <td class="mono">${t.name}</td><td class="num">${ms(nsToMs(t.duration))}</td>
          <td>${t.status_code === '2' ? html`<${Badge} kind="bad">failed<//> <span class="muted">${t.status_message || ''}</span>` : html`<${Badge} kind="ok">ok<//>`}</td>
          <td class="muted">${t.agent || ''}</td></tr>`)}</tbody></table>`}</div>`;
}

// One trace as a waterfall: every span a bar on a shared time axis, nested
// under the span that started it.
// WorkItemCost: what the model calls of everything done for one work item
// (a Boop item, a ticket) cost in the last week, from spans carrying its key.
function WorkItemCost({ item }) {
  const { data: cost } = useLoad(() => spanCost('work_item=' + encodeURIComponent(item)), [item]);
  return html`<div style="margin-top: 6px">Work item <span class="chip">${item}</span>${cost
    ? html` · ${cost.calls} model calls in the last 7 days, about <strong>${usd(cost.cost_usd)}</strong>` : ''}</div>`;
}

function TraceView({ id, params }) {
  const [open, setOpen] = useState(null);
  const { data: trace, error } = useLoad(async () => {
    if (!/^[0-9a-f]{16,32}$/.test(id)) throw new Error('This is not a trace id.');
    const r = await api('/api/v1/traces/jaeger/api/traces/' + id);
    return (r.data || [])[0] || { spans: [], processes: {} };
  }, [id]);
  const { data: cost } = useLoad(() => spanCost('trace=' + id), [id]);

  const view = useMemo(() => {
    if (!trace || trace.spans.length === 0) return null;
    // The store answers in Jaeger's form: times in microseconds, attributes
    // as tags, events as logs, the service under the span's process.
    const tagMap = tags => Object.fromEntries((tags || []).map(t => [t.key, String(t.value)]));
    const all = trace.spans.map(sp => {
      const tags = tagMap(sp.tags), proc = trace.processes[sp.processID] || {};
      const parent = (sp.references || []).find(r => r.refType === 'CHILD_OF');
      const fields = { ...tags };
      delete fields.error;
      for (const [k, v] of Object.entries(tagMap(proc.tags))) fields['resource: ' + k] = v;
      const events = (sp.logs || []).map(l => tagMap(l.fields));
      const exc = events.find(e => e.event === 'exception');
      if (exc) for (const k of ['exception.type', 'exception.message', 'exception.stacktrace']) if (exc[k]) fields[k] = exc[k];
      if (events.length) fields.events = String(events.length);
      return { span_id: sp.spanID, parent_span_id: parent && parent.spanID, name: sp.operationName, service: proc.serviceName || 'unknown',
        start: sp.startTime / 1000, dur: sp.duration / 1000, failed: tags.error === 'true', fields, children: [] };
    });
    const byId = new Map(all.map(s => [s.span_id, s]));
    const roots = [];
    for (const s of all) {
      const parent = s.parent_span_id && byId.get(s.parent_span_id);
      if (parent) parent.children.push(s); else { s.orphan = !!s.parent_span_id; roots.push(s); }
    }
    const order = [];
    const walk = (s, depth) => { s.depth = depth; order.push(s); s.children.sort((a, b) => a.start - b.start).forEach(c => walk(c, depth + 1)); };
    roots.sort((a, b) => a.start - b.start).forEach(r => walk(r, 0));
    const t0 = Math.min(...all.map(s => s.start)), t1 = Math.max(...all.map(s => s.start + s.dur));
    const services = [...new Set(all.map(s => s.service))].sort();
    return { order, t0, total: Math.max(t1 - t0, 0.001), services, root: roots.find(r => !r.orphan) || roots[0],
      errors: all.filter(s => s.failed).length, partial: roots.some(r => r.orphan),
      workItem: all.map(s => s.fields['work_item.key'] || s.fields['resource: work_item.key']).find(Boolean) };
  }, [trace]);

  const none = /no trace store/.test(error || '');
  if (error) return html`<div class="head"><a href="#/services">← Services</a><h1>Trace</h1></div>
    <${ErrorBox} error=${none ? 'This server keeps no traces: it has no trace store (storage.traces_url).' : error} />`;
  if (!trace) return html`<div class="empty">Loading…</div>`;
  if (!view) return html`<div class="head"><a href="#/services">← Services</a><h1>Trace</h1></div>
    <div class="panel empty">No spans are stored for this trace id. It may not have been sampled, it may be older than traces are kept, or it ended only seconds ago.</div>`;
  const color = s => COLORS[view.services.indexOf(s) % COLORS.length];
  return html`
    <div class="head"><a href=${'#/services/' + encodeURIComponent(view.root.service)}>← ${view.root.service}</a>
      <h1>${view.root.name}</h1>${view.errors > 0 && html`<${Badge} kind="bad">${view.errors} failed<//>`}<span class="spacer"></span>
      <a class="btn" href=${`#/explore/logs?q=${encodeURIComponent(`trace_id:=${q(id)}`)}&from=${Math.floor(view.t0 / 1000) - 60}&to=${Math.ceil((view.t0 + view.total) / 1000) + 60}`}>Logs of this trace</a></div>
    <div class="panel">
      <div><strong>${ms(view.total)}</strong> · ${view.order.length} spans · ${new Date(view.t0).toLocaleString()} · <span class="mono muted">${id}</span></div>
      ${cost && html`<div style="margin-top: 6px" title=${cost.limits.join(' ')}>${cost.calls} model call${cost.calls === 1 ? '' : 's'},
        about <strong>${usd(cost.cost_usd)}</strong>${cost.models.some(m => !m.priced) && html` <span class="muted">(some models have no price)</span>`}</div>`}
      ${view.workItem && html`<${WorkItemCost} item=${view.workItem} />`}
      <div style="margin-top: 6px">${view.services.map(s => html`<span class="chip"><span class="dot" style="background: ${color(s)}"></span>${s}</span>`)}</div>
      ${view.partial && html`<div class="muted" style="margin-top: 6px">Part of this trace is missing: some spans' parents were not stored (not sampled, not sent yet, or sent to a server that does not keep traces).</div>`}
    </div>
    <div class="panel flush waterfall">
      ${view.order.map(s => html`
        <div class="span ${s.failed ? 'failed' : ''} ${open === s.span_id ? 'open' : ''}" onClick=${() => setOpen(open === s.span_id ? null : s.span_id)}>
          <div class="name" style="padding-left: ${8 + Math.min(s.depth, 12) * 14}px" title=${`${s.service}: ${s.name}`}>
            <span class="dot" style="background: ${color(s.service)}"></span><strong>${s.service}</strong> ${s.name}</div>
          <div class="lane"><div class="bar" style="left: ${(100 * (s.start - view.t0)) / view.total}%; width: ${Math.max((100 * s.dur) / view.total, 0.3)}%; background: ${s.failed ? 'var(--bad)' : color(s.service)}"></div>
            ${(() => {
              // The duration goes after the bar, or before it when the bar reaches the right edge.
              const end = (100 * (s.start - view.t0 + s.dur)) / view.total, begin = (100 * (s.start - view.t0)) / view.total;
              return end > 86 && begin > 14
                ? html`<span class="d before" style="right: ${100 - begin}%">${ms(s.dur)}</span>`
                : end > 86 ? html`<span class="d inside" style="right: ${Math.max(100 - end, 0)}%">${ms(s.dur)}</span>`
                : html`<span class="d" style="left: ${end}%">${ms(s.dur)}</span>`;
            })()}</div>
        </div>
        ${open === s.span_id && html`<div class="spanfields"><table>
          ${s.orphan && html`<tr><td>note</td><td>the span that started this one is not stored</td></tr>`}
          <tr><td>starts</td><td>+${ms(s.start - view.t0)} into the trace</td></tr>
          <tr><td>span id</td><td>${s.span_id}</td></tr>
          ${Object.entries(s.fields).filter(([, v]) => v !== '').sort().map(([k, v]) => html`<tr><td>${k}</td>
            <td>${k === 'exception.stacktrace' ? html`<pre>${v}</pre>` : v}</td></tr>`)}
        </table></div>`}`)}
    </div>
    <p class="muted">Bars share one time axis, from the start of the first span to the end of the last. Click a span for everything it recorded.</p>`;
}
