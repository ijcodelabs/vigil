import { html, useState, useEffect, useRef, api, setParams, Tabs, ErrorBox, Dialog, fmtNum, fmtUnit } from './lib.js';
import uPlot from '../vendor/uPlot.esm.js';

export const RANGES = [['5m', 300], ['15m', 900], ['30m', 1800], ['1h', 3600], ['3h', 10800], ['6h', 21600], ['12h', 43200], ['24h', 86400], ['2d', 172800], ['7d', 604800]];
export const REFRESH = [['Off', 0], ['5s', 5], ['10s', 10], ['30s', 30], ['1m', 60]];
export const COLORS = ['#5794f2', '#73bf69', '#f2cc0c', '#ff7383', '#b877d9', '#ff9830', '#4ec9c0', '#e06bb0', '#8ab8ff', '#96d98d', '#ffcb7d', '#c4a2e8'];
const MAX_SERIES = 60;

export function Explore({ route }) {
  const tab = ['logs', 'volume'].includes(route.parts[1]) ? route.parts[1] : 'metrics';
  return html`
    <div class="head"><h1>Explore</h1><span class="spacer"></span>
      <a class="btn" href="/docs/explore" target="_blank" rel="noopener" title="How to use Explore, with examples">Help</a></div>
    <${Tabs} base="explore" current=${tab} tabs=${[['metrics', 'Metrics'], ['logs', 'Logs'], ['volume', 'Log volume']]} />
    ${tab === 'metrics' ? html`<${Metrics} key=${'m' + route.nav} params=${route.params} />`
      : tab === 'volume' ? html`<${Volume} key=${'v' + route.nav} params=${route.params} />`
      : html`<${Logs} key=${'l' + route.nav} params=${route.params} />`}`;
}

