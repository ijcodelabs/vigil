import { html, useState, useEffect, useMemo, api, useLoad, setParams, Badge, ErrorBox, fmtNum, fmtUnit } from './lib.js';
import { Chart, useRange, rangeParams, RANGES, COLORS, parseLines } from './explore.js';

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
  return range.last
    ? html`<select aria-label="Time range" value=${range.last} onChange=${e => setRange({ last: +e.target.value })}>
        ${RANGES.map(([l, s]) => html`<option value=${s}>Last ${l}</option>`)}</select>`
    : html`<span class="badge info">${new Date(range.from * 1000).toLocaleString()} – ${new Date(range.to * 1000).toLocaleTimeString()}</span>
        <button class="small" onClick=${() => setRange({ last: 3600 })}>Back to last 1h</button>`;
}

export function Services({ route }) {
  const [, a, b] = route.parts;
  if (a === '~trace' && b) return html`<${TraceView} key=${b} id=${b} params=${route.params} />`;
  if (a) return html`<${ServiceView} key=${a} name=${decodeURIComponent(a)} params=${route.params} />`;
  return html`<${ServiceList} params=${route.params} />`;
}

function ServiceList({ params }) {
  const [range, setRange, win] = useRange(params);
  useEffect(() => { setParams(['services'], rangeParams(range)); }, [range]);
  const { data, error } = useLoad(async () => {
    const [from, to] = win();
    const w = Math.round(to - from) + 's';
    const [all, entry, failed, p50, p95] = await Promise.all([
      instant(`sum by (service) (increase(traces_spans_total[${w}]))`, to),
      instant(`sum by (service) (increase(traces_spans_total{${ENTRY}}[${w}]))`, to),
      instant(`sum by (service) (increase(traces_spans_total{${ENTRY},status="error"}[${w}]))`, to),
      instant(`histogram_quantile(0.5, sum by (service, le) (increase(traces_span_duration_seconds_bucket{${ENTRY}}[${w}])))`, to),
      instant(`histogram_quantile(0.95, sum by (service, le) (increase(traces_span_duration_seconds_bucket{${ENTRY}}[${w}])))`, to),
    ]);
    const e = by(entry, 'service'), f = by(failed, 'service'), m = by(p50, 'service'), p = by(p95, 'service');
    return all.map(r => r.metric.service).sort().map(name => ({
      name, rate: (e[name] || 0) / (to - from), errors: pct(f[name], e[name]), p50: m[name], p95: p[name], entry: e[name] > 0,
    }));
  }, [range], 30000);
  return html`
    <div class="head"><h1>Services</h1><span class="spacer"></span>
      <${RangePicker} range=${range} setRange=${setRange} />
      <a class="btn" href="/docs/apm" target="_blank" rel="noopener">Help</a></div>
    <${ErrorBox} error=${error} />
    <div class="panel flush">${!data ? html`<div class="empty">Loading…</div>` : data.length === 0
      ? html`<div class="empty">No traces in this time range. Applications instrumented with OpenTelemetry send them to the agent on their host
          (<code>OTEL_EXPORTER_OTLP_ENDPOINT=http://localhost:4318</code>, protocol <code>http/protobuf</code>), once a profile
          sets <code>otlp: {enabled: true}</code>. <code>vigil tracegen</code> sends demo traces to try it. <a href="/docs/apm" target="_blank" rel="noopener">How</a></div>`
      : html`<table>
        <thead><tr><th>Service</th><th style="text-align: right">Requests</th><th style="text-align: right">Errors</th>
          <th style="text-align: right">Median</th><th style="text-align: right">95th percentile</th></tr></thead>
        <tbody>${data.map(s => html`<tr class="click" onClick=${() => { location.hash = '#/services/' + encodeURIComponent(s.name) + '?' + new URLSearchParams(rangeParams(range)); }}>
          <td><strong>${s.name}</strong>${!s.entry && html` <span class="muted">only calls out</span>`}</td>
          <td class="num">${s.entry ? fmtUnit(s.rate, 'ops/s') : '–'}</td>
          <td class="num">${s.entry ? html`<${Badge} kind=${s.errors >= 5 ? 'bad' : s.errors > 0 ? 'warn' : 'ok'}>${s.errors.toFixed(s.errors < 10 ? 1 : 0)}%<//>` : '–'}</td>
          <td class="num">${isFinite(s.p50) ? fmtUnit(s.p50, 'seconds') : '–'}</td>
          <td class="num">${isFinite(s.p95) ? fmtUnit(s.p95, 'seconds') : '–'}</td>
        </tr>`)}</tbody></table>`}</div>
    <p class="muted">Requests are what a service was asked to do: its server and consumer spans. The numbers count every request;
      which traces are stored in full is a separate setting (<code>otlp.traces.sample</code>).</p>`;
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
    const [rate, dur, ops, opErr, opP95, opAvg, out, outErr, outAvg, inn] = await Promise.all([
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
    return { rate, dur, operations, calls, callers };
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
function TraceView({ id, params }) {
  const [open, setOpen] = useState(null);
  const { data: trace, error } = useLoad(async () => {
    if (!/^[0-9a-f]{16,32}$/.test(id)) throw new Error('This is not a trace id.');
    const r = await api('/api/v1/traces/jaeger/api/traces/' + id);
    return (r.data || [])[0] || { spans: [], processes: {} };
  }, [id]);

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
      errors: all.filter(s => s.failed).length, partial: roots.some(r => r.orphan) };
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
