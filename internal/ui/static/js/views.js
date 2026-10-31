import { html, useState, useRef, api, useLoad, Badge, ErrorBox, Dialog, ago } from './lib.js';
import { PanelBody, ROW } from './dashboards.js';

// A view from a prompt: an admin asks for something to look at, the model
// writes a dashboard (views.go), vigil runs its queries and draws it here
// over the last hour. "Change …" asks again from the current draft; Save
// makes it an ordinary dashboard, after which no model is involved.

const STATUS = { ok: ['ok', 'ok'], no_data: ['info', 'no data'], invalid: ['bad', 'invalid'], slow: ['warn', 'too slow'],
  too_large: ['warn', 'too many series'], unavailable: ['warn', 'store unavailable'] };
const EXAMPLES = ['CPU and memory of every host', 'container restarts in the last day, by host', 'error log lines with their rate'];
const usd = v => '$' + (v >= 1 ? v.toFixed(2) : v.toFixed(3));

export function ViewPrompt({ ai, onSpend }) {
  const [draft, setDraft] = useState(null);
  const [ask, setAsk] = useState('');
  const [busy, setBusy] = useState(false);
  const [error, setError] = useState(null);
  const [showSent, setShowSent] = useState(false);
  const [saving, setSaving] = useState(false);
  const abort = useRef(null);
  const { data: drafts, reload } = useLoad(() => api('/api/v1/views'), []);
  const blocked = ai.paused ? 'AI is paused.' : ai.spent_this_month >= ai.monthly_budget ? "This month's AI budget is used up." : '';

  const send = async e => {
    e && e.preventDefault();
    if (!ask.trim() || busy) return;
    setBusy(true); setError(null);
    abort.current = new AbortController();
    try {
      const path = draft ? `/api/v1/views/${draft.id}/turns` : '/api/v1/views';
      const d = await api(path, { method: 'POST', body: { ask, revision: draft ? draft.revision : 0 }, signal: abort.current.signal });
      setDraft(d); setAsk(''); reload();
    } catch (err) { setError(err.name === 'AbortError' ? 'Cancelled. What the model had already used is counted.' : err.message); }
    setBusy(false);
    onSpend && onSpend(); // a turn costs something, a cancelled one too
  };
  const open = id => api('/api/v1/views/' + id).then(d => { setDraft(d); setError(null); }, err => setError(err.message));
  const discard = async () => {
    await api('/api/v1/views/' + draft.id, { method: 'DELETE' }).catch(() => {});
    setDraft(null); setError(null); reload();
  };

  const last = draft?.turns[draft.turns.length - 1];
  const spent = draft ? draft.turns.reduce((a, t) => a + t.cost, 0) : 0;
  const others = (drafts || []).filter(d => !draft || d.id !== draft.id);

  return html`<div class="panel" style="margin-bottom: 14px">
    <form class="row" style="gap: 8px" onSubmit=${send}>
      <input style="flex: 1" value=${ask} onInput=${e => setAsk(e.target.value)} maxlength="2000" disabled=${busy || !!blocked}
        aria-label=${draft ? 'Change the view' : 'Ask for a view'}
        placeholder=${blocked || (draft ? 'Change it: “make that per host”, “drop the memory panel”…' : `Show me… (for example: ${EXAMPLES[0]})`)} />
      ${busy ? html`<button type="button" onClick=${() => abort.current?.abort()}>Cancel</button>
          <button class="primary" disabled>Building…</button>`
        : html`<button class="primary" disabled=${!ask.trim() || !!blocked}>${draft ? 'Change' : 'Build view'}</button>`}
    </form>
    <div class="row muted" style="gap: 10px; margin-top: 6px; flex-wrap: wrap; font-size: 13px">
      <span>A view is a dashboard written by ${ai.model}; it is drawn here and saved only when you press Save.</span>
      <a href="#" onClick=${e => { e.preventDefault(); setShowSent(true); }}>What is sent</a>
      ${others.length > 0 && html`<span class="spacer"></span><span>Drafts:</span>
        ${others.slice(0, 5).map(d => html`<a href="#" title=${'changed ' + ago(d.updated)} onClick=${e => { e.preventDefault(); open(d.id); }}>${d.title || 'untitled'}</a>`)}`}
    </div>
    <${ErrorBox} error=${error} />
    ${draft && html`<div style="margin-top: 12px">
      <div class="row" style="gap: 8px; align-items: baseline">
        <h3 style="margin: 0">${draft.dashboard.title || 'Draft'}</h3>
        <span class="muted">draft · ${draft.turns.length} ${draft.turns.length === 1 ? 'request' : 'requests'}, ${usd(spent)}</span>
        <span class="spacer"></span>
        <button onClick=${discard}>Discard</button>
        <button class="primary" disabled=${!draft.dashboard.panels.length || busy} onClick=${() => setSaving(true)}>Save as dashboard</button>
      </div>
      ${last?.error && html`<div class="error">The last request did not change the draft: ${last.error}</div>`}
      ${draft.question && html`<div class="notice"><strong>The model asks:</strong> ${draft.question}</div>`}
      ${draft.note && html`<p style="margin: 6px 0">${draft.note}</p>`}
      ${draft.changes?.length > 0 && !last?.error && html`<p class="muted" style="margin: 4px 0">Changed: ${draft.changes.join('; ')}</p>`}
      <${Panels} draft=${draft} />
    </div>`}
    ${showSent && html`<${Sent} onClose=${() => setShowSent(false)} />`}
    ${saving && html`<${Save} draft=${draft} onClose=${() => setSaving(false)} />`}
  </div>`;
}

