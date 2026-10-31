import { html, useState, useEffect, api, useLoad, Badge, ErrorBox, Dialog, ago } from './lib.js';

// Home: what needs attention, what will soon, and what happened since you
// last looked. It reads one prepared answer from the server
// (/api/v1/briefing), kept up to date in the background, so it opens fast
// and asks the stores nothing; the links lead to the pages that do.

const SEVERITY = { critical: 'bad', error: 'bad', warning: 'warn', info: 'info' };
const KIND = { host_down: 'host down', security: 'security', slo: 'SLO', alert: 'alert' };
const n = (count, one, many = one + 's') => `${count} ${count === 1 ? one : many}`;
const usd = v => '$' + (v >= 100 ? v.toFixed(0) : v >= 1 ? v.toFixed(2) : v.toFixed(3));
const q = s => JSON.stringify(String(s));
// act makes a link do something without leaving the page.
const act = fn => e => { e.preventDefault(); fn(); };

function greeting(user) {
  const h = new Date().getHours();
  const part = h < 5 ? 'Good night' : h < 12 ? 'Good morning' : h < 18 ? 'Good afternoon' : 'Good evening';
  return `${part}, ${user}.`;
}

function since(iso) {
  return ago(iso).replace(' ago', '');
}

// Where to look next for an incident: its capture, its logs, its check.
function links(inc) {
  const l = inc.labels || {}, out = [];
  if (inc.annotations?.capture_id) out.push(['Capture', '#/captures/' + inc.annotations.capture_id]);
  if (l.container && l.agent) out.push(['Logs', `#/explore/logs?q=${encodeURIComponent(`agent:=${q(l.agent)} container:=${q(l.container)}`)}&last=3600`]);
  else if (inc.kind === 'security' && l.agent) out.push(['Events', `#/explore/logs?q=${encodeURIComponent(`source:=security agent:=${q(l.agent)} rule:=${q(l.alertname)}`)}&last=86400`]);
  if (l.probe) out.push(['Site checks', '#/dashboards/checks']);
  if (inc.kind === 'slo' && l.slo) out.push(['SLO', '#/slos/' + encodeURIComponent(l.slo)]);
  if (l.agent && inc.kind === 'host_down') out.push(['Fleet', '#/fleet']);
  return out;
}

function IncidentRow({ inc, admin, ai, onAck, onOpen, depth = 0 }) {
  const [open, setOpen] = useState(false);
  const kids = inc.children || [];
  return html`<div class="incident" style=${depth ? 'margin-left: 22px' : ''}>
    <div class="row" style="gap: 8px; align-items: baseline">
      <${Badge} kind=${SEVERITY[inc.severity] || 'off'}>${inc.severity || KIND[inc.kind]}<//>
      <a href="#" onClick=${act(() => onOpen(inc.id))} style="font-weight: 600">${inc.title}</a>
      ${inc.kind !== 'alert' && html`<${Badge} kind="info">${KIND[inc.kind]}<//>`}
      ${inc.silenced && html`<${Badge}>silenced<//>`}
      <span class="spacer"></span>
      <span class="muted" title=${new Date(inc.first_seen).toLocaleString()}>for ${since(inc.first_seen)}</span>
    </div>
    <div class="muted" style="font-size: 13px; margin: 2px 0 0 2px">
      ${inc.resource}${inc.reopened > 0 ? ` · came back ${inc.reopened}×` : ''}
      ${inc.state === 'acknowledged' && html` · <strong>${inc.ack_by}</strong> is on it${inc.ack_comment ? ': ' + inc.ack_comment : ''}`}
      ${links(inc).map(([label, href]) => html` · <a href=${href}>${label}</a>`)}
      ${admin && inc.state === 'open' && html` · <a href="#" onClick=${act(() => onAck(inc))}>I'm on it</a>`}
      ${admin && ai?.enabled && html` · <a href="#" onClick=${act(() => onOpen(inc.id, true))}>Explain</a>`}
      ${kids.length > 0 && html` · <a href="#" onClick=${act(() => setOpen(!open))}>${open ? 'hide' : 'show'} ${kids.length} more on it</a>`}
    </div>
    ${open && kids.map(k => html`<${IncidentRow} key=${k.id} inc=${k} admin=${admin} ai=${ai} onAck=${onAck} onOpen=${onOpen} depth=${depth + 1} />`)}
  </div>`;
}

function History({ id, onClose, admin, ai, explain }) {
  const { data, error } = useLoad(() => api('/api/v1/incidents/' + id), [id]);
  const inc = data?.incident;
  return html`<${Dialog} title=${inc ? inc.title : 'Incident'} wide onClose=${onClose}>
    <${ErrorBox} error=${error} />
    ${inc && html`
      <p class="muted">${inc.resource} · ${inc.state} · first seen ${new Date(inc.first_seen).toLocaleString()}</p>
      <${Explanations} id=${id} admin=${admin} ai=${ai} start=${explain} />
      <details style="margin-top: 10px"><summary class="muted" style="cursor: pointer">History (${data.events.length})</summary>
        <div class="panel flush" style="margin-top: 6px"><table><tbody>${data.events.map(e => html`<tr>
          <td style="white-space: nowrap">${new Date(e.time).toLocaleString()}</td><td>${e.type}</td><td class="muted">${e.detail}</td></tr>`)}</tbody></table></div></details>
      <details style="margin-top: 10px"><summary class="muted" style="cursor: pointer">Labels and annotations</summary>
        <pre class="secret" style="user-select: text">${JSON.stringify({ labels: inc.labels, annotations: inc.annotations }, null, 2)}</pre></details>`}
    <div class="actions"><span class="spacer"></span><button onClick=${onClose}>Close</button></div>
  <//>`;
}