// Volume shows who writes the logs: lines and bytes per source in the time
// range, biggest first. For a container, an admin can switch its logs off
// on that agent from here; what is switched off is listed underneath, since
// it no longer shows up in the table.
function Volume({ params }) {
  const [range, setRange, win] = useRange(params);
  const [rows, setRows] = useState(null);
  const [off, setOff] = useState([]);
  const [admin, setAdmin] = useState(false);
  const [error, setError] = useState(null);
  const [busy, setBusy] = useState(false);
  const [confirm, setConfirm] = useState(null); // { agent, container, collect }
  const [note, setNote] = useState(null);

  const run = async () => {
    setBusy(true); setError(null);
    const [from, to] = win();
    setParams(['explore', 'volume'], rangeParams(range));
    try {
      const query = `_time:[${new Date(from * 1000).toISOString()}, ${new Date(to * 1000).toISOString()}] * | stats by (agent, source, container, unit, pod, app) count() as lines, sum_len(_msg) as bytes`;
      const [stats, switched, me] = await Promise.all([
        api('/api/v1/logs/stats_query', { method: 'POST', body: new URLSearchParams({ query, time: to }) }),
        api('/api/v1/fleet/container-logs'), api('/api/v1/me')]);
      const by = new Map();
      for (const s of stats.data.result || []) {
        const { __name__: stat, ...labels } = s.metric;
        const key = JSON.stringify(labels);
        if (!by.has(key)) by.set(key, { ...labels, lines: 0, bytes: 0 });
        by.get(key)[stat] = Number(s.value[1]);
      }
      setRows([...by.values()].sort((a, b) => b.bytes - a.bytes));
      setOff(switched.filter(e => !e.collect));
      setAdmin(me.role === 'admin');
    } catch (e) { setError(e.message); }
    setBusy(false);
  };
  useEffect(() => { run(); }, [JSON.stringify(range)]);

  const apply = async () => {
    const { agent, container, collect } = confirm;
    try {
      await api(`/api/v1/agents/${encodeURIComponent(agent)}/container-logs`, { method: 'POST', body: { container, collect } });
      setNote(collect ? `Logs of ${container} on ${agent} are collected again once the agent has its new settings, within a minute.`
        : `Logs of ${container} on ${agent} stop arriving once the agent has its new settings, within a minute. What is stored stays.`);
      setConfirm(null); run();
    } catch (e) { setError(e.message); setConfirm(null); }
  };

  const total = (rows || []).reduce((a, r) => a + r.bytes, 0);
  const what = r => r.container ? ['container', r.container] : r.pod ? ['pod', r.pod] : r.unit ? ['unit', r.unit] : r.app ? ['app', r.app] : [r.source || 'other', ''];
  const search = r => { const [kind, name] = what(r); return `#/explore/logs?q=${encodeURIComponent(`agent:${JSON.stringify(r.agent)}` + (name ? ` ${kind}:${JSON.stringify(name)}` : ''))}&last=${range.last || 3600}`; };
  return html`
    <div class="panel">
      <${RangeBar} range=${range} setRange=${setRange} onRun=${run} busy=${busy} />
      <${ErrorBox} error=${error} />
      ${note && html`<div class="notice" style="margin-top: 10px">${note}</div>`}
    </div>
    ${rows && html`<div class="panel flush">${rows.length === 0 ? html`<div class="empty">No log lines in this time range.</div>` : html`<table>
      <thead><tr><th>Agent</th><th>From</th><th>Name</th><th style="text-align: right">Lines</th><th style="text-align: right">Size</th>
        <th style="text-align: right">Share</th><th></th></tr></thead>
      <tbody>${rows.slice(0, 200).map(r => { const [kind, name] = what(r); return html`<tr>
        <td>${r.agent}</td><td class="muted">${kind}</td><td class="mono">${name}</td>
        <td class="num mono">${fmtNum(r.lines)}</td><td class="num mono">${fmtUnit(r.bytes, 'bytes')}</td>
        <td class="num mono">${total ? (100 * r.bytes / total).toFixed(1) + '%' : ''}</td>
        <td style="text-align: right; white-space: nowrap"><a href=${search(r)}>show lines</a>
          ${admin && r.container && html`<button class="link" style="margin-left: 12px" title="Stop collecting this container's logs on this agent"
            onClick=${() => setConfirm({ agent: r.agent, container: r.container, collect: false })}>stop collecting</button>`}</td></tr>`; })}</tbody></table>`}
    </div>`}
    ${rows && html`<p class="muted">Sizes are the text of the lines as stored, in the time range above; ${fmtUnit(total, 'bytes')} in all.
      Stopping a container here writes <code>docker.container_logs</code> into the profile of that one agent (<code>agent-NAME</code>); a pattern for several containers or agents goes into a profile by hand.</p>`}
    ${off.length > 0 && html`<h3 style="margin: 18px 0 8px">Switched off</h3><div class="panel flush"><table>
      <thead><tr><th>Container</th><th>By profile</th><th>For agents</th><th></th></tr></thead>
      <tbody>${off.map(e => { const agent = e.profile.startsWith('agent-') && (e.select[0] || '').match(/^agent="(.*)"$/); return html`<tr>
        <td class="mono">${e.container}</td><td><a href="#/fleet/profiles">${e.profile}</a></td><td class="mono">${e.select.join(', ') || 'all'}</td>
        <td style="text-align: right">${admin && agent && html`<button class="link" onClick=${() => setConfirm({ agent: agent[1], container: e.container, collect: true })}>collect again</button>`}</td></tr>`; })}</tbody></table></div>`}
    ${confirm && html`<${Dialog} title=${confirm.collect ? 'Collect logs again' : 'Stop collecting logs'} onClose=${() => setConfirm(null)}>
      <p>${confirm.collect ? 'Collect' : 'Stop collecting'} the logs of container <strong>${confirm.container}</strong> on agent <strong>${confirm.agent}</strong>${confirm.collect ? ' again' : ''}?</p>
      <p class="muted">${confirm.collect ? 'This takes the entry out of' : 'This adds an entry to'} the profile <code>agent-${confirm.agent.toLowerCase()}</code>:</p>
      <pre class="mono" style="margin: 8px 0">config:\n  docker:\n    container_logs:\n      "${confirm.container}": false</pre>
      <p class="muted">Metrics of the container are not affected. If your profiles come from files in git, make the same change there, or the next <code>vigil apply -prune</code> removes this profile.</p>
      <div class="actions"><button type="button" onClick=${() => setConfirm(null)}>Cancel</button>
        <button class="primary" onClick=${apply}>${confirm.collect ? 'Collect again' : 'Stop collecting'}</button></div>
    <//>`}`;
}