function Panels({ draft }) {
  const to = Date.now() / 1000, from = to - 3600;
  const checks = (i, j) => draft.checks.filter(c => c.panel === i && (j === undefined || c.query === j));
  const worst = i => checks(i).map(c => c.status).find(s => s !== 'ok' && s !== 'no_data') || (checks(i).every(c => c.status === 'no_data') ? 'no_data' : 'ok');
  return html`<div class="grid" style="margin-top: 8px">${draft.dashboard.panels.map((p, i) => {
    const w = worst(i), [kind, label] = STATUS[w] || ['info', w];
    return html`<section key=${draft.revision + ':' + i} style="grid-column: span ${p.w}; height: ${p.h * ROW + 22}px" class="panel tile">
      <header><h2>${p.title}</h2><span class="spacer"></span>
        ${w !== 'ok' && html`<${Badge} kind=${kind}>${label}<//>`}</header>
      ${w === 'ok' || w === 'no_data'
        ? html`<${PanelBody} panel=${p} admin=${true} from=${from} to=${to} tick=${draft.revision} variables=${[]} values=${{}} height=${p.h * ROW - 58} onZoom=${() => {}} />`
        : html`<div class="tile-body"><div class="error" style="margin: 0">${checks(i).filter(c => c.status !== 'ok').map(c => c.detail).join('; ')}</div></div>`}
      <details class="muted" style="font-size: 12px; padding: 0 10px 6px"><summary>queries</summary>
        ${p.queries.map((q, j) => html`<div><code>${q.expr}</code> ${checks(i, j).map(c => c.status !== 'ok' && html`<span>(${STATUS[c.status]?.[1] || c.status})</span>`)}</div>`)}</details>
    </section>`; })}</div>`;
}

function Sent({ onClose }) {
  const { data, error } = useLoad(() => api('/api/v1/views/catalog'), []);
  return html`<${Dialog} title="What is sent to the model" wide onClose=${onClose}>
    <p>With every request: your words, the hosts vigil recognised in them (only their names), the current draft, your earlier requests in this draft
      and how its queries did. Always the same catalog below: vigil's metric reference, its query primers and the queries of the dashboards that come
      with vigil and its built-in integrations. No metric values, log lines or lists of hosts.</p>
    <${ErrorBox} error=${error} />
    ${data && html`<p class="muted">${Math.round(data.bytes / 1024)} KB, version ${data.fingerprint}</p>
      <pre style="max-height: 50vh; overflow: auto; white-space: pre-wrap">${data.text}</pre>`}
    <div class="actions"><button onClick=${onClose}>Close</button></div>
  <//>`;
}

function Save({ draft, onClose }) {
  const [title, setTitle] = useState(draft.dashboard.title);
  const [error, setError] = useState(null);
  const [busy, setBusy] = useState(false);
  const save = async e => {
    e.preventDefault(); setBusy(true);
    try {
      const { id } = await api(`/api/v1/views/${draft.id}/save`, { method: 'POST', body: { title } });
      location.hash = '#/dashboards/' + id;
    } catch (err) { setError(err.message); setBusy(false); }
  };
  return html`<${Dialog} title="Save as dashboard" onClose=${onClose}><form onSubmit=${save}>
    <p class="muted">The queries run once more first. It becomes an ordinary dashboard that anyone can open and admins can change; the draft is removed.</p>
    <div class="field"><label>Title</label><input value=${title} onInput=${e => setTitle(e.target.value)} required maxlength="100" autofocus /></div>
    <${ErrorBox} error=${error} />
    <div class="actions"><button type="button" onClick=${onClose}>Cancel</button><button class="primary" disabled=${busy}>${busy ? 'Saving…' : 'Save'}</button></div>
  </form><//>`;
}
