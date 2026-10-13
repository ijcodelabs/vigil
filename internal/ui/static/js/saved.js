// What Explore can do with a query besides running it: keep it under a
// name, find it again (also the ones nobody saved, under Recent), and send
// it on to a dashboard or to the alert rules.
import { html, useState, useEffect, useRef, api, useLoad, Dialog, ErrorBox } from './lib.js';
import { PanelEditor, slug } from './dashboards.js';

// Recent queries are kept in this browser only, per account: they are a
// convenience for the person typing, not something to store for them.
const RECENT = 20;
const recentKey = (user, type) => `vigil-recent:${user}:${type}`;
export function recent(user, type) {
  try {
    const list = JSON.parse(localStorage.getItem(recentKey(user, type)) || '[]');
    return Array.isArray(list) ? list.filter(q => typeof q === 'string') : [];
  } catch { return []; }
}
export function remember(user, type, query) {
  if (!query || query === '*') return;
  try {
    localStorage.setItem(recentKey(user, type), JSON.stringify([query, ...recent(user, type).filter(q => q !== query)].slice(0, RECENT)));
  } catch {}
}

// The autofocus attribute only works while a page loads; what appears
// later is focused by hand, once. A moment later, because the element may
// be handed over before it is in the page.
const focusOnce = el => { if (el && !el.dataset.focused) { el.dataset.focused = '1'; setTimeout(() => el.focus()); } };

// Popover is a button with a panel that drops down from it. It closes on
// Escape and on a click anywhere else. children is called with close().
function Popover({ label, title, narrow, disabled, children }) {
  const [open, setOpen] = useState(false);
  const box = useRef(null);
  useEffect(() => {
    if (!open) return;
    const away = e => { if (!box.current?.contains(e.target)) setOpen(false); };
    const esc = e => { if (e.key === 'Escape') setOpen(false); };
    addEventListener('mousedown', away);
    addEventListener('keydown', esc);
    return () => { removeEventListener('mousedown', away); removeEventListener('keydown', esc); };
  }, [open]);
  return html`<span class="menu" ref=${box}>
    <button type="button" title=${title} disabled=${disabled} aria-haspopup="true" aria-expanded=${open} onClick=${() => setOpen(!open)}>${label} ▾</button>
    ${open && html`<div class="pop ${narrow ? 'narrow' : ''}">${children(() => setOpen(false))}</div>`}
  </span>`;
}

