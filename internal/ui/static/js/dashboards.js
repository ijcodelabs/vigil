import { html, useState, useEffect, useRef, useErrorBoundary, api, useLoad, setParams, ErrorBox, Badge, Dialog, ago, fmtUnit } from './lib.js';
import { Chart, seriesName, useRange, rangeParams, REFRESH, parseLines } from './explore.js';
import { TimeRange } from './timerange.js';
import { MapView } from './map.js';

const UNITS = [['', 'plain number'], ['percent', 'percent (0–100)'], ['ratio', 'ratio (0–1 shown as %)'], ['bytes', 'bytes'],
  ['bytes/s', 'bytes per second'], ['seconds', 'seconds'], ['ms', 'milliseconds'], ['cores', 'CPU cores'], ['ops/s', 'per second'], ['yesno', 'yes / no (1 / 0)']];
const TYPES = [['timeseries', 'Lines over time'], ['stat', 'Single number'], ['table', 'Table'], ['logs', 'Log lines']];
export const ROW = 84; // pixels per unit of panel height

export function Dashboards({ route, admin }) {
  return route.parts[1]
    ? html`<${DashboardView} key=${route.parts[1] + route.nav} id=${route.parts[1]} params=${route.params} admin=${admin} />`
    : html`<${DashboardList} admin=${admin} />`;
}

export const slug = title => title.toLowerCase().replace(/[^a-z0-9]+/g, '-').replace(/^-+|-+$/g, '').slice(0, 60) || 'dashboard';