// A range is either the last N seconds or a fixed window picked by dragging
// on the chart.
export function useRange(params) {
  const init = params.get('from') && params.get('to')
    ? { from: +params.get('from'), to: +params.get('to') }
    : { last: +(params.get('last') || 3600) };
  const [range, setRange] = useState(init);
  const window_ = () => {
    if (range.last) { const to = Date.now() / 1000; return [to - range.last, to]; }
    return [range.from, range.to];
  };
  return [range, setRange, window_];
}
export const rangeParams = r => (r.last ? { last: r.last } : { from: Math.round(r.from), to: Math.round(r.to) });

function RangeBar({ range, setRange, refresh, setRefresh, onRun, busy, children }) {
  return html`<div class="row" style="margin-top: 10px">
    ${range.last ? html`<select aria-label="Time range" value=${range.last} onChange=${e => setRange({ last: +e.target.value })}>
        ${RANGES.map(([l, s]) => html`<option value=${s}>Last ${l}</option>`)}</select>`
      : html`<span class="badge info">${new Date(range.from * 1000).toLocaleString()} – ${new Date(range.to * 1000).toLocaleTimeString()}</span>
        <button class="small" onClick=${() => setRange({ last: 3600 })}>Back to last 1h</button>`}
    ${setRefresh && html`<select aria-label="Refresh" value=${refresh} onChange=${e => setRefresh(+e.target.value)} disabled=${!range.last}>
      ${REFRESH.map(([l, s]) => html`<option value=${s}>Refresh: ${l}</option>`)}</select>`}
    ${children}
    <span class="spacer"></span>
    <button class="primary" onClick=${onRun} disabled=${busy}>${busy ? 'Running…' : 'Run query'}</button>
  </div>`;
}

function QueryBox({ value, onChange, onRun, placeholder, label }) {
  const key = e => { if (e.key === 'Enter' && !e.shiftKey) { e.preventDefault(); onRun(); } };
  return html`<textarea rows="1" spellcheck="false" placeholder=${placeholder} aria-label=${label} value=${value}
    onInput=${e => onChange(e.target.value)} onKeyDown=${key}></textarea>`;
}

export function seriesName(metric) {
  const { __name__: name = '', ...rest } = metric;
  const labels = Object.entries(rest).sort().map(([k, v]) => `${k}="${v}"`).join(', ');
  return labels ? `${name}{${labels}}` : name || '{}';
}