// QueryTools sits next to the query box of Explore's Metrics and Logs.
export function QueryTools({ type, me, admin, query, onPick, onNote }) {
  const { data, error, reload } = useLoad(() => api('/api/v1/queries'));
  const [picked, setPicked] = useState(null); // the saved query last chosen or saved, to offer updating it
  const [dialog, setDialog] = useState(null);
  const [filter, setFilter] = useState('');
  const text = query.trim();
  const list = (data || []).filter(q => q.type === type);
  // The query on screen is a saved one if its text is one's: the button says which.
  const current = list.find(q => q.id && q.id === picked && q.query === text) || list.find(q => q.query === text);
  const last = list.find(q => q.id && q.id === picked);

  const remove = async q => {
    if (!confirm(`Delete the saved query “${q.name}”?${q.shared ? ' It is shared: it disappears for everyone.' : ''}`)) return;
    try { await api('/api/v1/queries/' + q.id, { method: 'DELETE' }); reload(); } catch (e) { alert(e.message); }
  };
  const menu = close => {
    const f = filter.trim().toLowerCase();
    const match = q => !f || q.name.toLowerCase().includes(f) || q.query.toLowerCase().includes(f);
    const pick = q => { setPicked(q.id || null); setFilter(''); close(); onPick(q.query); };
    const groups = [
      ['Yours', list.filter(q => q.id && !q.shared && match(q))],
      ['Shared', list.filter(q => q.shared && match(q))],
      ['Comes with vigil', list.filter(q => q.source && match(q))],
    ];
    const names = new Set(list.map(q => q.query));
    const recents = recent(me.user, type).filter(q => !names.has(q) && (!f || q.toLowerCase().includes(f)));
    const first = [...groups.flatMap(g => g[1]), ...recents.map(q => ({ query: q }))][0];
    return html`
      <input ref=${focusOnce} placeholder="Find a saved query" aria-label="Find a saved query" value=${filter} onInput=${e => setFilter(e.target.value)}
        onKeyDown=${e => { if (e.key === 'Enter' && first) pick(first); }} style="width: 100%" />
      <${ErrorBox} error=${error} />
      ${groups.map(([title, items]) => items.length > 0 && html`<div class="group">${title}</div>
        ${items.map(q => html`<div class="item ${q === current ? 'on' : ''}">
          <button type="button" class="what" onClick=${() => pick(q)} title=${q.query}>
            <div>${q.name}${q.source && q.source !== 'starter' && html`<span class="muted"> · ${q.source.replace('integration ', '')}</span>`}
              ${q.shared && q.origin === 'files' && html`<span class="muted" title="Applied from files with vigil apply"> · from files</span>`}</div>
            <div class="q">${q.query}</div></button>
          ${q.id && q.editable && html`<button type="button" class="small" title="Rename, change${admin ? ' or share' : ''}" onClick=${() => { close(); setDialog({ kind: 'edit', q }); }}>Edit</button>
            <button type="button" class="small danger" title="Delete" onClick=${() => remove(q)}>✕</button>`}
        </div>`)}`)}
      ${recents.length > 0 && html`<div class="group">Recent, in this browser</div>
        ${recents.map(q => html`<div class="item"><button type="button" class="what" onClick=${() => pick({ query: q })} title=${q}><div class="q">${q}</div></button></div>`)}`}
      ${data && !first && html`<div class="empty" style="padding: 16px">${f ? 'Nothing matches.' : 'Nothing saved yet. Run a query and press Save.'}</div>`}`;
  };

  return html`
    <${Popover} label=${current ? '★ ' + (current.name.length > 28 ? current.name.slice(0, 27) + '…' : current.name) : 'Saved'}
      title=${current ? `This is the saved query “${current.name}”. Open to choose another.` : 'Choose a saved or recent query'}>${menu}<//>
    <button type="button" disabled=${!text} title="Keep this query under a name" onClick=${() => setDialog({ kind: 'save' })}>Save</button>
    ${admin && html`<${Popover} label="Add to" narrow disabled=${!text} title="Put this query on a dashboard, or make an alert of it">${close => html`
      <div class="item"><button type="button" class="what" onClick=${() => { close(); setDialog({ kind: 'dashboard' }); }}>Dashboard…</button></div>
      <div class="item"><button type="button" class="what" onClick=${() => { close(); setDialog({ kind: 'alert' }); }}>Alert rules…</button></div>`}<//>`}
    ${dialog?.kind === 'save' && html`<${SaveDialog} type=${type} query=${text} admin=${admin} update=${last && last.editable && last.query !== text ? last : null}
      onClose=${() => setDialog(null)} onSaved=${q => { setDialog(null); setPicked(q.id); reload(); }} />`}
    ${dialog?.kind === 'edit' && html`<${SaveDialog} type=${type} edit=${dialog.q} admin=${admin}
      onClose=${() => setDialog(null)} onSaved=${() => { setDialog(null); reload(); }} />`}
    ${dialog?.kind === 'dashboard' && html`<${AddToDashboard} type=${type} query=${text} name=${current ? current.name : ''}
      onClose=${() => setDialog(null)} onDone=${d => { setDialog(null); onNote(html`Added to the dashboard <a href=${'#/dashboards/' + d.id}>${d.title}</a>.`); }} />`}
    ${dialog?.kind === 'alert' && html`<${AlertDialog} type=${type} query=${text} name=${current ? current.name : ''}
      onClose=${() => setDialog(null)} onDone=${a => { setDialog(null); onNote(html`The alert <strong>${a.alert}</strong> was added to the group ${a.group} and is being checked. <a href="#/alerts/rules">See the rules</a>.`); }} />`}`;
}

// SaveDialog saves the query on screen (as a new one, or over the one it
// was loaded from), or edits a saved one in place when edit is given.
function SaveDialog({ type, query, update, edit, admin, onClose, onSaved }) {
  const [over, setOver] = useState(!!update); // replace the query it came from
  const [name, setName] = useState(edit ? edit.name : '');
  const [text, setText] = useState(edit ? edit.query : query);
  const [shared, setShared] = useState(edit ? edit.shared : false);
  const [error, setError] = useState(null);
  const [busy, setBusy] = useState(false);
  const submit = async e => {
    e.preventDefault();
    setBusy(true); setError(null);
    try {
      const q = edit ? await api('/api/v1/queries/' + edit.id, { method: 'PUT', body: { name, query: text, ...(admin ? { shared } : {}) } })
        : over ? await api('/api/v1/queries/' + update.id, { method: 'PUT', body: { query: text } })
        : await api('/api/v1/queries', { method: 'POST', body: { name, type, query: text, shared } });
      onSaved(q);
    } catch (err) { setError(err.message); setBusy(false); }
  };
  return html`<${Dialog} title=${edit ? 'Saved query' : 'Save this query'} onClose=${onClose}><form onSubmit=${submit}>
    ${update && html`<div class="field">
      <label style="color: var(--text)"><input type="radio" name="how" checked=${over} onChange=${() => setOver(true)} /> Update “${update.name}”${update.shared ? ' (shared with everyone)' : ''}</label>
      <label style="color: var(--text)"><input type="radio" name="how" checked=${!over} onChange=${() => setOver(false)} /> Save as a new query</label></div>`}
    ${!(update && over) && html`<div class="field"><label>Name</label>
      <input required ref=${focusOnce} maxlength="100" value=${name} onInput=${e => setName(e.target.value)} placeholder="What it shows, e.g. CPU per host" /></div>`}
    <div class="field"><label>${type === 'logs' ? 'LogsQL' : 'PromQL'}</label>
      ${edit ? html`<textarea rows="3" spellcheck="false" required value=${text} onInput=${e => setText(e.target.value)}></textarea>`
        : html`<div class="secret">${text}</div>`}</div>
    ${admin && !(update && over) && html`<div class="field"><label style="color: var(--text)">
      <input type="checkbox" checked=${shared} onChange=${e => setShared(e.target.checked)} /> Shared: everyone sees it, admins can change it</label></div>`}
    ${!admin && !edit && html`<p class="muted" style="font-size: 12.5px">Only you see it. An admin can save one for everyone.</p>`}
    <${ErrorBox} error=${error} />
    <div class="actions"><button type="button" onClick=${onClose}>Cancel</button><button class="primary" disabled=${busy}>Save</button></div>
  </form><//>`;
}

