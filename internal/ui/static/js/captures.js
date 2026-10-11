import { html, useState, useMemo, api, useLoad, Badge, ErrorBox, ago } from './lib.js';

const SEVERITY = { critical: 'bad', warning: 'warn', info: 'info' };
const TYPES = ['exec', 'exit', 'connect', 'listen', 'open', 'setuid'];
const PAGE = 2000;

export function Captures({ route, admin }) {
  return route.parts[1]
    ? html`<${CaptureView} key=${route.parts[1]} id=${route.parts[1]} admin=${admin} />`
    : html`<${CaptureList} />`;
}

function CaptureList() {
  const { data, error } = useLoad(() => api('/api/v1/captures'), [], 15000);
  return html`
    <div class="head"><h1>Captures</h1><span class="spacer"></span>
      <a href="#/explore/logs?q=source%3Asecurity&last=86400">All detections in Explore</a>
      <a class="btn" href="/docs/security" target="_blank" rel="noopener">Help</a></div>
    <${ErrorBox} error=${error} />
    <div class="panel flush">${!data ? html`<div class="empty">Loading…</div>` : data.length === 0
      ? html`<div class="empty">No captures yet. A capture is saved when a security rule with <code>capture: true</code> fires on an agent.</div>`
      : html`<table>
        <thead><tr><th>When</th><th>Rule</th><th>Agent</th><th>What happened</th><th style="text-align: right">Events</th></tr></thead>
        <tbody>${data.map(c => html`<tr class="click" onClick=${() => { location.hash = '#/captures/' + c.id; }}>
          <td style="white-space: nowrap" title=${new Date(c.time).toLocaleString()}>${ago(c.time)}</td>
          <td><${Badge} kind=${SEVERITY[c.severity] || 'off'}>${c.severity}<//> <strong>${c.rule}</strong></td>
          <td>${c.agent}</td><td>${c.summary}</td><td class="num">${c.events}</td>
        </tr>`)}</tbody></table>`}</div>
    <p class="muted">Each capture holds everything the agent saw on that host from shortly before the detection until shortly after:
      programs started, connections made, watched files opened, and changes of user.</p>`;
}

// One line describing what an event did.
export function describe(ev) {
  switch (ev.type) {
    case 'exec': return (ev.args && ev.args.length ? ev.args.join(' ') : ev.exe) || '';
    case 'exit': return ev.signal ? `killed by signal ${ev.signal}` : `exit code ${ev.exit_code ?? 0}`;
    case 'connect': return `to ${ev.addr}:${ev.port}`;
    case 'listen': return `on ${ev.addr}:${ev.port}`;
    case 'open': return `${ev.path}${ev.write ? '  (for writing)' : ''}`;
    case 'setuid': return `user ${ev.old_euid} → ${ev.new_euid}`;
  }
  return '';
}