function Metrics({ params }) {
  const [query, setQuery] = useState(params.get('q') || '');
  const [range, setRange, win] = useRange(params);
  const [refresh, setRefresh] = useState(0);
  const [result, setResult] = useState(null);
  const [error, setError] = useState(null);
  const [busy, setBusy] = useState(false);
  const [browsing, setBrowsing] = useState(false);
  const ran = useRef('');

  const run = async (q = query) => {
    q = q.trim();
    if (!q) return;
    ran.current = q;
    setBusy(true);
    const [from, to] = win();
    // About 300 points across the window, never finer than 5 seconds.
    const step = Math.max(5, Math.ceil((to - from) / 300));
    setParams(['explore', 'metrics'], { q, ...rangeParams(range) });
    try {
      const r = await api('/api/v1/query_range', { method: 'POST', body: new URLSearchParams({ query: q, start: from, end: to, step }) });
      setResult({ series: r.data.result, from, to, step });
      setError(null);
    } catch (e) { setError(e.message); }
    setBusy(false);
  };
  useEffect(() => { if (ran.current || query) run(ran.current || query); }, [range]);
  useEffect(() => {
    if (!refresh || !range.last) return;
    const t = setInterval(() => ran.current && run(ran.current), refresh * 1000);
    return () => clearInterval(t);
  }, [refresh, range]);

  const shown = result ? result.series.slice(0, MAX_SERIES) : [];
  return html`
    <div class="panel">
      <${QueryBox} value=${query} onChange=${setQuery} onRun=${() => run()} label="PromQL query"
        placeholder=${'PromQL, e.g. rate(host_cpu_seconds_total{mode!="idle"}[5m])  ·  Enter runs it'} />
      <${RangeBar} range=${range} setRange=${setRange} refresh=${refresh} setRefresh=${setRefresh} onRun=${() => run()} busy=${busy}>
        <button onClick=${() => setBrowsing(true)} title="List every metric that has data in this time range">Show metrics</button>
      <//>
      <${ErrorBox} error=${error} />
    </div>
    ${browsing && html`<${MetricBrowser} win=${win} onClose=${() => setBrowsing(false)}
      onPick=${name => { setBrowsing(false); setQuery(name); run(name); }} />`}
    ${result && (result.series.length === 0
      ? html`<div class="panel empty">The query returned no data for this time range.</div>`
      : html`
        ${result.series.length > MAX_SERIES && html`<div class="notice">Showing the first ${MAX_SERIES} of ${result.series.length} series. Narrow the query to see the rest.</div>`}
        <div class="panel"><${Chart} result=${{ ...result, series: shown }} onZoom=${(from, to) => setRange({ from, to })} /></div>
        <div class="panel flush"><table>
          <thead><tr><th>Series (${result.series.length})</th><th style="text-align: right">Last value</th></tr></thead>
          <tbody>${shown.map((s, i) => html`<tr>
            <td class="mono"><span class="dot" style="background: ${COLORS[i % COLORS.length]}"></span>${seriesName(s.metric)}</td>
            <td class="num mono">${fmtNum(s.values[s.values.length - 1][1])}</td></tr>`)}</tbody>
        </table></div>`)}
    ${!result && !error && html`<div class="panel empty">Enter a PromQL query to graph metrics. Drag across the chart to zoom into a time window.</div>`}`;
}

// MetricBrowser lists every metric that has data in the current time range.
// Picking one graphs it; the labels on its series then show in the table,
// which is the usual way to find out what can be filtered on.
function MetricBrowser({ win, onClose, onPick }) {
  const [names, setNames] = useState(null);
  const [error, setError] = useState(null);
  const [filter, setFilter] = useState('');
  useEffect(() => {
    const [from, to] = win();
    api(`/api/v1/label/__name__/values?start=${Math.floor(from)}&end=${Math.ceil(to)}`)
      .then(r => setNames(r.data || []), e => setError(e.message));
  }, []);
  const needle = filter.trim().toLowerCase();
  const shown = (names || []).filter(n => !needle || n.toLowerCase().includes(needle));
  // Group by the part before the first underscore: host_, container_, kube_, ...
  const groups = {};
  for (const n of shown) (groups[n.split('_')[0]] ||= []).push(n);
  return html`<${Dialog} title="Collected metrics" wide onClose=${onClose}>
    <div class="row" style="margin-bottom: 10px">
      <input style="flex: 1" autofocus placeholder="Filter, e.g. memory, kube_pod, probe" aria-label="Filter metrics" value=${filter} onInput=${e => setFilter(e.target.value)} />
      <span class="muted">${names ? `${shown.length} of ${names.length} metrics with data in this time range` : 'Loading…'}</span>
    </div>
    <${ErrorBox} error=${error} />
    <div class="metric-list">${Object.keys(groups).sort().map(g => html`
      <div class="metric-group"><div class="muted">${g}</div>
        ${groups[g].map(n => html`<button class="link mono" onClick=${() => onPick(n)}>${n}</button>`)}</div>`)}
      ${names && shown.length === 0 && html`<div class="empty">No metric matches.</div>`}</div>
    <div class="actions"><a href="/docs/metrics" target="_blank" rel="noopener">What do these mean?</a><span class="spacer"></span>
      <button class="primary" onClick=${onClose}>Close</button></div>
  <//>`;
}