// AddToDashboard makes a panel of the query, in the same editor a
// dashboard uses, and appends it to a dashboard or starts a new one.
function AddToDashboard({ type, query, name, onClose, onDone }) {
  const { data, error: loadError } = useLoad(() => api('/api/v1/dashboards'));
  const [target, setTarget] = useState('');
  const [title, setTitle] = useState('');
  const [error, setError] = useState(null);
  // Plain LogsQL gives lines; with | stats it gives numbers, like PromQL.
  const panel = type !== 'logs' ? { title: name, type: 'timeseries', w: 6, h: 3, queries: [{ expr: query }] }
    : /\|\s*stats\b/.test(query) ? { title: name, type: 'table', source: 'logs', w: 6, h: 3, queries: [{ expr: query }] }
    : { title: name, type: 'logs', w: 12, h: 4, limit: 50, queries: [{ expr: query }] };
  const add = async p => {
    setError(null);
    try {
      if (target === '+') {
        let id = slug(title);
        if (data.some(d => d.id === id)) id += '-' + Math.random().toString(36).slice(2, 6);
        await api('/api/v1/dashboards/' + id, { method: 'PUT', body: { title: title.trim(), panels: [p] } });
        return onDone({ id, title: title.trim() });
      }
      // Read it now, not when the dialog opened: someone may have changed it since.
      const d = await api('/api/v1/dashboards/' + target);
      d.panels.push(p);
      await api('/api/v1/dashboards/' + target, { method: 'PUT', body: d });
      onDone({ id: target, title: d.title });
    } catch (e) { setError(e.message); }
  };
  const head = html`<div class="row" style="align-items: flex-end">
    <div class="field" style="flex: 1; min-width: 200px"><label>Dashboard</label>
      <select required value=${target} onChange=${e => setTarget(e.target.value)}>
        <option value="">Choose…</option>
        ${(data || []).map(d => html`<option value=${d.id}>${d.title}</option>`)}
        <option value="+">A new dashboard…</option></select></div>
    ${target === '+' && html`<div class="field" style="flex: 1; min-width: 200px"><label>Its title</label>
      <input required maxlength="100" value=${title} onInput=${e => setTitle(e.target.value)} /></div>`}
  </div>`;
  return html`<${PanelEditor} panel=${panel} heading="Add to a dashboard" action="Add to dashboard" head=${head} error=${error || loadError}
    onClose=${onClose} onSave=${add} />`;
}

