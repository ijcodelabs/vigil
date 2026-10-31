import { html, useState, useRef, useMemo, api, useLoad, Badge, ErrorBox, Dialog, YamlEditor, ago, fmtNum, fmtUnit } from './lib.js';
import { Chart } from './explore.js';

// SLOs: service level objectives, their error budgets and burn rates.
// The figures come from the server (GET /api/v1/slos?status=1 and
// /api/v1/slos/{name}), which reads the per-minute counts the SLO's rule
// group records; the charts read those counts directly.

const form = o => ({ method: 'POST', body: new URLSearchParams(o) });
const q = s => JSON.stringify(String(s));
const pct = v => (v == null ? '–' : fmtPct(v));
// Objectives live near 100 %: 99.95 must not show as 100.0.
function fmtPct(v) {
  const digits = v >= 99.99 ? 3 : v >= 99 ? 2 : 1;
  return v.toFixed(digits).replace(/\.?0+$/, '') + '%';
}
const BURN = ['1h', '6h', '1d', '3d'];
// The burn-rate alerts' pairs (internal/slo): long window, share of the
// budget it may spend before the alert fires.
const SPEND = { '1h': 0.02, '6h': 0.05, '1d': 0.1, '3d': 0.1 };
const HOURS = { '1h': 1, '6h': 6, '1d': 24, '3d': 72 };
const WINDOW_DAYS = { '7d': 7, '28d': 28, '30d': 30, calendar_month: 30 };
const threshold = (slo, w) => (SPEND[w] * WINDOW_DAYS[slo.window] * 24) / HOURS[w];
const windowText = w => (w === 'calendar_month' ? 'calendar month' : w.replace('d', ' days') + ' rolling');

// Warnings from the last save, shown once on the SLO's page.
let saved = null;

export function SLOs({ route, admin }) {
  if (route.parts[1] === '~report') return html`<${Report} key=${route.parts[2] || ''} period=${route.parts[2] || ''} />`;
  const name = route.parts[1] && decodeURIComponent(route.parts[1]);
  if (name) return html`<${SLOView} key=${name} name=${name} admin=${admin} />`;
  return html`<${SLOList} admin=${admin} />`;
}

function BudgetBar({ left }) {
  if (left == null) return '–';
  const w = Math.max(0, Math.min(1, left));
  const kind = left <= 0 ? 'bad' : left < 0.25 ? 'warn' : 'ok';
  return html`<span class="budget" title="${fmtNum(left * 100)}% of the error budget left"><i class=${kind} style="width: ${w * 100}%"></i></span>
    <span class=${left <= 0 ? 'bad-text' : ''}>${(left * 100).toFixed(0)}%</span>`;
}

function BurnBadge({ slo, st, w }) {
  const v = st.burn?.[w];
  if (v == null) return html`<span class="muted">–</span>`;
  const at = threshold(slo, w);
  return html`<span title="Alerts fire above ${fmtNum(at)} (with the shorter window too)"><${Badge} kind=${v > at ? 'bad' : v > 1 ? 'warn' : 'ok'}>${fmtNum(Number(v.toFixed(v >= 10 ? 0 : 2)))}<//></span>`;
}

const TEMPLATE = `name: frontend-errors
service: frontend
owner: web-team
description: The shop answers without server errors
objective: 99.9
window: 30d
indicator:
  span:
    service: frontend
    kind: server
    # latency_below: 500ms     # good means fast as well as not failed
# Or a check:   indicator: {probe: {name: shop-home}}
# Or a ratio:   indicator: {ratio: {bad: 'http_requests_total{code=~"5.."}', total: 'http_requests_total'}}
`;