// Chart draws series over time. result is {series, from, to, step} with
// series in the Prometheus matrix shape; names, if given, label them.
// Options: height, unit, min (bottom of the y axis), fill, legend (false
// hides it), sync (charts sharing a key share their cursor).
export function Chart({ result, onZoom, names, height = 300, unit = '', min, fill, legend, sync }) {
  const el = useRef(null);
  useEffect(() => {
    const { series, from, to, step } = result;
    // One shared time axis; a series without a sample at a step gets a gap.
    const xs = [];
    const t0 = Math.floor(from / step) * step;
    for (let t = t0; t <= to; t += step) xs.push(t);
    const index = new Map(xs.map((t, i) => [t, i]));
    const data = [xs, ...series.map(s => {
      const ys = new Array(xs.length).fill(null);
      for (const [t, v] of s.values) {
        const i = index.get(Math.round(t / step) * step);
        if (i !== undefined) ys[i] = Number(v);
      }
      return ys;
    })];
    const css = getComputedStyle(document.documentElement);
    const axis = { stroke: css.getPropertyValue('--muted'), grid: { stroke: css.getPropertyValue('--border'), width: 1 }, ticks: { stroke: css.getPropertyValue('--border') } };
    const fmt = v => (unit ? fmtUnit(v, unit) : fmtNum(v));
    const yRange = (u, lo, hi) => {
      if (lo == null || hi == null) return [min ?? 0, 1];
      const [a, b] = uPlot.rangeNum(min != null ? Math.min(min, lo) : lo, hi, 0.1, true);
      return [min != null ? Math.min(min, lo) : a, b === a ? a + 1 : b];
    };
    const plot = new uPlot({
      width: el.current.clientWidth, height,
      scales: { x: { time: true }, y: { range: yRange } },
      axes: [axis, { ...axis, size: 68, values: (_, ticks) => ticks.map(fmt) }],
      series: [{}, ...series.map((s, i) => ({
        label: names ? names[i] : seriesName(s.metric), stroke: COLORS[i % COLORS.length], width: 1.5, spanGaps: false,
        fill: fill ? COLORS[i % COLORS.length] + '2e' : undefined,
        points: { show: false }, value: (_, v) => (v == null ? '–' : fmt(v)),
      }))],
      legend: { show: legend ?? series.length <= 12 },
      cursor: { drag: { x: true, y: false, setScale: false }, ...(sync ? { sync: { key: sync } } : {}) },
      hooks: {
        setSelect: [u => {
          if (u.select.width < 4) return;
          const a = u.posToVal(u.select.left, 'x'), b = u.posToVal(u.select.left + u.select.width, 'x');
          u.setSelect({ left: 0, width: 0, top: 0, height: 0 }, false);
          onZoom && onZoom(a, b);
        }],
      },
    }, data, el.current);
    const ro = new ResizeObserver(() => plot.setSize({ width: el.current.clientWidth, height }));
    ro.observe(el.current);
    const retheme = () => plot.redraw(false);
    addEventListener('themechange', retheme);
    return () => { ro.disconnect(); removeEventListener('themechange', retheme); plot.destroy(); };
  }, [result, height, unit, min, fill]);
  return html`<div class="chart" ref=${el}></div>`;
}

// Logs

const LIMITS = [100, 500, 1000, 5000];
const HIDDEN = new Set(['_msg', '_time', '_stream', '_stream_id']);