// Explanations: the latest one, or a preview of what would be sent and a
// button to send it. Nothing leaves the server before that button.
function Explanations({ id, admin, ai, start }) {
  const { data: list, error, reload } = useLoad(() => api(`/api/v1/incidents/${id}/explanations`), [id]);
  const [preview, setPreview] = useState(null);
  const [busy, setBusy] = useState(false);
  const [err, setErr] = useState(null);
  const canAsk = admin && ai?.enabled;
  const showPreview = async () => {
    setErr(null);
    try { setPreview(await api(`/api/v1/incidents/${id}/explain`)); } catch (e) { setErr(e.message); }
  };
  useEffect(() => { if (start && canAsk && list && !list.length) showPreview(); }, [list]);
  const send = async () => {
    setBusy(true); setErr(null);
    try { await api(`/api/v1/incidents/${id}/explain`, { method: 'POST' }); setPreview(null); await reload(); }
    catch (e) { setErr(e.message); await reload(); }
    setBusy(false);
  };
  if (!list) return html`<${ErrorBox} error=${error} />`;
  const latest = list[0];
  return html`<div class="panel" style="margin: 0 0 6px">
    <div class="row" style="margin-bottom: 6px"><h3 style="margin: 0">Explanation</h3><span class="spacer"></span>
      ${canAsk && !preview && html`<button class="small" onClick=${showPreview}>${latest ? 'Explain again' : 'Explain'}</button>`}</div>
    <${ErrorBox} error=${err} />
    ${preview ? html`<${Preview} p=${preview} busy=${busy} onSend=${send} onCancel=${() => setPreview(null)} />`
      : latest ? html`<${Answer} ex=${latest} onVerdict=${reload} />`
      : html`<p class="muted" style="margin: 0">${!ai?.enabled ? 'No AI provider is set up on this server (ai in the server config).'
          : admin ? 'Not explained yet.' : 'Not explained yet. An admin can ask for one.'}</p>`}
  </div>`;
}

export function Preview({ p, busy, onSend, onCancel }) {
  const { ai, bundle } = p;
  const left = ai.monthly_budget - ai.spent_this_month;
  return html`
    <p style="margin-top: 0">This is what would be sent to <strong>${ai.provider}</strong> (${ai.model}), masked: ${bundle.items.length} items, ${Math.round(bundle.bytes / 1024)} kB. Sent classes: ${ai.data.join(', ')}.</p>
    ${bundle.items.map(e => html`<details style="margin-bottom: 4px"><summary style="cursor: pointer">
      <code>${e.id}</code> ${e.title} <span class="muted">· ${e.class} · ${e.text.length} chars</span></summary>
      <pre class="secret" style="user-select: text; max-height: 220px; overflow: auto">${e.text}</pre></details>`)}
    ${bundle.withheld?.length > 0 && html`<p class="muted">Not sent: ${bundle.withheld.join('; ')}</p>`}
    <p class="muted">This month: ${ai.spent_this_month.toFixed(2)} of ${ai.monthly_budget.toFixed(2)} spent.</p>
    ${ai.paused && html`<div class="error">AI explanations are paused.</div>`}
    <div class="row" style="gap: 8px"><span class="spacer"></span><button onClick=${onCancel}>Cancel</button>
      <button class="primary" disabled=${busy || ai.paused || left <= 0} onClick=${onSend}>${busy ? 'Waiting for the answer…' : 'Send and explain'}</button></div>`;
}