const FOR = [['', 'at once'], ['1m', 'for 1 minute'], ['5m', 'for 5 minutes'], ['10m', 'for 10 minutes'], ['15m', 'for 15 minutes'], ['30m', 'for 30 minutes'], ['1h', 'for 1 hour']];
const WINDOWS = ['1m', '5m', '10m', '15m', '30m', '1h'];
const OPS = [['', 'returns anything'], ['>', 'is above'], ['>=', 'is at least'], ['<', 'is below'], ['<=', 'is at most'], ['==', 'equals'], ['!=', 'is not']];

// "CPU used per host (%)" → CpuUsedPerHost, the usual shape of an alert name.
const alertName = s => s.split(/[^A-Za-z0-9]+/).filter(Boolean).map(w => w[0].toUpperCase() + w.slice(1).toLowerCase()).join('').slice(0, 60);
const yamlQuote = s => "'" + s.replaceAll("'", "''") + "'";

// AlertDialog turns the query into an alert rule and adds it to the rule
// file. The server checks the whole file before it changes anything.
function AlertDialog({ type, query, name, onClose, onDone }) {
  const logs = type === 'logs';
  const stats = /\|\s*stats\b/.test(query);
  const { data: groups } = useLoad(() => api('/api/v1/rules/config/groups'));
  const [alert_, setAlert] = useState(() => alertName(name));
  const [group, setGroup] = useState(null);
  const [op, setOp] = useState('');
  const [value, setValue] = useState('');
  const [win, setWin] = useState('5m');
  const [lines, setLines] = useState('0');
  const [custom, setCustom] = useState(null); // the expression once it was edited by hand
  const [hold, setHold] = useState(logs ? '' : '5m'); // a log rule already looks at a stretch of time
  const [severity, setSeverity] = useState('warning');
  const [summary, setSummary] = useState('');
  const [test, setTest] = useState(null);
  const [error, setError] = useState(null);
  const [busy, setBusy] = useState(false);

  // What the controls above the expression make of the query.
  const simple = /^[A-Za-z_:][\w:]*(\{[^{}]*\})?$/.test(query);
  const built = !logs ? (op && value !== '' ? `${simple ? query : `(${query})`} ${op} ${value}` : query)
    : stats ? (/(^|\s)_time:/.test(query) ? query : `_time:${win} ${query}`)
    : `_time:${win} ${query} | stats count() as lines | filter lines:>${lines || 0}`;
  const expr = custom ?? built;
  const mine = (groups || []).filter(g => g.type === type);
  const inGroup = group ?? (mine[0] ? mine[0].name : logs ? 'log-alerts' : 'alerts');

  // Run it as the rule would be run, to show what it would do right now.
  useEffect(() => {
    let live = true;
    const timer = setTimeout(async () => {
      if (!expr.trim()) return setTest(null);
      const form = o => ({ method: 'POST', body: new URLSearchParams(o) });
      try {
        const r = logs ? await api('/api/v1/logs/stats_query', form({ query: expr, time: Date.now() / 1000 }))
          : await api('/api/v1/query', form({ query: expr }));
        const rows = Array.isArray(r.data.result) && r.data.resultType !== 'scalar' ? r.data.result : [];
        if (live) setTest({ n: rows.length, some: rows.slice(0, 4).map(s => ({
          labels: Object.entries(s.metric || {}).filter(([k]) => k !== '__name__').sort().map(([k, v]) => `${k}="${v}"`).join(', '), value: s.value[1] })) });
      } catch (e) { if (live) setTest({ error: e.message }); }
    }, 500);
    return () => { live = false; clearTimeout(timer); };
  }, [expr]);

  const submit = async e => {
    e.preventDefault();
    setBusy(true); setError(null);
    try {
      await api('/api/v1/rules/config/alerts', { method: 'POST', body: { group: inGroup, type, alert: alert_, expr, for: hold, severity, summary } });
      onDone({ alert: alert_.trim(), group: inGroup });
    } catch (err) { setError(err.message); setBusy(false); }
  };
  const yaml = [`- alert: ${alert_ || '…'}`,
    expr.includes('\n') ? '  expr: |\n' + expr.split('\n').map(l => '    ' + l).join('\n') : `  expr: ${yamlQuote(expr)}`,
    hold && `  for: ${hold}`, severity && `  labels: {severity: ${severity}}`,
    summary && `  annotations:\n    summary: ${yamlQuote(summary)}`].filter(Boolean).join('\n');

  return html`<${Dialog} title="Make an alert rule" wide onClose=${onClose}><form onSubmit=${submit}>
    <div class="row" style="align-items: flex-end">
      <div class="field" style="flex: 2; min-width: 200px"><label>Name of the alert</label>
        <input required ref=${focusOnce} maxlength="100" pattern="[A-Za-z_][A-Za-z0-9_]*" title="Letters, digits and _, without spaces" value=${alert_} onInput=${e => setAlert(e.target.value)} placeholder="e.g. DiskAlmostFull" /></div>
      <div class="field" style="flex: 1; min-width: 160px"><label>Group in the rule file</label>
        <input required maxlength="100" list="rule-groups" value=${inGroup} onInput=${e => setGroup(e.target.value)} />
        <datalist id="rule-groups">${mine.map(g => html`<option value=${g.name} />`)}</datalist></div>
    </div>
    ${custom === null && (!logs ? html`<div class="row" style="margin-bottom: 12px"><span>Alert when the value</span>
        <select aria-label="Condition" value=${op} onChange=${e => setOp(e.target.value)}>${OPS.map(([v, l]) => html`<option value=${v}>${l}</option>`)}</select>
        ${op && html`<input type="number" step="any" required aria-label="Threshold" value=${value} onInput=${e => setValue(e.target.value)} style="width: 120px" />`}</div>`
      : html`<div class="row" style="margin-bottom: 12px">
        ${stats ? html`<span>Alert for every row the query returns, looking at the last</span>` : html`<span>Alert when more than</span>
          <input type="number" min="0" required aria-label="Lines" value=${lines} onInput=${e => setLines(e.target.value)} style="width: 100px" /><span>lines match in the last</span>`}
        <select aria-label="Window" value=${win} onChange=${e => setWin(e.target.value)} disabled=${stats && /(^|\s)_time:/.test(query)}>${WINDOWS.map(w => html`<option value=${w}>${w}</option>`)}</select></div>`)}
    <div class="field"><label>Expression: every ${logs ? 'row' : 'series'} it returns is one alert
        ${custom !== null && html` · <button type="button" class="link" onClick=${() => setCustom(null)}>start again from the query</button>`}</label>
      <textarea rows="3" spellcheck="false" required aria-label="Expression" value=${expr} onInput=${e => setCustom(e.target.value)}></textarea></div>
    ${test && (test.error ? html`<div class="error">${test.error}</div>` : html`<div class="panel" style="margin: 0 0 12px">
      ${test.n === 0 ? html`Right now it returns nothing: no alert would fire.`
        : html`Right now it returns <strong>${test.n}</strong> ${logs ? 'row' : 'series'}${test.n > 1 ? (logs ? 's' : '') : ''}: ${test.n > 1 ? 'each would be an alert' : 'it would be an alert'}${hold ? `, once it has lasted ${hold}` : ''}.
          ${test.some.map(s => html`<div class="mono" style="font-size: 12px; overflow-wrap: anywhere">${s.labels || '(no labels)'} <span class="muted">= ${s.value}</span></div>`)}
          ${test.n > test.some.length && html`<div class="muted" style="font-size: 12px">and ${test.n - test.some.length} more</div>`}
          ${!logs && !op && custom === null && html`<div class="muted" style="font-size: 12.5px; margin-top: 6px">A query that always returns a value is always an alert. Give it a condition above.</div>`}`}
    </div>`)}
    <div class="row" style="align-items: flex-end">
      <div class="field"><label>It has to last</label><select value=${hold} onChange=${e => setHold(e.target.value)}>${FOR.map(([v, l]) => html`<option value=${v}>${l}</option>`)}</select></div>
      <div class="field"><label>Severity</label><select value=${severity} onChange=${e => setSeverity(e.target.value)}>
        ${['info', 'warning', 'critical'].map(v => html`<option value=${v}>${v}</option>`)}</select></div>
      <div class="field" style="flex: 1; min-width: 240px"><label>Message (optional)</label>
        <input value=${summary} onInput=${e => setSummary(e.target.value)} placeholder="e.g. {{ $labels.agent }} is at {{ $value | humanize }}" /></div>
    </div>
    <details style="margin-bottom: 8px"><summary class="muted" style="cursor: pointer; font-size: 12.5px">As YAML, for a rule file that is kept in git</summary>
      <div class="secret" style="margin-top: 6px">${yaml}</div></details>
    <${ErrorBox} error=${error} />
    <div class="actions"><button type="button" onClick=${onClose}>Cancel</button><button class="primary" disabled=${busy}>${busy ? 'Checking…' : 'Add the alert'}</button></div>
  </form><//>`;
}