export function parseLines(text) {
  return text.split('\n').filter(Boolean).map(l => { try { return JSON.parse(l); } catch { return null; } }).filter(Boolean);
}

function Logs({ params }) {
  const [query, setQuery] = useState(params.get('q') || '*');
  const [range, setRange, win] = useRange(params);
  const [limit, setLimit] = useState(500);
  const [rows, setRows] = useState(null);
  const [hits, setHits] = useState(null);
  const [error, setError] = useState(null);
  const [busy, setBusy] = useState(false);
  const [live, setLive] = useState(false);
  const tail = useRef(null);

  const stopTail = () => { tail.current?.abort(); tail.current = null; setLive(false); };
  useEffect(() => stopTail, []);

  const run = async (q = query) => {
    q = q.trim() || '*';
    stopTail();
    setBusy(true);
    const [from, to] = win();
    setParams(['explore', 'logs'], { q, ...rangeParams(range) });
    const form = extra => new URLSearchParams({ query: q, start: from, end: to, ...extra });
    try {
      const [text, h] = await Promise.all([
        api('/api/v1/logs/query', { method: 'POST', body: form({ limit }) }),
        api('/api/v1/logs/hits', { method: 'POST', body: form({ step: Math.max(1, Math.ceil((to - from) / 80)) + 's' }) }),
      ]);
      const list = parseLines(text);
      list.sort((a, b) => (a._time < b._time ? 1 : a._time > b._time ? -1 : 0));
      setRows(list);
      setHits(mergeHits(h));
      setError(null);
    } catch (e) { setError(e.message); }
    setBusy(false);
  };
  useEffect(() => { run(); }, [range, limit]);

  // Live tail: the server streams matching lines as they arrive.
  const startTail = async () => {
    const q = query.trim() || '*';
    const ctl = new AbortController();
    tail.current = ctl;
    setLive(true); setRows([]); setHits(null); setError(null);
    try {
      const res = await fetch('/api/v1/logs/tail', { method: 'POST', body: new URLSearchParams({ query: q }), headers: { 'X-Vigil-CSRF': '1' }, signal: ctl.signal });
      if (!res.ok) throw new Error((await res.text()) || `tail failed (${res.status})`);
      const reader = res.body.getReader();
      const dec = new TextDecoder();
      let buf = '';
      for (;;) {
        const { done, value } = await reader.read();
        if (done) break;
        buf += dec.decode(value, { stream: true });
        const cut = buf.lastIndexOf('\n');
        if (cut < 0) continue;
        const fresh = parseLines(buf.slice(0, cut)).reverse();
        buf = buf.slice(cut + 1);
        if (fresh.length) setRows(old => [...fresh, ...(old || [])].slice(0, 1000));
      }
    } catch (e) { if (e.name !== 'AbortError') setError(e.message); }
    if (tail.current === ctl) stopTail();
  };

  // "filter" narrows the query to lines with this value, "exclude" to lines
  // without it. Either one replaces the other for the same value, so a
  // click can be taken back with the opposite click.
  const addFilter = (k, v, exclude) => {
    const term = `${k}:${JSON.stringify(v)}`;
    const rest = (' ' + query.trim() + ' ').split(` -${term} `).join(' ').split(` ${term} `).join(' ').trim();
    const q = exclude ? `${rest || '*'} -${term}` : `${rest === '*' ? '' : rest + ' '}${term}`.trim();
    setQuery(q); run(q);
  };

  return html`
    <div class="panel">
      <${QueryBox} value=${query} onChange=${setQuery} onRun=${() => run()} label="LogsQL query"
        placeholder="LogsQL, e.g. error app:nginx  ·  Enter runs it" />
      <${RangeBar} range=${range} setRange=${setRange} onRun=${() => run()} busy=${busy}>
        <select aria-label="Line limit" value=${limit} onChange=${e => setLimit(+e.target.value)}>
          ${LIMITS.map(n => html`<option value=${n}>Up to ${n} lines</option>`)}</select>
        ${live ? html`<button onClick=${stopTail}>■ Stop live tail</button>` : html`<button onClick=${startTail}>▶ Live tail</button>`}
      <//>
      <${ErrorBox} error=${error} />
    </div>
    ${hits && hits.total > 0 && html`<div class="panel">
      <div class="hits" aria-label="Log volume over time">${hits.values.map((v, i) =>
        html`<div style="height: ${Math.max(2, (100 * v) / hits.max)}%" title="${new Date(hits.times[i]).toLocaleString()}: ${v} lines"></div>`)}</div>
      <div class="row muted" style="font-size: 12px"><span>${new Date(hits.times[0]).toLocaleString()}</span><span class="spacer"></span>
        <span>${fmtNum(hits.total)} lines match</span><span class="spacer"></span><span>${new Date(hits.times[hits.times.length - 1]).toLocaleString()}</span></div>
    </div>`}
    ${rows && html`<div class="panel flush logs">
      ${live && html`<div class="empty" style="padding: 8px">Live: new lines appear at the top.</div>`}
      ${rows.length === 0 && !live && html`<div class="empty">No log lines match in this time range.</div>`}
      ${rows.map(r => html`<${LogRow} key=${r._stream_id + r._time + r._msg} row=${r} onFilter=${addFilter} />`)}
      ${!live && rows.length >= limit && html`<div class="empty" style="padding: 10px">Showing the newest ${limit} lines. Narrow the query or raise the limit to see more.</div>`}
    </div>`}`;
}

