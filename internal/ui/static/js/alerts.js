import { html, useState, api, useLoad, Tabs, Badge, Chips, ErrorBox, Dialog, YamlEditor, ago, until, fmtNum } from './lib.js';

const AM = '/alertmanager/api/v2';
const fingerprint = labels => Object.entries(labels).sort().map(([k, v]) => k + '=' + v).join('\u0001');

export function Alerts({ route, admin }) {
  const tabs = [['active', 'Alerts'], ['rules', 'Rules'], ['silences', 'Silences']];
  if (admin) tabs.push(['contacts', 'Contacts & routing']);
  const tab = tabs.some(t => t[0] === route.parts[1]) ? route.parts[1] : 'active';
  const View = { active: Active, rules: Rules, silences: Silences, contacts: Contacts }[tab];
  return html`
    <div class="head"><h1>Alerts</h1><span class="spacer"></span>
      <a class="btn" href="/docs/alerts" target="_blank" rel="noopener">Help</a></div>
    <${Tabs} base="alerts" current=${tab} tabs=${tabs} />
    <${View} admin=${admin} />`;
}

function Active({ admin }) {
  const { data, error, reload } = useLoad(async () => {
    const [rules, am] = await Promise.all([api('/api/v1/rules'), api(AM + '/alerts')]);
    const silenced = new Set(am.filter(a => a.status.state === 'suppressed').map(a => fingerprint(a.labels)));
    const out = [], fromRules = new Set();
    for (const g of rules.data.groups) for (const r of g.rules) for (const a of r.alerts || []) {
      fromRules.add(fingerprint(a.labels));
      out.push({ ...a, group: g.name, silenced: silenced.has(fingerprint(a.labels)) });
    }
    // Runtime detections are raised by agents directly, not by a rule here.
    for (const a of am) {
      if (fromRules.has(fingerprint(a.labels))) continue;
      out.push({ labels: a.labels, annotations: a.annotations, state: 'firing', activeAt: a.startsAt,
        silenced: a.status.state === 'suppressed' });
    }
    const rank = a => (a.state === 'firing' ? (a.silenced ? 1 : 0) : 2);
    return out.sort((a, b) => rank(a) - rank(b) || a.labels.alertname.localeCompare(b.labels.alertname));
  }, [], 10000);
  const [silence, setSilence] = useState(null);

  return html`
    <${ErrorBox} error=${error} />
    <div class="panel flush">${!data ? html`<div class="empty">Loading…</div>` : data.length === 0
      ? html`<div class="empty">Nothing is firing or pending.</div>`
      : html`<table class="fixed">
        <thead><tr><th style="width: 96px">State</th><th style="width: 38%">Alert</th><th>Labels</th><th style="width: 86px">Since</th>
          <th style="width: 90px; text-align: right">Value</th><th style="width: 92px"></th></tr></thead>
        <tbody>${data.map(a => html`<tr>
          <td>${a.silenced ? html`<${Badge} kind="off">silenced<//>` : a.state === 'firing' ? html`<${Badge} kind="bad">firing<//>` : html`<${Badge} kind="warn">pending<//>`}</td>
          <td><strong>${a.labels.alertname}</strong>
            ${a.labels.severity && html` <span class="muted">${a.labels.severity}</span>`}
            <div class="muted">${a.annotations.summary || a.annotations.description || ''}
              ${a.annotations.capture_id && html` · <a href="#/captures/${a.annotations.capture_id}">open capture</a>`}</div></td>
          <td><${Chips} labels=${a.labels} skip=${['alertname', 'severity']} /></td>
          <td style="white-space: nowrap">${ago(a.activeAt)}</td>
          <td class="num mono">${a.value === undefined ? '' : fmtNum(a.value)}</td>
          <td style="text-align: right">${admin && !a.silenced && html`<button class="small" onClick=${() => setSilence(a.labels)}>Silence</button>`}</td>
        </tr>`)}</tbody></table>`}</div>
    <p class="muted">Pending alerts have met their condition but not yet for as long as the rule requires. This list refreshes every 10 seconds.</p>
    ${silence && html`<${SilenceForm} labels=${silence} onClose=${() => { setSilence(null); reload(); }} />`}`;
}

function Rules({ admin }) {
  const { data, error, reload } = useLoad(() => api('/api/v1/rules'), [], 15000);
  const [editing, setEditing] = useState(false);
  const groups = data ? data.data.groups : null;
  const state = r => r.health === 'err' ? html`<${Badge} kind="bad">error<//>`
    : r.type === 'recording' ? html`<${Badge} kind="info">recording<//>`
    : r.state === 'firing' ? html`<${Badge} kind="bad">firing<//>`
    : r.state === 'pending' ? html`<${Badge} kind="warn">pending<//>`
    : r.health === 'unknown' ? html`<${Badge} kind="off">not run yet<//>` : html`<${Badge} kind="ok">ok<//>`;

  return html`
    <div class="row" style="margin-bottom: 14px">
      <span class="muted">Prometheus rule-file format. A group with <code>type: logs</code> uses LogsQL.</span><span class="spacer"></span>
      ${admin && html`<button class="primary" onClick=${() => setEditing(true)}>Edit rule file</button>`}
    </div>
    <${ErrorBox} error=${error} />
    ${groups && groups.length === 0 && html`<div class="panel empty">No rules yet.${admin && ' Use “Edit rule file” to add some.'}</div>`}
    ${(groups || []).map(g => html`<div class="panel flush">
      <div style="padding: 10px 14px; border-bottom: 1px solid var(--border)"><strong>${g.name}</strong>
        <span class="muted"> · ${g.type} · every ${g.interval}s${g.name === 'builtin' && ' · comes with vigil; an alert of the same name in the rule file replaces one'}</span></div>
      <table><tbody>${g.rules.map(r => html`<tr>
        <td style="width: 110px">${state(r)}</td>
        <td><strong>${r.name}</strong>${r.duration > 0 && html`<span class="muted"> · for ${r.duration}s</span>`}
          <div class="mono muted" style="overflow-wrap: anywhere">${r.query}</div>
          ${r.lastError && html`<div class="error">${r.lastError}</div>`}</td>
        <td class="muted" style="white-space: nowrap; text-align: right">${r.health === 'unknown' ? '' : 'checked ' + ago(r.lastEvaluation)}</td>
      </tr>`)}</tbody></table></div>`)}
    ${editing && html`<${YamlEditor} title="Rule file"
      help="Saving replaces all rules. Every expression is run once against its store first; if anything is wrong, nothing changes and the reason is shown."
      load=${() => api('/api/v1/rules/config')} save=${text => api('/api/v1/rules/config', { method: 'PUT', body: text })}
      onClose=${saved => { setEditing(false); if (saved) reload(); }} />`}`;
}

function Silences({ admin }) {
  const { data, error, reload } = useLoad(() => api(AM + '/silences'), [], 15000);
  const [adding, setAdding] = useState(false);
  const [showExpired, setShowExpired] = useState(false);
  const expire = async s => {
    if (!confirm('End this silence now? Matching alerts will notify again.')) return;
    try { await api(`${AM}/silence/${s.id}`, { method: 'DELETE' }); reload(); } catch (e) { alert(e.message); }
  };
  const list = (data || []).filter(s => showExpired || s.status.state !== 'expired')
    .sort((a, b) => (a.status.state === 'expired') - (b.status.state === 'expired') || a.endsAt.localeCompare(b.endsAt));
  const matcher = m => `${m.name}${m.isEqual === false ? '!' : ''}${m.isRegex ? '=~' : '='}${m.value}`;

  return html`
    <div class="row" style="margin-bottom: 14px">
      <label style="margin: 0"><input type="checkbox" checked=${showExpired} onChange=${e => setShowExpired(e.target.checked)} /> Show ended silences</label>
      <span class="spacer"></span>
      ${admin && html`<button class="primary" onClick=${() => setAdding(true)}>New silence</button>`}
    </div>
    <${ErrorBox} error=${error} />
    <div class="panel flush">${list.length === 0 ? html`<div class="empty">${data ? 'No silences.' : 'Loading…'}</div>` : html`<table>
      <thead><tr><th>State</th><th>Matches alerts where</th><th>Reason</th><th>By</th><th>Ends</th><th></th></tr></thead>
      <tbody>${list.map(s => html`<tr>
        <td><${Badge} kind=${s.status.state === 'active' ? 'info' : 'off'}>${s.status.state === 'expired' ? 'ended' : s.status.state}<//></td>
        <td>${s.matchers.map(m => html`<span class="chip">${matcher(m)}</span>`)}</td>
        <td>${s.comment}</td><td>${s.createdBy}</td>
        <td style="white-space: nowrap" title=${new Date(s.endsAt).toLocaleString()}>${until(s.endsAt)}</td>
        <td style="text-align: right">${admin && s.status.state !== 'expired' && html`<button class="small" onClick=${() => expire(s)}>End now</button>`}</td>
      </tr>`)}</tbody></table>`}</div>
    ${adding && html`<${SilenceForm} labels=${{}} onClose=${() => { setAdding(false); reload(); }} />`}`;
}

const DURATIONS = [['1 hour', 1], ['2 hours', 2], ['4 hours', 4], ['8 hours', 8], ['1 day', 24], ['3 days', 72], ['1 week', 168]];

// SilenceForm creates a silence. Matchers are one per line: label=value,
// label!=value, label=~regex or label!~regex.
function SilenceForm({ labels, onClose }) {
  const [error, setError] = useState(null);
  const initial = Object.entries(labels).sort().map(([k, v]) => `${k}=${v}`).join('\n');
  const submit = async e => {
    e.preventDefault();
    const f = new FormData(e.target);
    const matchers = [];
    for (const line of f.get('matchers').split('\n').map(l => l.trim()).filter(Boolean)) {
      const m = line.match(/^([A-Za-z_][A-Za-z0-9_]*)\s*(=~|!~|!=|=)\s*"?(.*?)"?$/);
      if (!m) return setError(`Cannot read “${line}”. Write one matcher per line, like  alertname=HostDown  or  agent=~web-.*`);
      matchers.push({ name: m[1], value: m[3], isRegex: m[2].includes('~'), isEqual: !m[2].startsWith('!') });
    }
    if (!matchers.length) return setError('Add at least one matcher, otherwise the silence would cover every alert.');
    const now = new Date();
    try {
      const me = await api('/api/v1/me');
      await api(AM + '/silences', { method: 'POST', body: {
        matchers, startsAt: now.toISOString(), endsAt: new Date(now.getTime() + f.get('hours') * 3600e3).toISOString(),
        createdBy: me.user, comment: f.get('comment'),
      } });
      onClose();
    } catch (err) { setError(err.message); }
  };
  return html`<${Dialog} title="Silence alerts" onClose=${onClose}><form onSubmit=${submit}>
    <div class="field"><label>Silence alerts whose labels match all of these (one per line)</label>
      <textarea name="matchers" rows="5" spellcheck="false" required>${initial}</textarea></div>
    <div class="field"><label>For how long</label><select name="hours">${DURATIONS.map(([l, h]) => html`<option value=${h} selected=${h === 2}>${l}</option>`)}</select></div>
    <div class="field"><label>Reason</label><input name="comment" required placeholder="Why these alerts can be ignored for now" /></div>
    <${ErrorBox} error=${error} />
    <div class="actions"><button type="button" onClick=${onClose}>Cancel</button><button class="primary">Silence</button></div>
  </form><//>`;
}

function Contacts() {
  const { data: receivers, error, reload } = useLoad(() => api(AM + '/receivers'));
  const [editing, setEditing] = useState(false);
  return html`
    <div class="row" style="margin-bottom: 14px">
      <span class="muted">Who is told about which alert. This is Alertmanager's configuration format.</span><span class="spacer"></span>
      <button class="primary" onClick=${() => setEditing(true)}>Edit contacts & routing</button>
    </div>
    <${ErrorBox} error=${error} />
    <div class="panel"><h2>Contacts in use</h2>
      ${(receivers || []).map(r => html`<span class="chip">${r.name}</span>`)}
      ${receivers && receivers.length === 1 && receivers[0].name === 'none' && html`
        <p class="muted" style="margin-bottom: 0">Only the placeholder contact exists, so no notifications are sent yet.</p>`}
    </div>
    ${editing && html`<${YamlEditor} title="Contacts & routing"
      help="Routes, receivers (email, Slack, webhook, PagerDuty, …), inhibit rules and time intervals. Passwords and webhook URLs in here are stored on the server as written."
      load=${() => api('/api/v1/alerting/config')} save=${text => api('/api/v1/alerting/config', { method: 'PUT', body: text })}
      onClose=${saved => { setEditing(false); if (saved) reload(); }} />`}`;
}