const CONF = { high: 'ok', medium: 'warn', low: 'off' };
// What an explanation of a log line says about it.
const ASSESS = { harmless: ['ok', 'harmless'], needs_attention: ['bad', 'needs attention'], unknown: ['off', 'unclear'] };

export function Answer({ ex, onVerdict }) {
  const a = ex.answer;
  const [shown, setShown] = useState(null);
  const [picked, setPicked] = useState(null);
  const [note, setNote] = useState('');
  const ev = Object.fromEntries((ex.evidence || []).map(e => [e.id, e]));
  const verdict = async (v, text = '') => {
    await api(`/api/v1/explanations/${ex.id}/verdict`, { method: 'POST', body: { verdict: v, note: text } });
    setPicked(null); onVerdict();
  };
  if (!a) return html`<div class="error">The explanation failed: ${ex.error}</div>`;
  return html`
    <p style="margin-top: 0">${a.assessment && ASSESS[a.assessment] && html`<${Badge} kind=${ASSESS[a.assessment][0]}>${ASSESS[a.assessment][1]}<//> `}<strong>${a.summary}</strong></p>
    <p>Likely cause: ${a.hypothesis} <${Badge} kind=${CONF[a.confidence] || 'off'}>${a.confidence} confidence<//></p>
    ${a.claims.length > 0 && html`<ul style="margin: 6px 0">${a.claims.map(c => html`<li>
      ${c.text} ${c.evidence.map(id => html` <a href="#" onClick=${act(() => setShown(shown === id ? null : id))}><code>${id}</code></a>`)}
      ${c.interpretation && html` <${Badge}>interpretation<//>`}
      ${c.unsupported?.length > 0 && html` <${Badge} kind="bad">cites evidence that was not sent: ${c.unsupported.join(', ')}<//>`}</li>`)}</ul>`}
    ${shown && ev[shown] && html`<div class="panel" style="margin: 6px 0"><strong>${shown}: ${ev[shown].title}</strong>
      <pre class="secret" style="user-select: text; max-height: 220px; overflow: auto">${ev[shown].text}</pre></div>`}
    ${a.unknowns.length > 0 && html`<p style="margin-bottom: 2px"><strong>Unknown</strong></p><ul style="margin-top: 0">${a.unknowns.map(u => html`<li>${u}</li>`)}</ul>`}
    ${a.next_checks.length > 0 && html`<p style="margin-bottom: 2px"><strong>Next checks</strong></p><ul style="margin-top: 0">${a.next_checks.map(u => html`<li>${u}</li>`)}</ul>`}
    <p class="muted" style="font-size: 12px">Written by ${ex.model} for ${ex.user}, ${ago(ex.created)} · ${(ex.latency_ms / 1000).toFixed(1)} s ·
      cost ${ex.cost.toFixed(4)} · from the evidence above only; check before acting.</p>
    <div class="row" style="gap: 6px; flex-wrap: wrap">
      ${ex.verdict ? html`<span class="muted">Marked <strong>${ex.verdict}</strong> by ${ex.verdict_by}${ex.verdict_note ? ': ' + ex.verdict_note : ''}</span>`
        : html`<span class="muted">Was it right?</span>
          ${['right', 'partly', 'wrong', 'unhelpful'].map(v => html`<button class=${'small' + (picked === v ? ' primary' : '')}
            onClick=${() => (v === 'right' ? verdict(v) : setPicked(v))}>${v}</button>`)}`}
    </div>
    ${picked && html`<div class="row" style="gap: 6px; margin-top: 6px">
      <input style="flex: 1" maxlength="1000" value=${note} onInput=${e => setNote(e.target.value)} placeholder="What was it really? (optional)" />
      <button class="small primary" onClick=${() => verdict(picked, note)}>Save</button></div>`}`;
}

function Ack({ inc, onClose }) {
  const [comment, setComment] = useState('');
  const [error, setError] = useState(null);
  const save = async () => {
    try { await api(`/api/v1/incidents/${inc.id}/ack`, { method: 'POST', body: { comment } }); onClose(true); } catch (e) { setError(e.message); }
  };
  return html`<${Dialog} title="I'm on it" onClose=${() => onClose(false)}>
    <p>${inc.title}</p>
    <div class="field"><label>A note for the others (optional)</label>
      <input value=${comment} maxlength="500" onInput=${e => setComment(e.target.value)} placeholder="e.g. restarting it, cleaning up /data" /></div>
    <${ErrorBox} error=${error} />
    <div class="actions"><span class="spacer"></span><button onClick=${() => onClose(false)}>Cancel</button>
      <button class="primary" onClick=${save}>Acknowledge</button></div>
  <//>`;
}