function DashboardList({ admin }) {
  const { data, error } = useLoad(() => api('/api/v1/dashboards'));
  const [adding, setAdding] = useState(false);
  const [formError, setFormError] = useState(null);
  const create = async e => {
    e.preventDefault();
    const title = new FormData(e.target).get('title').trim();
    let id = slug(title);
    if (data.some(d => d.id === id)) id += '-' + Math.random().toString(36).slice(2, 6);
    try {
      await api('/api/v1/dashboards/' + id, { method: 'PUT', body: { title, panels: [] } });
      location.hash = `#/dashboards/${id}?edit=1`;
    } catch (err) { setFormError(err.message); }
  };
  return html`
    <div class="head"><h1>Dashboards</h1><span class="spacer"></span>
      <a class="btn" href="/docs/dashboards" target="_blank" rel="noopener">Help</a>
      ${admin && html`<button class="primary" onClick=${() => setAdding(true)}>New dashboard</button>`}</div>
    <${ErrorBox} error=${error} />
    <div class="panel flush">${!data ? html`<div class="empty">Loading…</div>` : data.length === 0
      ? html`<div class="empty">No dashboards.${admin && ' Use “New dashboard” to make one.'}</div>`
      : html`<table><thead><tr><th>Dashboard</th><th>Changed</th></tr></thead>
        <tbody>${data.map(d => html`<tr class="click" onClick=${() => { location.hash = '#/dashboards/' + d.id; }}>
          <td><strong>${d.title}</strong>
            ${d.source && html` <span class="muted" title="Comes with vigil; it is yours to change or delete">· ${d.source}</span>`}
            ${d.update && html` <${Badge} kind="info">newer version available<//>`}</td>
          <td class="muted">${ago(d.updated)}</td></tr>`)}</tbody></table>`}</div>
    ${adding && html`<${Dialog} title="New dashboard" onClose=${() => setAdding(false)}><form onSubmit=${create}>
      <div class="field"><label>Title</label><input name="title" required autofocus maxlength="100" /></div>
      <${ErrorBox} error=${formError} />
      <div class="actions"><button type="button" onClick=${() => setAdding(false)}>Cancel</button><button class="primary">Create</button></div>
    </form><//>`}`;
}

// $name in a query becomes the chosen value, or a match-everything pattern
// when "All" is chosen, so queries write  agent=~"$agent".
function fill(expr, variables, values) {
  return expr.replace(/\$([a-z][a-z0-9_]*)/g, (m, name) =>
    variables.some(v => v.name === name) ? (values[name] || '.*') : m);
}

function DashboardView({ id, params, admin }) {
  const [dash, setDash] = useState(null);
  const [error, setError] = useState(null);
  const [draft, setDraft] = useState(null); // a copy being edited
  const [range, setRange, win] = useRange(params);
  const [refresh, setRefresh] = useState(+(params.get('refresh') || 30));
  const [values, setValues] = useState(() => Object.fromEntries([...params].filter(([k]) => k.startsWith('var-')).map(([k, v]) => [k.slice(4), v])));
  const [tick, setTick] = useState(0);
  const [dialog, setDialog] = useState(null);
  const [saveError, setSaveError] = useState(null);
  const grid = useRef(null);
  const [resize, setResize] = useState(null); // {i, w, h} while a panel's corner is being dragged
  const [drag, setDrag] = useState(null); // {from, to, after} while a panel is being moved
  const moving = useRef(null);

  const [offer, setOffer] = useState(null); // a newer version that comes with vigil
  const load = () => api('/api/v1/dashboards/' + id).then(d => {
    setDash(d);
    if (params.get('edit') && admin) setDraft(structuredClone(d));
  }, e => setError(e.message));
  useEffect(() => {
    load();
    api(`/api/v1/dashboards/${id}/update`).then(o => setOffer(o && o.panels ? o : null), () => {}); // empty: nothing on offer
  }, []);
  const decide = (action, copy) => async () => {
    try {
      await api(`/api/v1/dashboards/${id}/update`, { method: 'POST', body: { action, copy } });
      setOffer(null);
      if (action === 'apply') load();
    } catch (e) { setSaveError(e.message); }
  };
  useEffect(() => {
    if (!refresh || !range.last || draft) return;
    const t = setInterval(() => setTick(n => n + 1), refresh * 1000);
    return () => clearInterval(t);
  }, [refresh, range, !!draft]);
  useEffect(() => {
    const vars = Object.fromEntries(Object.entries(values).filter(([, v]) => v).map(([k, v]) => ['var-' + k, v]));
    setParams(['dashboards', id], { ...rangeParams(range), ...(refresh !== 30 ? { refresh } : {}), ...vars });
  }, [range, refresh, values]);

  if (error) return html`<div class="head"><h1>Dashboard</h1></div><${ErrorBox} error=${error} /><a href="#/dashboards">Back to dashboards</a>`;
  if (!dash) return html`<div class="empty">Loading…</div>`;

  const shown = draft || dash;
  const variables = shown.variables || [];
  const [from, to] = win();
  const change = fn => { const next = structuredClone(draft); fn(next); setDraft(next); };
  const save = async () => {
    try {
      await api('/api/v1/dashboards/' + id, { method: 'PUT', body: draft });
      setDash(draft); setDraft(null); setSaveError(null); setTick(n => n + 1);
    } catch (e) { setSaveError(e.message); }
  };
  const remove = async () => {
    if (!confirm(`Delete the dashboard “${dash.title}”? This cannot be undone.`)) return;
    try { await api('/api/v1/dashboards/' + id, { method: 'DELETE' }); location.hash = '#/dashboards'; } catch (e) { alert(e.message); }
  };
  const move = (i, by) => change(d => { const j = i + by; if (j < 0 || j >= d.panels.length) return; [d.panels[i], d.panels[j]] = [d.panels[j], d.panels[i]]; });

  // Dragging a panel's corner resizes it. While the pointer is down only
  // the tile's box follows, in whole columns and rows; the panel itself is
  // changed once, on release, so its queries do not run for every step.
  const startResize = (e, i) => {
    e.preventDefault();
    const p = draft.panels[i], gap = 12;
    const col = (grid.current.clientWidth - 11 * gap) / 12 + gap;
    const x0 = e.clientX, y0 = e.clientY;
    const clamp = n => Math.max(1, Math.min(12, n));
    let last = { i, w: p.w, h: p.h };
    const follow = ev => {
      const w = clamp(Math.round((p.w * col + ev.clientX - x0) / col)), h = clamp(Math.round((p.h * ROW + ev.clientY - y0) / ROW));
      if (w !== last.w || h !== last.h) setResize(last = { i, w, h });
    };
    const done = () => {
      removeEventListener('pointermove', follow); removeEventListener('pointerup', done); removeEventListener('pointercancel', done);
      setResize(null);
      if (last.w !== p.w || last.h !== p.h) change(d => { d.panels[i].w = last.w; d.panels[i].h = last.h; });
    };
    addEventListener('pointermove', follow); addEventListener('pointerup', done); addEventListener('pointercancel', done);
    setResize(last);
  };
  // Dragging a panel by its title moves it. Panels flow in order, left to
  // right and then down, so moving means taking another place in that
  // order: before or after the panel it is dropped on.
  // The move is tracked in a ref as well as in state: the browser asks
  // "may it be dropped here" right after the drag starts, before the page
  // has drawn again, and an answer from the old state would be no.
  const track = d => { moving.current = d; setDrag(d); };
  const dragOver = (e, i) => {
    const m = moving.current;
    if (!m) return;
    e.preventDefault();
    e.dataTransfer.dropEffect = 'move';
    const r = e.currentTarget.getBoundingClientRect(), after = e.clientX > r.left + r.width / 2;
    if (m.to !== i || m.after !== after) track({ ...m, to: i, after });
  };
  // The panels are put in their new order when the drag ends, not on the
  // drop itself: reordering redraws the dragged panel, and a drag whose
  // source disappears under it never gets its end.
  const drop = e => {
    if (!moving.current) return;
    e.preventDefault();
    moving.current.dropped = true;
  };
  const dragEnd = () => {
    const m = moving.current;
    track(null);
    if (!m || !m.dropped) return;
    const { from, to, after } = m;
    const at = (to > from ? to - 1 : to) + (after ? 1 : 0);
    if (at !== from) change(d => { const [p] = d.panels.splice(from, 1); d.panels.splice(at, 0, p); });
  };

  return html`
    <div class="head"><a href="#/dashboards">← Dashboards</a><h1>${shown.title}</h1>
      ${variables.map(v => html`<${VariablePicker} key=${v.name} variable=${v} from=${from} to=${to}
        value=${values[v.name] || ''} onChange=${val => setValues({ ...values, [v.name]: val })} />`)}
      <span class="spacer"></span>
      <${TimeRange} range=${range} setRange=${setRange} />
      <select aria-label="Refresh" value=${refresh} onChange=${e => setRefresh(+e.target.value)} disabled=${!range.last || !!draft}>
        ${REFRESH.map(([l, s]) => html`<option value=${s}>Refresh: ${l}</option>`)}</select>
      ${admin && !draft && html`<button onClick=${() => setDraft(structuredClone(dash))}>Edit</button>`}
    </div>
    ${offer && !draft && html`<div class="notice row">
      <span><strong>A newer version of this dashboard comes with this vigil</strong>
        ${' '}(${offer.panels.length} panels; yours has ${shown.panels.length}). Yours was changed, so it was left as it is.</span>
      <span class="spacer"></span>
      ${admin ? html`
        <button onClick=${decide('keep')} title="Do not offer this version again">Keep mine</button>
        <button onClick=${decide('apply', true)} title="Yours is kept as a second dashboard, “${shown.title} (mine)”">Update, keep mine as a copy</button>
        <button class="primary" onClick=${() => confirm('Replace your version of this dashboard with the newer one?') && decide('apply', false)()}>Update</button>`
        : html`<span class="muted">An admin can update it.</span>`}
    </div>`}
    ${draft && html`<div class="notice row">
      <span><strong>Editing.</strong> Drag a panel by its title to move it, by its corner to resize it. Nothing is saved until you press Save.</span>
      <span class="spacer"></span>
      <button onClick=${() => setDialog({ kind: 'panel', index: -1 })}>Add panel</button>
      <button onClick=${() => setDialog({ kind: 'settings' })}>Title, variables, JSON</button>
      <button class="danger" onClick=${remove}>Delete dashboard</button>
      <button onClick=${() => { setDraft(null); setSaveError(null); }}>Discard changes</button>
      <button class="primary" onClick=${save}>Save</button>
    </div>`}
    <${ErrorBox} error=${saveError} />
    ${shown.panels.length === 0 && html`<div class="panel empty">This dashboard has no panels yet.${draft ? ' Use “Add panel”.' : admin ? ' Press Edit to add some.' : ''}</div>`}
    <div class="grid ${drag ? 'moving' : ''}" ref=${grid}>${shown.panels.map((p, i) => { const size = resize && resize.i === i ? resize : p; return html`
      <section key=${i + JSON.stringify(p)} style="grid-column: span ${size.w}; height: ${size.h * ROW}px"
        class="panel tile ${draft ? 'editing' : ''} ${drag && drag.to === i && drag.from !== i ? (drag.after ? 'drop-after' : 'drop-before') : ''}"
        onDragOver=${e => dragOver(e, i)} onDrop=${drop}>
        <header draggable=${!!draft} onDragEnd=${dragEnd} onDragStart=${e => {
          e.dataTransfer.effectAllowed = 'move'; e.dataTransfer.setData('text/plain', p.title);
          e.dataTransfer.setDragImage(e.currentTarget.parentNode, 24, 16); track({ from: i, to: i, after: false }); }}>
          <h2>${p.title}</h2><span class="spacer"></span>
          ${draft && html`<button class="small" title="Move earlier" onClick=${() => move(i, -1)}>◀</button>
            <button class="small" title="Move later" onClick=${() => move(i, 1)}>▶</button>
            <button class="small" onClick=${() => setDialog({ kind: 'panel', index: i })}>Edit</button>
            <button class="small danger" title="Remove panel" onClick=${() => change(d => d.panels.splice(i, 1))}>✕</button>`}
        </header>
        <${PanelBody} key=${JSON.stringify(p)} admin=${admin} panel=${p} from=${from} to=${to} tick=${tick} variables=${variables} values=${values}
          height=${p.h * ROW - 58} onZoom=${(a, b) => setRange({ from: a, to: b })} />
        ${draft && html`<div class="resize" title="Drag to resize" onPointerDown=${e => startResize(e, i)}></div>`}
        ${size !== p && html`<div class="size">${size.w} of 12 wide · ${size.h} high</div>`}
      </section>`; })}</div>
    ${dialog?.kind === 'panel' && html`<${PanelEditor}
      panel=${dialog.index < 0 ? { title: '', type: 'timeseries', w: 6, h: 3, queries: [{ expr: '' }] } : draft.panels[dialog.index]}
      onClose=${() => setDialog(null)}
      onSave=${p => { change(d => { dialog.index < 0 ? d.panels.push(p) : (d.panels[dialog.index] = p); }); setDialog(null); }} />`}
    ${dialog?.kind === 'settings' && html`<${SettingsEditor} dash=${draft} onClose=${() => setDialog(null)}
      onSave=${d => { setDraft(d); setDialog(null); }} />`}`;
}

function VariablePicker({ variable, from, to, value, onChange }) {
  const { data } = useLoad(async () => {
    const q = new URLSearchParams({ start: Math.floor(from), end: Math.ceil(to) });
    if (variable.match) q.set('match[]', variable.match);
    return (await api(`/api/v1/label/${encodeURIComponent(variable.label)}/values?${q}`)).data || [];
  }, [variable.label, variable.match]);
  const options = data || [];
  return html`<label style="margin: 0" class="row">${variable.name}
    <select value=${value} onChange=${e => onChange(e.target.value)} aria-label=${variable.name}>
      <option value="">All</option>
      ${value && !options.includes(value) && html`<option value=${value}>${value}</option>`}
      ${options.map(o => html`<option value=${o}>${o}</option>`)}</select></label>`;
}

const legendFor = (q, metric) => q.legend
  ? q.legend.replace(/\{\{\s*([A-Za-z_][A-Za-z0-9_]*)\s*\}\}/g, (_, k) => metric[k] ?? '')
  : seriesName(metric);
const iso = t => new Date(t * 1000).toISOString();

// run fetches what a panel shows for the window [from, to].
async function run(panel, from, to, variables, values, width) {
  const logs = panel.source === 'logs' || panel.type === 'logs';
  // $__range is the length of the shown time, for "in the time shown" numbers:
  // increase(errors_total[$__range]); $__range_s is the same as a number of seconds.
  const span = Math.max(60, Math.round(to - from)) + 's';
  const queries = panel.queries.map(q => ({ ...q, expr: fill(q.expr, variables, values).replaceAll('$__range_s', String(parseInt(span))).replaceAll('$__range', span) }));
  const form = o => ({ method: 'POST', body: new URLSearchParams(o) });
  if (panel.type === 'logs') {
    const text = await api('/api/v1/logs/query', form({ query: queries[0].expr, start: from, end: to, limit: panel.limit || 50 }));
    return { rows: parseLines(text).sort((a, b) => (a._time < b._time ? 1 : -1)) };
  }
  if (panel.type === 'timeseries') {
    const step = Math.max(logs ? 10 : 5, Math.ceil((to - from) / Math.max(60, Math.min(400, width / 3))));
    const series = [], names = [];
    for (const q of queries) {
      const r = logs
        ? await api('/api/v1/logs/stats_query_range', form({ query: q.expr, start: from, end: to, step: step + 's' }))
        : await api('/api/v1/query_range', form({ query: q.expr, start: from, end: to, step }));
      for (const s of r.data.result) { series.push(s); names.push(legendFor(q, s.metric)); }
    }
    return { series: series.slice(0, 40), names: names.slice(0, 40), total: series.length, from, to, step };
  }
  // stat and table: one value per series, at the end of the window
  const rows = [];
  for (const q of queries) {
    const r = logs
      ? await api('/api/v1/logs/stats_query', form({ query: `_time:[${iso(from)}, ${iso(to)}] ${q.expr}`, time: to }))
      : await api('/api/v1/query', form({ query: q.expr, time: to }));
    for (const s of r.data.result || []) rows.push({ metric: s.metric, value: Number(s.value[1]), name: legendFor(q, s.metric), q: queries.indexOf(q) });
  }
  return { values: rows };
}

export function PanelBody({ panel, admin, from, to, tick, variables, values, height, onZoom }) {
  const box = useRef(null);
  const [state, setState] = useState({ data: null, error: null });
  // Maps are for admins in this version: they show the whole layout of
  // what a system reaches. The data stays queryable for anyone who may
  // query, so this hides a picture, not the facts behind it.
  const hidden = panel.type === 'map' && !admin;
  // A fixed window changes only when the user picks another; "last N" moves with every refresh.
  useEffect(() => {
    if (hidden) return;
    let live = true;
    run(panel, from, to, variables, values, box.current ? box.current.clientWidth : 600)
      .then(data => live && setState({ data: { ...data, type: panel.type }, error: null }), e => live && setState(s => ({ ...s, error: e.message })));
    return () => { live = false; };
  }, [JSON.stringify(panel), Math.round(to - from), from > 1e9 && to - from ? Math.round(from / 5) : 0, tick, JSON.stringify(values)]);

  const { data, error } = state;
  let body = html`<div class="empty">Loading…</div>`;
  if (hidden) body = html`<div class="empty">Only admins see maps.</div>`;
  else if (error) body = html`<div class="error" style="margin: 0">${error}</div>`;
  else if (data && data.type === panel.type) body = html`<${Guard}><${Content} panel=${panel} data=${data} height=${height} onZoom=${onZoom} /><//>`;
  return html`<div class="tile-body" ref=${box}>${body}</div>`;
}

// Guard keeps a panel that cannot be drawn from taking the page with it.
function Guard({ children }) {
  const [crash] = useErrorBoundary();
  return crash ? html`<div class="error" style="margin: 0">This panel could not be drawn: ${crash.message}</div>` : children;
}
const Content = ({ panel, data, height, onZoom }) => show(panel, data, height, onZoom);

function show(panel, data, height, onZoom) {
  const none = html`<div class="empty">No data in this time range.</div>`;
  if (panel.type === 'timeseries') {
    if (!data.series.length) return none;
    const legend = data.series.length > 1 && data.series.length <= 12 && height >= 150;
    return html`<${Chart} result=${data} names=${data.names} unit=${panel.unit} min=${panel.min} fill=${panel.fill}
      height=${Math.max(90, height - (legend ? 42 : 0))} legend=${legend} sync="dashboard" onZoom=${onZoom} />
      ${data.total > data.series.length && html`<div class="muted" style="font-size: 12px">Showing ${data.series.length} of ${data.total} series.</div>`}`;
  }
  if (panel.type === 'map') return html`<${MapView} panel=${panel} data=${data} height=${height} />`;
  if (panel.type === 'stat') {
    if (!data.values.length) return none;
    const v = data.values[0].value;
    const level = panel.crit != null && v >= panel.crit ? 'bad' : panel.warn != null && v >= panel.warn ? 'warn' : 'ok';
    const colored = panel.crit != null || panel.warn != null;
    return html`<div class="stat ${colored ? level : ''}">${fmtUnit(v, panel.unit)}
      ${data.values.length > 1 && html`<small>first of ${data.values.length} results</small>`}</div>`;
  }
  if (panel.type === 'table') {
    if (!data.values.length) return none;
    // One row per series, one value column per query: the rows of the
    // queries are matched by their labels, whatever the metric is called.
    // A log query whose stats give several results (count() as calls,
    // avg(ms) as avg_ms) gets a column for each, named by the result.
    const logs = panel.source === 'logs';
    const named = new Map(); // query index -> result names, in the order seen
    for (const r of data.values) {
      const n = logs ? r.metric.__name__ : undefined;
      if (!named.has(r.q || 0)) named.set(r.q || 0, []);
      if (n != null && !named.get(r.q || 0).includes(n)) named.get(r.q || 0).push(n);
    }
    const qs = panel.queries;
    const columns = qs.flatMap((q, i) => {
      const names = named.get(i) || [];
      if (names.length > 1) return names.map(n => ({ q, key: i + ':' + n, title: n }));
      return [{ q, key: i + ':' + (names[0] ?? ''), title: qs.length > 1 ? q.legend || 'Value ' + (i + 1) : 'Value' }];
    });
    const rows = new Map();
    for (const r of data.values) {
      const labels = Object.entries(r.metric).filter(([k]) => k !== '__name__').sort(([a], [b]) => (a < b ? -1 : 1));
      const key = JSON.stringify(labels);
      if (!rows.has(key)) rows.set(key, { metric: r.metric, cells: {} });
      rows.get(key).cells[(r.q || 0) + ':' + ((logs ? r.metric.__name__ : undefined) ?? '')] = r.value;
    }
    const all = [...rows.values()];
    const cols = panel.columns && panel.columns.length ? panel.columns
      : [...new Set(all.flatMap(r => Object.keys(r.metric)))].filter(k => k !== '__name__').sort();
    // Yellow and red by the query's thresholds; with red below yellow, low is bad.
    const level = (q, v) => {
      if (v == null || (q.warn == null && q.crit == null)) return '';
      const low = q.warn != null && q.crit != null && q.crit < q.warn;
      const past = t => t != null && (low ? v <= t : v >= t);
      return past(q.crit) ? 'lvl-bad' : past(q.warn) ? 'lvl-warn' : 'lvl-ok';
    };
    return html`<div class="scroll"><table><thead><tr>${cols.map(c => html`<th>${c}</th>`)}
      ${columns.map(c => html`<th style="text-align: right">${c.title}</th>`)}</tr></thead>
      <tbody>${all.slice(0, 200).map(r => html`<tr>${cols.map(c => html`<td class="mono">${r.metric[c] ?? ''}</td>`)}
        ${columns.map(c => html`<td class="num mono ${level(c.q, r.cells[c.key])}">${r.cells[c.key] == null ? '' : fmtUnit(r.cells[c.key], c.q.unit || panel.unit)}</td>`)}</tr>`)}</tbody></table></div>`;
  }
  if (panel.type !== 'logs') return html`<div class="error" style="margin: 0">Unknown panel type “${panel.type}”.</div>`;
  if (!data.rows.length) return html`<div class="empty">No log lines in this time range.</div>`;
  return html`<div class="scroll logs">${data.rows.map(r => {
    const t = new Date(r._time);
    const src = r.container || r.unit || r.app || r.rule || r.source || '';
    return html`<div class="log compact"><div class="t" title=${r._time}>${t.toLocaleTimeString(undefined, { hour12: false })}</div>
      <div><span class="m">${r._msg}</span> <span class="src">${r.agent}${src && ' · ' + src}</span></div></div>`;
  })}</div>`;
}

// PanelEditor edits one panel. Explore uses it too, to make a panel of a
// query: heading and action name the dialog and its button there, head is
// drawn above the form's own fields, error under them.
export function PanelEditor({ panel, onSave, onClose, heading, action, head, error }) {
  const [p, setP] = useState(() => structuredClone(panel));
  const set = (k, v) => setP({ ...p, [k]: v });
  const num = v => (v === '' || v == null ? undefined : Number(v));
  const setQuery = (i, k, v) => set('queries', p.queries.map((q, j) => (j === i ? { ...q, [k]: v } : q)));
  const logsOnly = p.type === 'logs';
  const isLogs = logsOnly || p.source === 'logs';
  const submit = e => {
    e.preventDefault();
    const out = { title: p.title.trim(), type: p.type, w: +p.w, h: +p.h,
      queries: p.queries.filter(q => q.expr.trim()).map(q => ({ expr: q.expr.trim(), ...(q.legend ? { legend: q.legend } : {}),
        ...(p.type === 'table' && q.unit ? { unit: q.unit } : {}), ...(p.type === 'table' && q.warn != null ? { warn: q.warn } : {}),
        ...(p.type === 'table' && q.crit != null ? { crit: q.crit } : {}) })) };
    const columns = (Array.isArray(p.columns) ? p.columns.join(',') : String(p.columns || '')).split(',').map(c => c.trim()).filter(Boolean);
    if (p.type === 'table' && columns.length) out.columns = columns;
    if (!logsOnly && p.source === 'logs') out.source = 'logs';
    if (p.unit && !logsOnly) out.unit = p.unit;
    if (p.type === 'timeseries') { if (p.min != null) out.min = p.min; if (p.fill) out.fill = true; }
    if (p.type === 'stat') { if (p.warn != null) out.warn = p.warn; if (p.crit != null) out.crit = p.crit; }
    if (logsOnly && p.limit) out.limit = +p.limit;
    // A map's queries carry roles (node, from/to, of) this form does not
    // edit: they are kept as they are, and changed in the dashboard's JSON.
    if (p.type === 'map') {
      out.queries = p.queries.filter(q => q.expr.trim());
      if (Array.isArray(p.columns) && p.columns.length) out.columns = p.columns;
    }
    if (!out.queries.length) return;
    onSave(out);
  };
  return html`<${Dialog} title=${heading || (panel.title ? 'Edit panel' : 'Add panel')} wide onClose=${onClose}><form onSubmit=${submit}>
    ${head}
    <div class="row" style="align-items: flex-end">
      <div class="field" style="flex: 2; min-width: 200px"><label>Title</label><input required value=${p.title} onInput=${e => set('title', e.target.value)} autofocus /></div>
      <div class="field" style="flex: 1"><label>Shows</label><select value=${p.type} onChange=${e => set('type', e.target.value)}>
        ${(panel.type === 'map' ? [...TYPES, ['map', 'Map (nodes and links)']] : TYPES).map(([v, l]) => html`<option value=${v}>${l}</option>`)}</select></div>
      ${!logsOnly && html`<div class="field" style="flex: 1"><label>Queries are</label><select value=${p.source || 'metrics'} onChange=${e => set('source', e.target.value)}>
        <option value="metrics">PromQL (metrics)</option><option value="logs">LogsQL (logs)</option></select></div>`}
    </div>
    ${p.queries.map((q, i) => html`<div class="row" style="align-items: flex-start; margin-bottom: 8px">
      <textarea rows="2" style="flex: 3" spellcheck="false" required=${i === 0} aria-label="Query" value=${q.expr} onInput=${e => setQuery(i, 'expr', e.target.value)}
        placeholder=${logsOnly ? 'LogsQL, e.g. app:nginx error' : isLogs ? 'LogsQL ending in | stats …, e.g. error | stats by (app) count() as errors' : 'PromQL, e.g. rate(http_requests_total[5m])'}></textarea>
      ${!logsOnly && html`<input style="flex: 1; min-width: 120px" aria-label=${p.type === 'table' ? 'Column title' : 'Legend'} placeholder=${p.type === 'table' ? 'Column title' : 'Legend, e.g. {{agent}}'} value=${q.legend || ''} onInput=${e => setQuery(i, 'legend', e.target.value)} />`}
      ${p.type === 'table' && html`
        <select aria-label="Unit of this column" title="Unit of this column" value=${q.unit || ''} onChange=${e => setQuery(i, 'unit', e.target.value)} style="width: 110px">
          <option value="">panel's unit</option>${UNITS.filter(([v]) => v).map(([v, l]) => html`<option value=${v}>${l}</option>`)}</select>
        <input type="number" step="any" aria-label="Yellow from" title="Yellow from this value" placeholder="yellow" value=${q.warn ?? ''} onInput=${e => setQuery(i, 'warn', num(e.target.value))} style="width: 84px" />
        <input type="number" step="any" aria-label="Red from" title="Red from this value. Below the yellow value means low is bad" placeholder="red" value=${q.crit ?? ''} onInput=${e => setQuery(i, 'crit', num(e.target.value))} style="width: 84px" />`}
      ${p.queries.length > 1 && html`<button type="button" class="small" onClick=${() => set('queries', p.queries.filter((_, j) => j !== i))}>Remove</button>`}
    </div>`)}
    ${!logsOnly && p.queries.length < 10 && html`<button type="button" class="small" onClick=${() => set('queries', [...p.queries, { expr: '' }])}>Add query</button>`}
    ${p.type === 'map' && html`<p class="muted" style="font-size: 12.5px">Each query of a map is nodes, links or details, set with
      <code>node</code> and <code>tier</code>, <code>from</code> and <code>to</code>, or <code>of</code>: edit those in the dashboard's JSON
      (“Title, variables, JSON”). Only admins see maps.</p>`}
    ${p.type === 'table' && html`<p class="muted" style="font-size: 12.5px">Each query is a column; rows are matched by their labels. A cell turns yellow and red from the values given; with red below yellow, low values are the bad ones.</p>
      <div class="field"><label>Label columns, in order (empty: all)</label><input placeholder="probe, target, agent" value=${Array.isArray(p.columns) ? p.columns.join(', ') : p.columns || ''} onInput=${e => set('columns', e.target.value)} /></div>`}
    <p class="muted" style="font-size: 12.5px">Use the dashboard's variables as <code>$name</code>, for example <code>${isLogs ? 'agent:~"$agent"' : 'agent=~"$agent"'}</code>.</p>
    <div class="row" style="align-items: flex-end">
      <div class="field"><label>Width (of 12)</label><input type="number" min="1" max="12" required value=${p.w} onInput=${e => set('w', e.target.value)} style="width: 90px" /></div>
      <div class="field"><label>Height (rows)</label><input type="number" min="1" max="12" required value=${p.h} onInput=${e => set('h', e.target.value)} style="width: 90px" /></div>
      ${!logsOnly && html`<div class="field"><label>Unit</label><select value=${p.unit || ''} onChange=${e => set('unit', e.target.value)}>
        ${UNITS.map(([v, l]) => html`<option value=${v}>${l}</option>`)}</select></div>`}
      ${p.type === 'timeseries' && html`
        <div class="field"><label>Y axis starts at</label><input type="number" step="any" placeholder="auto" value=${p.min ?? ''} onInput=${e => set('min', num(e.target.value))} style="width: 100px" /></div>
        <div class="field"><label><input type="checkbox" checked=${!!p.fill} onChange=${e => set('fill', e.target.checked)} /> Fill under lines</label></div>`}
      ${p.type === 'stat' && html`
        <div class="field"><label>Yellow from</label><input type="number" step="any" value=${p.warn ?? ''} onInput=${e => set('warn', num(e.target.value))} style="width: 100px" /></div>
        <div class="field"><label>Red from</label><input type="number" step="any" value=${p.crit ?? ''} onInput=${e => set('crit', num(e.target.value))} style="width: 100px" /></div>`}
      ${logsOnly && html`<div class="field"><label>Lines</label><input type="number" min="1" max="1000" value=${p.limit || 50} onInput=${e => set('limit', e.target.value)} style="width: 100px" /></div>`}
    </div>
    <${ErrorBox} error=${error} />
    <div class="actions"><button type="button" onClick=${onClose}>Cancel</button><button class="primary">${action || (panel.title ? 'Apply' : 'Add')}</button></div>
  </form><//>`;
}

function SettingsEditor({ dash, onSave, onClose }) {
  const [title, setTitle] = useState(dash.title);
  const [vars, setVars] = useState(() => structuredClone(dash.variables || []));
  const [json, setJson] = useState(null); // text while the JSON view is open
  const [error, setError] = useState(null);
  const setVar = (i, k, v) => setVars(vars.map((x, j) => (j === i ? { ...x, [k]: v } : x)));
  const current = () => ({ ...dash, title: title.trim(), variables: vars.filter(v => v.name && v.label) });
  const apply = e => {
    e.preventDefault();
    if (json === null) return onSave(current());
    try {
      const d = JSON.parse(json);
      if (!d || typeof d !== 'object' || !Array.isArray(d.panels)) throw new Error('The JSON must be an object with a "panels" list.');
      onSave({ ...d, id: dash.id });
    } catch (err) { setError(err.message); }
  };
  return html`<${Dialog} title="Dashboard settings" wide onClose=${onClose}><form onSubmit=${apply}>
    ${json === null ? html`
      <div class="field"><label>Title</label><input required maxlength="100" value=${title} onInput=${e => setTitle(e.target.value)} /></div>
      <h2 style="margin-top: 16px">Variables</h2>
      <p class="muted" style="font-size: 12.5px">A variable is a drop-down listing the values of a label. Queries use it as <code>$name</code>.</p>
      ${vars.map((v, i) => html`<div class="row" style="margin-bottom: 8px">
        <input placeholder="name, e.g. agent" aria-label="Variable name" pattern="[a-z][a-z0-9_]*" value=${v.name} onInput=${e => setVar(i, 'name', e.target.value)} style="flex: 1" />
        <input placeholder="label, e.g. agent" aria-label="Label" value=${v.label} onInput=${e => setVar(i, 'label', e.target.value)} style="flex: 1" />
        <input placeholder="only from series matching, e.g. host_load1 (optional)" aria-label="Series selector" value=${v.match || ''} onInput=${e => setVar(i, 'match', e.target.value)} style="flex: 2" />
        <button type="button" class="small" onClick=${() => setVars(vars.filter((_, j) => j !== i))}>Remove</button></div>`)}
      <button type="button" class="small" onClick=${() => setVars([...vars, { name: '', label: '', match: '' }])}>Add variable</button>`
    : html`<p class="muted">The whole dashboard as JSON. Paste one here to import it.</p>
      <textarea rows="22" spellcheck="false" aria-label="Dashboard JSON" value=${json} onInput=${e => setJson(e.target.value)}></textarea>`}
    <${ErrorBox} error=${error} />
    <div class="actions">
      <button type="button" onClick=${() => { setError(null); setJson(json === null ? JSON.stringify(current(), null, 2) : null); }}>${json === null ? 'Edit as JSON' : 'Back to the form'}</button>
      <span class="spacer"></span>
      <button type="button" onClick=${onClose}>Cancel</button><button class="primary">Apply</button></div>
  </form><//>`;
}