// The hits API answers one series per group; the bar chart wants the total.
function mergeHits(h) {
  const sum = new Map();
  for (const s of h.hits || []) s.timestamps.forEach((t, i) => sum.set(t, (sum.get(t) || 0) + s.values[i]));
  const times = [...sum.keys()].sort();
  const values = times.map(t => sum.get(t));
  return { times, values, max: Math.max(1, ...values), total: values.reduce((a, b) => a + b, 0) };
}

function LogRow({ row, onFilter }) {
  const [open, setOpen] = useState(false);
  const level = String(row.level || '').toLowerCase();
  const cls = /^(err|crit|alert|emerg|fatal)/.test(level) ? 'lvl-error' : /^warn/.test(level) ? 'lvl-warn' : '';
  const src = row.container || row.unit || row.app || row.pod || (row.path || '').split('/').pop() || row.source || '';
  const t = new Date(row._time);
  return html`<div class="log ${cls} ${open ? 'open' : ''}" onClick=${() => !getSelection().toString() && setOpen(!open)}>
    <div class="t" title=${row._time}>${t.toLocaleDateString(undefined, { month: 'short', day: '2-digit' })} ${t.toLocaleTimeString(undefined, { hour12: false })}.${String(t.getMilliseconds()).padStart(3, '0')}</div>
    <div><div class="m">${row._msg}</div><div class="src">${row.agent}${src && ' · ' + src}${row.level ? ' · ' + row.level : ''}</div></div>
    ${open && html`<div class="logfields" onClick=${e => e.stopPropagation()}><table>
      ${Object.entries(row).filter(([k]) => !HIDDEN.has(k)).sort().map(([k, v]) => html`<tr>
        <td>${k}</td><td>${k === 'trace_id' && /^[0-9a-f]{16,32}$/.test(v)
          ? html`<a href=${`#/services/~trace/${v}?t=${Math.round(t.getTime() / 1000)}`} title="Open the trace this line belongs to">${v}</a>` : v}</td>
        <td style="white-space: nowrap"><button class="link" title="Show only lines with this value" onClick=${() => onFilter(k, v)}>filter</button>
          <button class="link" style="margin-left: 10px" title="Hide lines with this value" onClick=${() => onFilter(k, v, true)}>exclude</button></td></tr>`)}
    </table></div>`}
  </div>`;
}