function Section({ title, children, right }) {
  return html`<div class="panel" style="margin-bottom: 14px">
    <div class="row" style="margin-bottom: 8px"><h3 style="margin: 0">${title}</h3><span class="spacer"></span>${right}</div>
    ${children}</div>`;
}

export function Home({ me, admin }) {
  const { data: b, error, reload } = useLoad(() => api('/api/v1/briefing'), [], 30000);
  const [history, setHistory] = useState(null); // {id, explain}
  const { data: ai, reload: reloadAI } = useLoad(() => api('/api/v1/ai'), []);
  const pause = paused => api('/api/v1/ai/paused', { method: 'PUT', body: { paused } }).then(reloadAI);
  const [ack, setAck] = useState(null);
  const [showSilenced, setShowSilenced] = useState(false);
  if (!b) return html`<${ErrorBox} error=${error} />${!error && html`<div class="empty">Loading…</div>`}`;

  const cov = b.coverage, s = b.since;
  const loud = b.open.filter(i => !i.silenced), silenced = b.open.filter(i => i.silenced);
  const seen = () => api('/api/v1/briefing/seen', { method: 'POST', body: { event_id: s.latest } }).then(reload);
  const open = (id, explain = false) => setHistory({ id, explain });
  const row = inc => html`<${IncidentRow} key=${inc.id} inc=${inc} admin=${admin} ai=${ai} onAck=${setAck} onOpen=${open} />`;

  return html`
    <div class="row" style="margin-bottom: 6px; align-items: baseline">
      <h2 style="margin: 0">${greeting(b.user === 'admin-token' ? me.user : b.user)}</h2><span class="spacer"></span>
      <span class="muted">updated ${ago(b.generated)}</span>
    </div>
    <p style="margin: 0 0 14px">
      ${s.after === 0 ? 'Here is where things stand.'
        : s.opened + s.resolved === 0 ? 'Nothing new since you last looked.'
        : html`Since you last looked: <strong>${s.opened}</strong> opened, <strong>${s.resolved}</strong> resolved.`}
      ${s.latest > s.after && html` <button class="small" onClick=${seen}>Mark as seen</button>`}
    </p>
    <${ErrorBox} error=${error} />
    ${cov.sync?.error && html`<div class="error">Incidents are not being updated: ${cov.sync.error}</div>`}
    <div class="row" style="gap: 8px; flex-wrap: wrap; margin-bottom: 14px">
      <${Badge} kind=${!cov.agents ? 'off' : cov.silent.length ? 'warn' : 'ok'}>${cov.reporting} of ${n(cov.agents, 'agent')} reporting<//>
      ${cov.silent.length > 0 && html`<span class="muted">silent: ${cov.silent.join(', ')}</span>`}
      ${cov.rules_failing.length > 0 && html`<a href="#/alerts"><${Badge} kind="bad">${n(cov.rules_failing.length, 'rule')} cannot be evaluated<//></a>`}
      ${cov.silences > 0 && html`<a href="#/alerts"><${Badge}>${n(cov.silences, 'silence')} in effect<//></a>`}
      ${ai?.enabled && html`<span class="spacer"></span>
        <span class="muted" title=${`${ai.provider} ${ai.model}; sends: ${ai.data.join(', ')}`}>AI explanations ${ai.paused ? 'paused' : 'on'}, ${usd(ai.spent_this_month)} of ${usd(ai.monthly_budget)} this month</span>
        ${admin && html`<button class="small" onClick=${() => pause(!ai.paused)}>${ai.paused ? 'Resume' : 'Pause'}</button>`}`}
    </div>

    <${Section} title="Needs attention">
      ${loud.length ? loud.map(row) : html`<div class="empty" style="padding: 18px">
        ✓ Nothing needs attention${cov.silent.length ? ` — but ${n(cov.silent.length, 'agent')} ${cov.silent.length === 1 ? 'is' : 'are'} silent, so this may not be the whole picture` : '.'}</div>`}
      ${silenced.length > 0 && html`<p class="muted" style="margin: 10px 0 0">
        <a href="#" onClick=${act(() => setShowSilenced(!showSilenced))}>${showSilenced ? 'Hide' : 'Show'} ${silenced.length} silenced</a></p>
        ${showSilenced && silenced.map(row)}`}
    <//>

    ${(b.coming.items.length > 0 || b.coming.error) && html`<${Section} title="Coming up">
      ${b.coming.error && html`<p class="muted" style="margin-top: 0" title=${b.coming.error}>Some forecasts could not be made: the metric store did not answer.</p>`}
      ${b.coming.items.map(c => html`<div class="row" style="gap: 8px; margin-bottom: 6px">
        <${Badge} kind=${SEVERITY[c.severity] || 'info'}>${c.severity === 'warning' ? 'soon' : 'later'}<//>
        <span>${c.title}</span><span class="spacer"></span>
        ${c.link && html`<a href=${c.link}>open</a>`}
        ${c.query && html`<a href=${'#/explore/metrics?q=' + encodeURIComponent(c.query) + '&last=604800'}>chart</a>`}</div>`)}
    <//>`}

    ${b.spend && html`<${Section} title="AI spend, last 24 hours" right=${html`<a href="#/dashboards/ai-costs">AI costs</a>`}>
      <div class="row" style="gap: 24px; flex-wrap: wrap">
        <div><strong style="font-size: 20px">${usd(b.spend.billed)}</strong> <span class="muted">billed${b.spend.usual_billed > 0 ? `, usually ${usd(b.spend.usual_billed)} a day` : ''}</span></div>
        ${b.spend.subscription > 0 && html`<div><strong style="font-size: 20px">${usd(b.spend.subscription)}</strong> <span class="muted">of subscription use at API prices, not billed</span></div>`}
        ${b.spend.unpriced > 0 && html`<${Badge} kind="warn">${b.spend.unpriced} models or GPUs without a price<//>`}
      </div>
    <//>`}

    ${b.noisy.length > 0 && html`<${Section} title="Alerts that keep coming back">
      <p class="muted" style="margin-top: 0">Fired at least 5 times this week. Worth a look: a longer <code>for</code> or <code>keep_firing_for</code>,
        a different threshold, a fix, or a silence. The history tells which.</p>
      ${b.noisy.map(n => html`<div style="margin-bottom: 4px">
        <a href="#" onClick=${act(() => open(n.incident))}>${n.alertname}</a> on ${n.resource}:
        <strong>${n.firings}</strong> times${n.median_minutes ? `, usually for ${Math.round(n.median_minutes)} min` : ''}</div>`)}
    <//>`}

    ${b.resolved.length > 0 && html`<${Section} title="Resolved in the last 24 hours">
      ${b.resolved.map(r => html`<div class="row" style="gap: 8px; margin-bottom: 4px">
        <${Badge} kind="ok">resolved<//><a href="#" onClick=${act(() => open(r.id))}>${r.title}</a>
        <span class="spacer"></span><span class="muted">${ago(r.resolved_at)}</span></div>`)}
    <//>`}

    ${history && html`<${History} id=${history.id} explain=${history.explain} admin=${admin} ai=${ai} onClose=${() => setHistory(null)} />`}
    ${ack && html`<${Ack} inc=${ack} onClose=${done => { setAck(null); if (done) reload(); }} />`}`;
}