function newSLO(text) {
  const m = /^name:\s*["']?([a-z0-9-]+)["']?\s*$/m.exec(text);
  if (!m) throw new Error('The document needs a name: line (lower-case letters, digits and dashes).');
  return m[1];
}

async function save(name, text) {
  const r = await api('/api/v1/slos/' + encodeURIComponent(name), { method: 'PUT', body: text });
  saved = { name, warnings: r.warnings || [] };
  return r;
}

function SLOList({ admin }) {
  const { data, error, reload } = useLoad(() => api('/api/v1/slos?status=1'), [], 60000);
  const [filter, setFilter] = useState('');
  const [creating, setCreating] = useState(null); // the document to start from
  const [picking, setPicking] = useState(false);
  const created = useRef('');
  const rows = useMemo(() => {
    if (!data) return null;
    const st = Object.fromEntries(data.status.map(s => [s.name, s]));
    const f = filter.trim().toLowerCase();
    return data.slos.map(s => ({ slo: s, st: st[s.name] || { burn: {}, firing: [] } })).filter(({ slo }) => !f ||
      [slo.name, slo.service, slo.owner, slo.description, ...Object.entries(slo.labels || {}).map(([k, v]) => k + '=' + v)]
        .some(x => x && x.toLowerCase().includes(f)));
  }, [data, filter]);

  return html`
    <div class="head"><h1>SLOs</h1><span class="spacer"></span>
      ${data && data.slos.length > 3 && html`<input type="search" placeholder="Service, owner, label…" value=${filter} onInput=${e => setFilter(e.target.value)} aria-label="Filter SLOs" style="width: 220px" />`}
      ${data && data.slos.length > 0 && html`<a class="btn" href="#/slos/~report">Reports</a>`}
      ${admin && html`<button onClick=${() => setPicking(true)}>From a template</button>
        <button class="primary" onClick=${() => setCreating(TEMPLATE)}>New SLO</button>`}
      <a class="btn" href="/docs/slo" target="_blank" rel="noopener">Help</a></div>
    <${ErrorBox} error=${error} />
    <div class="panel flush">${!rows ? html`<div class="empty">Loading…</div>` : data.slos.length === 0
      ? html`<div class="empty">No SLOs yet. An SLO says what share of events must be good over a window (99.9 % of requests without
          a server error over 30 days, say); vigil counts good and total events every minute, keeps the error budget and alerts when it
          burns too fast. ${admin ? 'Start from a template (one for each check, traced service or HAProxy backend), create one with New SLO, or' : 'An admin creates them here, or'} keep them in git and
          apply them with <code>vigil apply -f DIR</code> (<code>DIR/slos/*.yaml</code>). <a href="/docs/slo" target="_blank" rel="noopener">How</a></div>`
      : html`<table>
        <thead><tr><th>SLO</th><th>Service</th><th style="text-align: right">Objective</th><th style="text-align: right">Achieved</th>
          <th>Budget left</th><th style="text-align: right">Burn rate, 1h</th><th style="text-align: right">Coverage</th><th>Alerts</th></tr></thead>
        <tbody>${rows.length === 0 && html`<tr><td colspan="8" class="empty">No SLO matches.</td></tr>`}
        ${rows.map(({ slo, st }) => html`<tr class="click" onClick=${() => { location.hash = '#/slos/' + encodeURIComponent(slo.name); }}>
          <td><strong>${slo.name}</strong>${slo.error ? html` <${Badge} kind="bad">does not parse<//>` : html` <span class="muted">${windowText(slo.window)}</span>`}
            ${slo.description && html`<div class="muted" style="font-size: 12.5px">${slo.description}</div>`}</td>
          <td>${slo.service || html`<span class="muted">–</span>`}${slo.owner && html`<div class="muted" style="font-size: 12.5px">${slo.owner}</div>`}</td>
          <td class="num">${slo.objective ? fmtPct(slo.objective) : '–'}</td>
          <td class="num">${st.achieved == null ? html`<span class="muted" title="No events in the window yet">no events</span>`
            : html`<span class=${st.achieved < slo.objective ? 'bad-text' : ''}>${pct(st.achieved)}</span>`}</td>
          <td style="white-space: nowrap"><${BudgetBar} left=${st.budget_left} /></td>
          <td class="num"><${BurnBadge} slo=${slo} st=${st} w="1h" /></td>
          <td class="num">${st.coverage == null ? '–' : html`<span class=${st.coverage < 0.99 ? 'warn-text' : ''}
            title=${st.counting ? 'Counting since ' + new Date(st.counting).toLocaleString() : ''}>${(st.coverage * 100).toFixed(st.coverage < 0.99 ? 1 : 0)}%</span>`}</td>
          <td>${st.firing.map(a => html`<${Badge} kind=${a === 'SLOBurnFast' ? 'bad' : 'warn'}>${a.replace(/^SLO/, '')}<//> `)}</td>
        </tr>`)}</tbody></table>`}</div>
    ${data && data.slos.length > 0 && html`<p class="muted">Achieved and budget left are over each SLO's window; the burn rate is how fast
      the budget went in the last hour (1 spends it exactly in one window). Coverage is the share of minutes counted since the SLO started counting.</p>`}
    ${picking && html`<${TemplatePicker} onClose=${() => setPicking(false)} onPick=${doc => { setPicking(false); setCreating(doc); }} />`}
    ${creating && html`<${YamlEditor} title="New SLO" help="The server checks the indicator's queries against the metric store before it saves."
      load=${() => Promise.resolve(creating)}
      save=${async text => {
        created.current = newSLO(text);
        // A PUT replaces: a new SLO must not take an existing one's name.
        if (data && data.slos.some(x => x.name === created.current)) throw new Error(`There is an SLO called ${created.current} already; change it on its own page, or pick another name.`);
        await save(created.current, text);
      }}
      onClose=${ok => { setCreating(null); if (ok) location.hash = '#/slos/' + encodeURIComponent(created.current); else reload(); }} />`}`;
}

// TemplatePicker lists the SLOs the templates offer for what reports now:
// vigil's (checks, traced services) and those of the integrations in use.
function TemplatePicker({ onClose, onPick }) {
  const { data, error } = useLoad(() => api('/api/v1/slos/templates'), []);
  const groups = useMemo(() => {
    const out = new Map();
    for (const o of data || []) {
      if (!out.has(o.template)) out.set(o.template, { ...o, offers: [] });
      out.get(o.template).offers.push(o);
    }
    return [...out.values()];
  }, [data]);
  return html`<${Dialog} title="SLO from a template" wide onClose=${onClose}>
    <p class="muted" style="margin-top: 0">One for each check, traced service or integration backend that reported in the last day.
      Pick one to see and change its document before it is saved.</p>
    <${ErrorBox} error=${error} />
    ${!data && !error && html`<div class="empty">Looking…</div>`}
    ${data && groups.length === 0 && html`<div class="empty">Nothing to offer: no checks, no traced services and no integration with
      SLO templates (HAProxy) reported in the last day.</div>`}
    <div style="max-height: 60vh; overflow: auto">${groups.map(g => html`
      <h3 class="section" style="margin-bottom: 2px">${g.title} <span class="muted" style="font-weight: 400; font-size: 12.5px">${g.source}</span></h3>
      ${g.description && html`<div class="muted" style="font-size: 12.5px; margin-bottom: 6px">${g.description}</div>`}
      <div class="panel flush"><table><tbody>${g.offers.map(o => html`<tr>
        <td class="mono">${o.value}</td><td class="muted">${o.name}</td>
        <td style="text-align: right">${o.exists ? html`<a href=${'#/slos/' + encodeURIComponent(o.name)} onClick=${onClose}>exists</a>`
          : html`<button class="small" onClick=${() => onPick(o.doc)}>Use</button>`}</td></tr>`)}</tbody></table></div>`)}</div>
    <div class="actions"><span class="spacer"></span><button onClick=${onClose}>Close</button></div>
  <//>`;
}

// series builds a result the Chart takes from [t, value] pairs.
const result = (lines, from, to, step) => ({ from, to, step, series: lines.map(values => ({ metric: {}, values })) });

function SLOView({ name, admin }) {
  const { data, error, reload } = useLoad(() => api('/api/v1/slos/' + encodeURIComponent(name)), [name], 60000);
  const [warnings, setWarnings] = useState(() => {
    const w = saved && saved.name === name ? saved.warnings : [];
    saved = null;
    return w;
  });
  const [editing, setEditing] = useState(false);
  const [showVersion, setShowVersion] = useState(null);
  const [busy, setBusy] = useState(null);
  const [actionError, setActionError] = useState(null);
  const slo = data?.slo, st = data?.status;

  // The counts in buckets across the window: budget left as it went, the
  // bad share and the number of events.
  const { data: charts, error: chartError } = useLoad(async () => {
    if (!st) return null;
    const from = Date.parse(st.from) / 1000, to = Date.parse(st.to) / 1000;
    const now = Date.now() / 1000 - 120; // the newest minutes are still being written
    const step = to - from > 10 * 86400 ? 3 * 3600 : 3600;
    // The store evaluates a range at start + k·step: start on a step so
    // that each bucket is a whole step, ending at its timestamp.
    const first = Math.ceil(from / step) * step;
    const end = Math.floor(Math.min(now, to) / step) * step;
    const sel = `{slo=${q(name)}}`;
    const read = async metric => {
      const r = await api('/api/v1/query_range', form({ query: `sum(sum_over_time(${metric}${sel}[${step}s]))`, start: first, end, step }));
      return new Map((r.data.result[0]?.values || []).map(([t, v]) => [Math.round(t), Number(v)]));
    };
    const [good, total] = await Promise.all([read('slo_good_per_minute'), read('slo_total_per_minute')]);
    const budget = st.budget;
    const all = [...total.values()].reduce((a, b) => a + b, 0);
    const left = [], even = [], bad = [], allowed = [], events = [], badEvents = [];
    let spent = 0;
    for (let t = first; t <= Math.ceil(to / step) * step; t += step) {
      even.push([t, String(1 - (t - from) / (to - from))]);
      if (t > end) continue;
      const n = total.get(t), g = good.get(t) ?? 0;
      if (n > 0) {
        spent += n - g;
        bad.push([t, String(1 - g / n)]);
        events.push([t, String(n)]);
        badEvents.push([t, String(n - g)]);
      }
      allowed.push([t, String(budget)]);
      // From the first counted bucket on: before it there is no budget yet.
      if (all > 0 && (n > 0 || left.length > 0)) left.push([t, String(1 - spent / (budget * all))]);
    }
    return {
      left: result([left, even], from, to, step),
      bad: result([bad, allowed], from, end, step),
      events: result([events, badEvents], from, end, step),
      step,
    };
  }, [st?.from, st?.to, name], 300000);

  const { data: ops } = useLoad(async () => {
    if (!data?.operations) return null;
    const now = Date.now() / 1000;
    const [bad, total] = await Promise.all([data.operations.bad, data.operations.total].map(expr =>
      api('/api/v1/query', form({ query: expr, time: now })).then(r => r.data.result)));
    const n = Object.fromEntries(total.map(r => [r.metric.span_name, Number(r.value[1])]));
    return bad.map(r => ({ name: r.metric.span_name, bad: Number(r.value[1]), total: n[r.metric.span_name] || 0 }))
      .filter(o => o.total > 0 && o.bad > 0).sort((a, b) => b.bad * b.total - a.bad * a.total).slice(0, 8);
  }, [data?.operations?.bad], 300000);

  const { data: versions, reload: reloadVersions } = useLoad(() => api(`/api/v1/slos/${encodeURIComponent(name)}/versions`), [name]);
  const { data: silences } = useLoad(async () => {
    const all = await api('/alertmanager/api/v2/silences');
    return all.filter(s => s.status.state !== 'expired' && s.matchers.some(m => m.name === 'slo' && !m.isRegex && m.value === name));
  }, [name], 60000);

  const remove = async () => {
    if (!confirm(`Delete the SLO ${name}? Its rules stop at once; the counts already recorded stay in the metric store.`)) return;
    try { await api('/api/v1/slos/' + encodeURIComponent(name), { method: 'DELETE' }); location.hash = '#/slos'; } catch (e) { setActionError(e.message); }
  };
  const restore = async v => {
    setBusy(v); setActionError(null);
    try {
      const doc = (await api(`/api/v1/slos/${encodeURIComponent(name)}/versions/${v}`)).doc;
      await save(name, doc);
      setWarnings(saved?.warnings || []); saved = null;
      setShowVersion(null);
      await reload(); reloadVersions();
    } catch (e) { setActionError(e.message); }
    setBusy(null);
  };

  const back = html`<a href="#/slos">← SLOs</a>`;
  if (error) return html`<div class="head">${back}<h1>${name}</h1></div><${ErrorBox} error=${error} />`;
  if (!data) return html`<div class="empty">Loading…</div>`;
  const firing = st?.firing || [];
  const tile = (title, body, sub) => html`<section class="panel tile" style="grid-column: span 3; height: 120px"><header><h2>${title}</h2></header>
    <div class="tile-body">${body}${sub && html`<div class="muted" style="text-align: center; font-size: 12px">${sub}</div>`}</div></section>`;

  return html`
    <div class="head">${back}<h1>${name}</h1>
      ${firing.map(a => html`<${Badge} kind=${a === 'SLOBurnFast' ? 'bad' : 'warn'}>${a}<//>`)}
      <span class="spacer"></span>
      <a class="btn" href="#/slos/~report">Reports</a>
      ${admin && html`<button onClick=${() => setEditing(true)}>Edit</button><button onClick=${remove}>Delete</button>`}
      <a class="btn" href="/docs/slo" target="_blank" rel="noopener">Help</a></div>
    ${warnings.length > 0 && html`<div class="notice">Saved, with ${warnings.length === 1 ? 'a warning' : 'warnings'}:
      ${warnings.map(w => html`<div>· ${w}</div>`)}</div>`}
    <${ErrorBox} error=${actionError || (slo.error && 'This SLO\'s document no longer parses, so it records nothing until it is saved again: ' + slo.error)} />
    <div class="panel">
      <div>${slo.description || html`<span class="muted">No description.</span>`}</div>
      <div class="muted" style="margin-top: 4px">${[
        `${fmtPct(slo.objective)} over ${windowText(slo.window)}`,
        `${slo.kind} indicator`,
        slo.service && html`service <a href=${'#/services/' + encodeURIComponent(slo.service)}>${slo.service}</a>`,
        slo.owner && `owner ${slo.owner}`,
        slo.runbook_url && html`<a href=${slo.runbook_url} target="_blank" rel="noopener">runbook</a>`,
        `version ${slo.version}, ${ago(slo.updated)} by ${slo.author || 'unknown'}${slo.origin === 'files' ? ' (vigil apply)' : ''}`,
      ].filter(Boolean).map((x, i) => html`${i > 0 && ' · '}${x}`)}</div>
    </div>
    ${data.status_error && html`<${ErrorBox} error=${'The figures could not be read: ' + data.status_error} />`}
    ${st && html`
      <div class="grid">
        ${tile('Achieved', html`<div class="stat ${st.achieved == null ? '' : st.achieved >= slo.objective ? 'ok' : 'bad'}" style="height: 60px">${st.achieved == null ? '–' : fmtPct(st.achieved)}</div>`,
          st.total > 0 ? `${fmtNum(st.total - st.good)} bad of ${fmtNum(st.total)}` : 'no events in the window yet')}
        ${tile('Error budget left', html`<div class="stat ${st.budget_left == null ? '' : st.budget_left <= 0 ? 'bad' : st.budget_left < 0.25 ? 'warn' : 'ok'}" style="height: 60px">${st.budget_left == null ? '–' : (st.budget_left * 100).toFixed(0) + '%'}</div>`,
          st.total > 0 ? `${fmtNum(Math.max(0, Math.floor(st.budget * st.total - (st.total - st.good))))} more bad events allowed at this volume` : '')}
        ${tile('Burn rate', html`<div class="row" style="justify-content: center; gap: 14px; height: 60px">${BURN.map(w => html`<div style="text-align: center">
            <div><${BurnBadge} slo=${slo} st=${st} w=${w} /></div><div class="muted" style="font-size: 12px">${w}</div></div>`)}</div>`,
          '1 spends the budget in exactly one window')}
        ${tile('Coverage', html`<div class="stat ${st.coverage != null && st.coverage < 0.99 ? 'warn' : ''}" style="height: 60px">${st.coverage == null ? '–' : (st.coverage * 100).toFixed(1) + '%'}</div>`,
          st.counting ? 'counting since ' + new Date(st.counting).toLocaleString() : 'not counted yet')}
      </div>`}
    <${ErrorBox} error=${chartError} />
    ${charts && html`<div class="grid">
      <section class="panel tile" style="grid-column: span 12; height: 260px"><header><h2>Error budget left</h2>
        <span class="muted" style="font-size: 12px">over the window; the straight line spends it evenly</span></header>
        <div class="tile-body"><${Chart} result=${charts.left} names=${['budget left', 'spent evenly']} height=${200} unit="ratio" sync="slo" /></div></section>
      <section class="panel tile" style="grid-column: span 6; height: 250px"><header><h2>Bad share</h2>
        <span class="muted" style="font-size: 12px">per ${charts.step / 3600} h</span></header>
        <div class="tile-body"><${Chart} result=${charts.bad} names=${['bad', 'allowed']} height=${190} unit="ratio" min=${0} sync="slo" /></div></section>
      <section class="panel tile" style="grid-column: span 6; height: 250px"><header><h2>Events</h2>
        <span class="muted" style="font-size: 12px">per ${charts.step / 3600} h</span></header>
        <div class="tile-body"><${Chart} result=${charts.events} names=${['all', 'bad']} height=${190} min=${0} fill sync="slo" /></div></section>
    </div>`}

    ${ops && ops.length > 0 && html`<h2 class="section">Where the budget goes, last 24 hours</h2>
      <div class="panel flush"><table>
        <thead><tr><th>Operation</th><th style="text-align: right">Events</th><th style="text-align: right">Bad</th></tr></thead>
        <tbody>${ops.map(o => html`<tr><td class="mono">${o.name}</td><td class="num">${fmtNum(Math.round(o.total))}</td>
          <td class="num"><${Badge} kind=${o.bad * 100 >= 100 - slo.objective ? 'bad' : 'warn'}>${fmtPct(o.bad * 100)}<//></td></tr>`)}</tbody></table></div>`}

    <h2 class="section">Incidents in the window</h2>
    <div class="panel flush">${(data.incidents || []).length === 0 ? html`<div class="empty">None.</div>` : html`<table>
      <thead><tr><th>Opened</th><th>Alert</th><th>State</th><th>Lasted</th><th>Acknowledged</th></tr></thead>
      <tbody>${data.incidents.map(i => html`<tr>
        <td style="white-space: nowrap">${new Date(i.first_seen).toLocaleString()}</td>
        <td><${Badge} kind=${i.severity === 'critical' ? 'bad' : 'warn'}>${i.labels?.alertname || i.kind}<//> <span class="muted">${i.title}</span></td>
        <td>${i.state === 'resolved' ? 'resolved' : html`<a href="#/home">${i.state}</a>`}</td>
        <td class="muted">${fmtUnit(((i.resolved_at ? Date.parse(i.resolved_at) : Date.now()) - Date.parse(i.first_seen)) / 1000, 'seconds')}</td>
        <td class="muted">${i.ack_by ? `${i.ack_by}${i.ack_comment ? ': ' + i.ack_comment : ''}` : ''}</td></tr>`)}</tbody></table>`}</div>
    ${silences && silences.length > 0 && html`<p class="muted">Silenced: ${silences.map((s, i) => html`${i > 0 && '; '}${s.comment || 'no comment'}
      (${s.createdBy}, until ${new Date(s.endsAt).toLocaleString()})`)} · <a href="#/alerts/silences">Silences</a></p>`}

    ${versions && versions.length > 0 && html`<details style="margin-top: 16px"><summary class="muted" style="cursor: pointer">History (${versions.length} versions)</summary>
      <div class="panel flush" style="margin-top: 8px"><table><tbody>${versions.map(v => html`<tr>
        <td>version ${v.version}${v.version === slo.version && html` <${Badge} kind="info">current<//>`}${v.deleted && html` <${Badge}>deleted<//>`}</td>
        <td class="muted">${new Date(v.updated).toLocaleString()} by ${v.author || 'unknown'}${v.origin === 'files' ? ' (vigil apply)' : ''}</td>
        <td style="text-align: right">${!v.deleted && html`<button class="small" onClick=${async () => {
          try { setShowVersion({ version: v.version, doc: (await api(`/api/v1/slos/${encodeURIComponent(name)}/versions/${v.version}`)).doc }); } catch (e) { setActionError(e.message); }
        }}>Show</button>`}</td></tr>`)}</tbody></table></div></details>`}

    ${data.rules && html`<details style="margin-top: 10px"><summary class="muted" style="cursor: pointer">Generated rules (${data.rules.reduce((n, g) => n + g.Rules.length, 0)})</summary>
      ${data.rules.map(g => html`<div class="panel" style="margin-top: 8px"><strong class="mono">${g.Name}</strong>
        <span class="muted"> · every ${fmtUnit(g.Interval / 1e9, 'seconds')}${g.Name.endsWith(':alerts') ? '' : ', each minute counted once'}</span>
        ${g.Rules.map(r => html`<div style="margin-top: 8px"><span class="mono">${r.Record || r.Alert}</span>
          ${r.Labels?.severity && html` <${Badge} kind=${r.Labels.severity === 'critical' ? 'bad' : 'warn'}>${r.Labels.severity}<//>`}
          <pre style="white-space: pre-wrap; margin: 4px 0 0">${r.Expr}</pre></div>`)}</div>`)}
      <p class="muted">Made from the SLO; they change when it does, and show under Alerts › Rules as read-only.</p></details>`}

    ${editing && html`<${YamlEditor} title=${'Edit SLO ' + name}
      help="Saving makes a new version; the counts recorded so far stay. A changed indicator counts the new way from the next minute."
      load=${() => Promise.resolve(slo.doc || '')} save=${text => save(name, text)}
      onClose=${ok => { setEditing(false); if (ok) { setWarnings(saved?.warnings || []); saved = null; reload(); reloadVersions(); } }} />`}
    ${showVersion && html`<${Dialog} title=${`${name}, version ${showVersion.version}`} wide onClose=${() => setShowVersion(null)}>
      <pre style="max-height: 60vh; overflow: auto">${showVersion.doc}</pre>
      <div class="actions"><span class="spacer"></span><button onClick=${() => setShowVersion(null)}>Close</button>
        ${admin && showVersion.version !== slo.version && html`<button class="primary" disabled=${busy} onClick=${() => restore(showVersion.version)}>${busy ? 'Restoring…' : 'Restore this version'}</button>`}</div>
    <//>`}`;
}

// Periods to offer: the months of the last year and the quarters of the
// last two, newest first, with any stored report further back.
function periods(stored) {
  const out = new Set(), now = new Date();
  for (let i = 0; i < 12; i++) {
    const d = new Date(Date.UTC(now.getUTCFullYear(), now.getUTCMonth() - i, 1));
    out.add(d.toISOString().slice(0, 7));
  }
  for (let i = 0; i < 8; i++) {
    const d = new Date(Date.UTC(now.getUTCFullYear(), now.getUTCMonth() - 3 * i, 1));
    out.add(`${d.getUTCFullYear()}-Q${Math.floor(d.getUTCMonth() / 3) + 1}`);
  }
  for (const r of stored || []) out.add(r.period);
  // Months and quarters of a year together, the year's quarters after its months.
  const key = p => p.includes('Q') ? p.slice(0, 5) + String(Number(p.slice(6)) * 3).padStart(2, '0') + 'q' : p;
  return [...out].sort((a, b) => (key(a) < key(b) ? 1 : -1));
}
const periodName = p => {
  if (p.includes('Q')) return `${p.slice(5)} ${p.slice(0, 4)}`;
  return new Date(p + '-01T00:00:00Z').toLocaleDateString(undefined, { month: 'long', year: 'numeric', timeZone: 'UTC' });
};

// Report is the SLA report of a month or quarter: the page to print or
// save as PDF, and the CSV for a spreadsheet.
function Report({ period }) {
  const { data: stored } = useLoad(() => api('/api/v1/slos/reports'), []);
  const { data: rep, error } = useLoad(() => api('/api/v1/slos/report' + (period ? '?period=' + encodeURIComponent(period) : '')), [period]);
  const p = period || rep?.period || '';
  const pick = e => { location.hash = '#/slos/~report/' + e.target.value; };
  const days = s => fmtUnit(s, 'seconds');
  return html`
    <div class="head no-print"><a href="#/slos">← SLOs</a><h1>SLA report</h1><span class="spacer"></span>
      <select value=${p} onChange=${pick} aria-label="Period">${periods(stored).map(x => html`<option value=${x} selected=${x === p}>${periodName(x)}</option>`)}</select>
      ${rep && html`<a class="btn" href=${'/api/v1/slos/report?format=csv&period=' + encodeURIComponent(rep.period)} download>CSV</a>
        <button onClick=${() => print()}>Print or PDF</button>`}
      <a class="btn" href="/docs/slo#sla-reports" target="_blank" rel="noopener">Help</a></div>
    <${ErrorBox} error=${error} />
    ${!rep && !error && html`<div class="empty">Working it out…</div>`}
    ${rep && html`
      <div class="report">
        <h2 style="margin-top: 0">Service levels, ${periodName(rep.period)}</h2>
        <p class="muted">${new Date(rep.from).toISOString().slice(0, 10)} to ${new Date(new Date(rep.to) - 1).toISOString().slice(0, 10)} (UTC).
          ${rep.final ? `Made ${new Date(rep.created).toLocaleString()}; a report of a finished period does not change afterwards.`
            : html`<${Badge} kind="warn">not final<//> The period is still running: these figures are up to ${new Date(rep.created).toLocaleString()} and change until it ends.`}</p>
        ${rep.slos.length === 0 ? html`<div class="panel empty">No SLO counted anything in this period.</div>` : html`
        <div class="panel flush"><table>
          <thead><tr><th>SLO</th><th style="text-align: right">Objective</th><th style="text-align: right">Achieved</th><th>Met</th>
            <th style="text-align: right">Budget used</th><th style="text-align: right">Bad events</th><th>Worst day</th>
            <th style="text-align: right">Coverage</th><th style="text-align: right">Incidents</th></tr></thead>
          <tbody>${rep.slos.map(r => html`<tr>
            <td><strong>${r.name}</strong>${(r.service || r.owner) && html`<div class="muted" style="font-size: 12.5px">${[r.service, r.owner].filter(Boolean).join(' · ')}</div>`}
              ${r.description && html`<div class="muted" style="font-size: 12.5px">${r.description}</div>`}</td>
            <td class="num">${fmtPct(r.objective)}${r.objective_changed && html`<span title="The objective changed during the period; the achieved share is judged against the one at the end"> *</span>`}</td>
            <td class="num">${r.achieved == null ? '–' : fmtPct(r.achieved)}</td>
            <td>${r.met == null ? html`<span class="muted">no events</span>` : r.met ? html`<${Badge} kind="ok">met<//>` : html`<${Badge} kind="bad">missed<//>`}</td>
            <td class="num">${r.budget_used == null ? '–' : (r.budget_used * 100).toFixed(0) + '%'}</td>
            <td class="num">${fmtNum(Math.round(r.bad))} <span class="muted">of ${fmtNum(Math.round(r.total))}</span></td>
            <td>${r.worst_day ? html`${r.worst_day.day} <span class="muted">${fmtPct(r.worst_day.achieved)}</span>` : html`<span class="muted">–</span>`}</td>
            <td class="num"><span class=${r.coverage < 0.99 ? 'warn-text' : ''} title=${r.counting ? 'Counting since ' + new Date(r.counting).toLocaleString() : ''}>${(r.coverage * 100).toFixed(r.coverage < 0.99 ? 1 : 0)}%</span></td>
            <td class="num">${r.incidents.length}</td></tr>`)}</tbody></table></div>
        ${rep.slos.filter(r => r.incidents.length > 0).map(r => html`
          <h3 class="section">${r.name}: incidents</h3>
          <div class="panel flush"><table>
            <thead><tr><th>Opened</th><th>Alert</th><th style="text-align: right">Lasted</th><th>Acknowledged</th></tr></thead>
            <tbody>${r.incidents.map(i => html`<tr>
              <td style="white-space: nowrap">${new Date(i.opened).toLocaleString()}</td>
              <td><${Badge} kind=${i.severity === 'critical' ? 'bad' : 'warn'}>${i.alert || 'alert'}<//> <span class="muted">${i.title}</span></td>
              <td class="num">${days(i.seconds)}${!i.resolved && html` <span class="muted">open</span>`}</td>
              <td class="muted">${i.ack_by ? `${i.ack_by}${i.ack_comment ? ': ' + i.ack_comment : ''}` : ''}</td></tr>`)}</tbody></table></div>`)}`}
        <p class="muted">Achieved is the share of good events in the period; budget used is the share of bad events the objective allows
          that were spent (above 100 %, the objective was missed). Coverage is the share of the period's minutes that were counted: a minute
          the server did not count is neither good nor bad, and an SLO started during the period shows the minutes from then on.
          The worst day is the UTC day with the lowest share of good events. vigil reports what it measured; what a contract makes of it is not part of this.</p>
      </div>`}`;
}