function CaptureView({ id, admin }) {
  const { data: cap, error } = useLoad(() => api('/api/v1/captures/' + id));
  const [text, setText] = useState('');
  const [types, setTypes] = useState(() => new Set(TYPES.filter(t => t !== 'exit')));
  const [open, setOpen] = useState(null);
  const [limit, setLimit] = useState(PAGE);

  const t0 = cap ? new Date(cap.meta.time).getTime() : 0;
  const rows = useMemo(() => {
    if (!cap) return [];
    const needle = text.trim().toLowerCase();
    return cap.events.filter(ev => types.has(ev.type) && (!needle ||
      `${ev.comm} ${ev.exe || ''} ${ev.pid} ${ev.container || ''} ${describe(ev)} ${ev.parent_comm || ''}`.toLowerCase().includes(needle)));
  }, [cap, text, types]);
  const isTrigger = ev => ev.time === cap.trigger.time && ev.pid === cap.trigger.pid && ev.type === cap.trigger.type;
  const toggle = t => { const s = new Set(types); s.has(t) ? s.delete(t) : s.add(t); setTypes(s); };
  const remove = async () => {
    if (!confirm('Delete this capture? This cannot be undone.')) return;
    try { await api('/api/v1/captures/' + id, { method: 'DELETE' }); location.hash = '#/captures'; } catch (e) { alert(e.message); }
  };
  const rel = ev => { const d = (new Date(ev.time).getTime() - t0) / 1000; return (d >= 0 ? '+' : '−') + Math.abs(d).toFixed(3) + 's'; };

  if (error) return html`<div class="head"><h1>Capture</h1></div><${ErrorBox} error=${error} /><a href="#/captures">Back to captures</a>`;
  if (!cap) return html`<div class="empty">Loading…</div>`;
  const m = cap.meta;
  return html`
    <div class="head"><a href="#/captures">← Captures</a><h1>${m.rule}</h1>
      <${Badge} kind=${SEVERITY[m.severity] || 'off'}>${m.severity}<//><span class="spacer"></span>
      <a class="btn" href=${'/api/v1/captures/' + id} download=${`capture-${id}.json`}>Download</a>
      ${admin && html`<button class="danger" onClick=${remove}>Delete</button>`}</div>
    <div class="panel">
      <div><strong>${m.summary}</strong></div>
      <div class="muted">${new Date(m.time).toLocaleString()} · ${m.events} events from ${Math.round((t0 - new Date(m.from).getTime()) / 1000)}s before
        to ${Math.round((new Date(m.until).getTime() - t0) / 1000)}s after${m.truncated ? ' · cut short: the host was too busy to keep everything' : ''}</div>
    </div>
    <div class="row" style="margin-bottom: 12px">
      <input style="flex: 1; min-width: 200px" placeholder="Filter by process, path, address, container, pid…" aria-label="Filter events"
        value=${text} onInput=${e => { setText(e.target.value); setLimit(PAGE); }} />
      ${TYPES.map(t => html`<label style="margin: 0; white-space: nowrap"><input type="checkbox" checked=${types.has(t)} onChange=${() => toggle(t)} /> ${t}</label>`)}
    </div>
    <div class="panel flush"><table class="fixed events">
      <thead><tr><th style="width: 96px">Time</th><th style="width: 82px">Event</th><th style="width: 24%">Process</th>
        <th style="width: 15%">Container</th><th>What</th></tr></thead>
      <tbody>${rows.length === 0 && html`<tr><td colspan="5" class="empty">No events match.</td></tr>`}
      ${rows.slice(0, limit).map((ev, i) => html`
        <tr class="click ${isTrigger(ev) ? 'trigger' : ''}" onClick=${() => setOpen(open === i ? null : i)}>
          <td class="mono" title=${ev.time}>${rel(ev)}</td>
          <td><span class="badge off">${ev.type}</span></td>
          <td class="mono"><strong>${ev.comm}</strong> <span class="muted">${ev.pid}</span>
            ${ev.parent_comm && html`<div class="muted">from ${ev.parent_comm}</div>`}</td>
          <td class="mono">${ev.container || html`<span class="muted">host</span>`}</td>
          <td class="mono">${isTrigger(ev) && html`<${Badge} kind=${SEVERITY[m.severity] || 'off'}>detected<//> `}${describe(ev)}</td>
        </tr>
        ${open === i && html`<tr><td colspan="5"><pre class="mono" style="margin: 0; white-space: pre-wrap; overflow-wrap: anywhere">${JSON.stringify(ev, null, 2)}</pre></td></tr>`}`)}
      </tbody></table>
      ${rows.length > limit && html`<div class="empty" style="padding: 10px">Showing ${limit} of ${rows.length} events.${' '}
        <button class="small" onClick=${() => setLimit(limit + PAGE * 5)}>Show more</button></div>`}
    </div>
    <p class="muted">Times are relative to the detected event. Process-exit events are hidden until you tick “exit”. Click a row for every field.</p>`;
}
