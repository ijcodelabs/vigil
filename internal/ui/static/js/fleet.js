import { html, useState, useEffect, useRef, api, useLoad, Tabs, Badge, Chips, ErrorBox, Dialog, YamlEditor, ago } from './lib.js';

const STARTER = `# Which agents this applies to. All conditions must hold; leave the list
# empty to apply to every agent. Labels come from each agent's own file;
# facts (os, arch, kernel, distro, kubernetes, runtime, ebpf, agent, ...)
# are reported by the agent itself.
select: []
#  - env="prod"
#  - role=~"db|cache"

# Profiles with a higher priority are merged later and win.
priority: 10

# Integrations to switch on for those agents (see the Integrations tab),
# with parameters where the defaults do not fit.
# integrations:
#   postgres: {metrics_target: "localhost:9187"}

# Spread changes to this profile in steps: first one agent, then a quarter
# of them, then everyone. Without "wait", each step waits for Promote.
# rollout:
#   steps: [1, 25%]
#   wait: 10m

# A fragment of agent config. Jobs, log sources and security rules are
# merged by name with those of other profiles and integrations;
# \`enabled: false\` on one switches off what a broader profile defines.
config:
  # interval: 15s
  scrape:
    static: []
    #  - job: node
    #    targets: ["localhost:9100"]
    #    interval: 30s
  logs:
    files: []
    #  - name: nginx_access
    #    paths: [/var/log/nginx/access.log]
`;

const STATE = { 'in sync': 'ok', pending: 'warn', error: 'bad' };

export function Fleet({ route, admin }) {
  const tab = ['profiles', 'integrations', 'enrolment'].includes(route.parts[1]) && (admin || route.parts[1] !== 'enrolment') ? route.parts[1] : 'agents';
  const View = { agents: Agents, profiles: Profiles, integrations: Integrations, enrolment: Enrolment }[tab];
  return html`
    <div class="head"><h1>Fleet</h1><span class="spacer"></span>
      <a class="btn" href="/docs/fleet" target="_blank" rel="noopener">Help</a></div>
    <${Tabs} base="fleet" current=${tab} tabs=${[['agents', 'Agents'], ['profiles', 'Profiles'], ['integrations', 'Integrations'], ...(admin ? [['enrolment', 'Enrolment']] : [])]} />
    <${View} admin=${admin} />`;
}

function Agents({ admin }) {
  const { data: agents, error, reload } = useLoad(() => api('/api/v1/agents'), [], 10000);
  const [dialog, setDialog] = useState(null);
  const [filter, setFilter] = useState('');
  const close = () => { setDialog(null); reload(); };
  const up = agents ? agents.filter(a => a.up).length : 0;
  const needle = filter.trim().toLowerCase();
  const shown = (agents || []).filter(a => !needle ||
    `${a.id} ${a.via ? 'via=' + a.via : ''} ${Object.entries({ ...a.labels, ...a.facts }).map(([k, v]) => k + '=' + v).join(' ')} ${a.profiles.join(' ')}`.toLowerCase().includes(needle));

  return html`
    <div class="row" style="margin-bottom: 14px">
      <input style="flex: 1; max-width: 420px" placeholder="Filter by name, label, fact or profile, e.g. env=prod" aria-label="Filter agents"
        value=${filter} onInput=${e => setFilter(e.target.value)} />
      ${agents && html`<span class="muted">${up} of ${agents.length} reporting</span>`}
      <span class="spacer"></span>
      ${admin && html`<button class="primary" onClick=${() => setDialog({ kind: 'add' })}>Add agent</button>`}
    </div>
    <${ErrorBox} error=${error} />
    <div class="panel flush">${!agents ? html`<div class="empty">Loading…</div>` : agents.length === 0
      ? html`<div class="empty">No agent has reported yet.${admin && ' Use “Add agent” to get a token and the file to start one.'}</div>`
      : html`<table>
        <thead><tr><th>Agent</th><th>Labels</th><th>Host</th><th>Profiles</th><th>Config</th><th></th></tr></thead>
        <tbody>${shown.map(a => html`<tr>
          <td style="white-space: nowrap"><span class="dot ${a.up ? 'ok' : 'bad'}" title=${a.up ? 'reporting' : 'not reporting'}></span><strong>${a.id}</strong>
            <div class="muted" style="font-size: 12px">${ago(a.last_seen)} · ${a.version}${a.via ? html` · <span title="Its data reaches the server through this relay">via ${a.via.split(',').join(' → ')}</span>` : ''}</div></td>
          <td><${Chips} labels=${a.labels} /></td>
          <td class="muted" style="font-size: 12.5px">${a.facts ? html`${a.facts.distro || a.facts.os} ${a.facts.distro_version || ''}<br />${a.facts.kernel || ''} ${a.facts.arch || ''}` : 'not reported yet'}</td>
          <td>${a.profiles.length ? a.profiles.map(p => html`<span class="chip" title=${(a.rollouts || []).includes(p) ? 'This agent has the new version of this profile ahead of the others' : ''}>${p}${(a.rollouts || []).includes(p) ? ' ◆ new' : ''}</span>`) : html`<span class="muted">none: its own file</span>`}
            ${a.unmonitored.length > 0 && html`<div style="margin-top: 4px"><a href="#/fleet/integrations" title="Found running on this host, but no profile switches it on">
              <${Badge} kind="warn">not monitored: ${a.unmonitored.join(', ')}<//></a></div>`}</td>
          <td><${Badge} kind=${STATE[a.config_state]}>${a.config_state}<//>
            ${(a.limited || []).length > 0 && html`<div style="margin-top: 4px"><a href="/docs/install#limits" target="_blank" rel="noopener"
              title="This agent sends more than the server's limits allow; the server takes only that much, the agent queues and then drops the rest">
              <${Badge} kind="bad">over limit: ${a.limited.join(', ')}<//></a></div>`}
            ${a.config_error && html`<div class="error">${a.config_error}</div>`}</td>
          <td style="text-align: right; white-space: nowrap">
            <button class="small" onClick=${() => setDialog({ kind: 'config', agent: a })}>Config</button>${' '}
            ${admin && html`<button class="small" onClick=${() => setDialog({ kind: 'agent', agent: a })}>Manage</button>`}</td>
        </tr>`)}</tbody></table>`}</div>
    <p class="muted">An agent's config is the merge of every profile whose selector matches its labels and facts. With no matching profile
      it uses the file on its host. “Pending” means it has not picked up the latest version yet; agents check about every 30 seconds.</p>

    ${dialog?.kind === 'config' && html`<${EffectiveConfig} agent=${dialog.agent} admin=${admin} onClose=${close}
      onOverride=${() => setDialog({ kind: 'profile', name: 'agent-' + dialog.agent.id.toLowerCase(),
        text: `# Applies to this one agent, on top of everything else.\nselect: ['agent="${dialog.agent.id}"']\npriority: 1000\nconfig:\n  # interval: 5s\n` })} />`}
    ${dialog?.kind === 'profile' && html`<${ProfileEditor} name=${dialog.name} initial=${dialog.text} onClose=${close} />`}
    ${dialog?.kind === 'add' && html`<${AddAgent} onClose=${close} />`}
    ${dialog?.kind === 'agent' && html`<${ManageAgent} agent=${dialog.agent} onClose=${close} />`}`;
}

function EffectiveConfig({ agent, admin, onClose, onOverride }) {
  const { data, error } = useLoad(() => api(`/api/v1/agents/${encodeURIComponent(agent.id)}/config`));
  return html`<${Dialog} title=${'Config of ' + agent.id} wide onClose=${onClose}>
    <${ErrorBox} error=${error || data?.error} />
    ${data && html`
      <p class="muted" style="margin-top: 0">Matched on: <${Chips} labels=${data.attributes} /></p>
      ${data.source === 'local' && !data.error && html`<div class="notice">No profile matches this agent, so it collects what the file on its host says.</div>`}
      ${data.source === 'managed' && html`
        <p>Merged from, in order: ${data.profiles.map((p, i) => html`${i > 0 && ' → '}<span class="chip">${p}</span>`)}</p>
        <div class="split">
          <pre class="secret" style="user-select: text; margin: 0">${data.yaml}</pre>
          <div class="panel flush" style="margin: 0"><table>
            <thead><tr><th>Setting</th><th>Comes from</th></tr></thead>
            <tbody>${data.origins.map(o => html`<tr><td class="mono">${o.path}</td>
              <td>${o.profiles.map((p, i) => html`<span class="chip" style=${i < o.profiles.length - 1 ? 'opacity: .55; text-decoration: line-through' : ''}
                title=${i < o.profiles.length - 1 ? 'changed by a later profile' : 'decides'}>${p}</span>`)}</td></tr>`)}</tbody></table></div>
        </div>`}`}
    <div class="actions">
      ${admin && html`<button onClick=${onOverride}>Override for this agent…</button>`}<span class="spacer"></span>
      <button class="primary" onClick=${onClose}>Close</button></div>
  <//>`;
}

const ROLLOUT_AGENT = { applied: 'ok', pending: 'warn', failed: 'bad', silent: 'bad' };

// One change on its way through the fleet, and what can be done about it.
function Rollout({ r, admin, onChange }) {
  const [error, setError] = useState(null);
  const act = (action, question) => async () => {
    if (question && !confirm(question)) return;
    try { await api(`/api/v1/fleet/rollouts/${encodeURIComponent(r.profile)}/${action}`, { method: 'POST' }); setError(null); onChange(); }
    catch (e) { setError(e.message); }
  };
  const last = r.step >= r.steps_total - 1;
  const state = r.status === 'paused' ? ['bad', 'paused'] : r.ready ? ['info', 'waiting for you'] : ['warn', 'rolling out'];
  return html`<div class="panel">
    <div class="row">
      <strong>${r.profile}</strong> <${Badge} kind=${state[0]}>${state[1]}<//>
      <span class="muted">v${r.from || '–'} → v${r.to} · step ${r.step} of ${r.steps_total}${' '}
        (${[...r.steps, 'everyone'].map((st, i) => i === r.step - 1 ? `[${st}]` : st).join(' → ')}) · started ${ago(r.started)} by ${r.author}</span>
      <span class="spacer"></span>
      ${admin && html`
        ${r.status === 'paused'
          ? html`<button class="small" onClick=${act('resume')}>Resume</button>`
          : html`<button class="small" onClick=${act('pause')}>Pause</button>`}
        <button class="small ${r.ready ? 'primary' : ''}" onClick=${act('promote', r.ready ? null : 'Not every agent of this step has applied the change yet. Go to the next step anyway?')}>
          ${last ? 'Promote to everyone' : 'Promote'}</button>
        ${!last && html`<button class="small" onClick=${act('complete', `Give the new version of ${r.profile} to all ${r.affected} agents now?`)}>Everyone now</button>`}
        <button class="small danger" onClick=${act('abort', r.from
          ? `Abort? Every agent goes back to v${r.from} of ${r.profile}, which is saved again as the newest version.`
          : `Abort? ${r.profile} is new, so it is deleted.`)}>Abort</button>`}
    </div>
    <div style="margin-top: 8px">
      <strong>${r.applied}</strong> of ${r.agents.length} agents in this step have applied it${r.agents.length < r.target ? ` (the step asks for ${r.target}; waiting for more agents to report)` : ''};
      the change affects ${r.affected} in all.
      ${r.next_at && html` Next step ${new Date(r.next_at) > new Date() ? 'at ' + new Date(r.next_at).toLocaleTimeString() : 'now'} if they stay healthy.`}
      ${r.status === 'running' && !r.ready && !r.next_at && r.applied < r.agents.length && ' Agents pick it up within about half a minute.'}
    </div>
    ${r.reason && html`<div class="error" style="margin-top: 8px">${r.reason}</div>`}
    <div style="margin-top: 8px">${r.agents.map(a => html`<span class="chip" title=${a.error || a.state}>
      <span class="dot ${ROLLOUT_AGENT[a.state]}"></span>${a.id}${a.state !== 'applied' ? ' · ' + a.state : ''}</span>`)}</div>
    <${ErrorBox} error=${error} />
  </div>`;
}

function Profiles({ admin }) {
  const { data: profiles, error, reload: reloadProfiles } = useLoad(() => api('/api/v1/fleet/profiles'), [], 15000);
  const { data: rollouts, reload: reloadRollouts } = useLoad(() => api('/api/v1/fleet/rollouts').catch(() => []), [], 5000);
  const reload = () => { reloadProfiles(); reloadRollouts(); };
  const rolling = new Set((rollouts || []).map(r => r.profile));
  const [editing, setEditing] = useState(null); // {name} or {isNew}
  return html`
    ${(rollouts || []).map(r => html`<${Rollout} key=${r.profile} r=${r} admin=${admin} onChange=${reload} />`)}
    <div class="row" style="margin-bottom: 14px">
      <span class="muted">Broad profiles first; a higher priority is merged later and wins.</span><span class="spacer"></span>
      ${admin && html`<button class="primary" onClick=${() => setEditing({ isNew: true })}>New profile</button>`}
    </div>
    <${ErrorBox} error=${error} />
    <div class="panel flush">${!profiles ? html`<div class="empty">Loading…</div>` : profiles.length === 0
      ? html`<div class="empty">No profiles yet, so every agent uses the file on its host.${admin && ' A profile with an empty selector is a good start: it applies to all agents.'}</div>`
      : html`<table>
        <thead><tr><th>Profile</th><th>Applies to</th><th style="text-align: right">Priority</th><th style="text-align: right">Agents</th><th>Last change</th></tr></thead>
        <tbody>${profiles.map(p => html`<tr class="click" onClick=${() => setEditing({ name: p.name })}>
          <td><strong>${p.name}</strong> ${p.origin === 'files' && html`<${Badge} kind="info">from files<//>`}
            ${rolling.has(p.name) && html`<${Badge} kind="warn">rolling out<//>`}</td>
          <td>${p.select.length ? p.select.map(s => html`<span class="chip">${s}</span>`) : html`<span class="muted">every agent</span>`}</td>
          <td class="num">${p.priority}</td><td class="num">${p.matches}</td>
          <td class="muted" style="white-space: nowrap">v${p.version} · ${ago(p.updated)} · ${p.author}</td>
        </tr>`)}</tbody></table>`}</div>
    <p class="muted">Profiles can also be kept as files and applied with <code>vigil apply -f DIR</code>, for example from a git repository's CI.</p>
    ${editing && html`<${ProfileEditor} name=${editing.name} readOnly=${!admin} onClose=${() => { setEditing(null); reload(); }} />`}`;
}

function Integrations({ admin }) {
  const { data, error, reload } = useLoad(async () => {
    const [list, profiles] = await Promise.all([api('/api/v1/fleet/integrations'), api('/api/v1/fleet/profiles')]);
    return { list, profiles };
  }, [], 15000);
  const [dialog, setDialog] = useState(null);
  const close = () => { setDialog(null); reload(); };
  const list = data ? data.list : null;

  // "Enable where detected" opens a profile that follows the detection:
  // any host the technology shows up on later is covered too.
  const enable = it => {
    const existing = data.profiles.find(p => p.name === it.name);
    const params = Object.entries(it.params || {});
    const text = `# ${it.title}: switched on wherever it is found running, now and later.
select:
  - detected_${it.name}="true"
priority: 50
integrations:
  ${it.name}:${params.length ? '' : ' {}'}
${params.map(([k, p]) => `    # ${p.description || k}\n    ${k}: ${JSON.stringify(p.default)}`).join('\n')}
config: {}
`;
    setDialog({ kind: 'profile', name: it.name, text: existing ? undefined : text });
  };

  return html`
    <div class="row" style="margin-bottom: 14px">
      <span class="muted">An integration bundles what one technology needs: what to collect, alert rules and a dashboard. A profile switches it on.</span>
      <span class="spacer"></span>
      ${admin && html`<button class="primary" onClick=${() => setDialog({ kind: 'integration', isNew: true })}>New integration</button>`}
    </div>
    <${ErrorBox} error=${error} />
    <div class="panel flush">${!list ? html`<div class="empty">Loading…</div>` : html`<table>
      <thead><tr><th>Integration</th><th>Brings</th><th style="text-align: right">Found on</th><th style="text-align: right">Switched on for</th><th></th></tr></thead>
      <tbody>${list.map(it => html`<tr>
        <td><strong>${it.title}</strong> ${!it.builtin && html`<${Badge} kind="info">yours<//>`}${it.customised && html`<${Badge} kind="info">customised<//>`}
          <div class="muted">${it.description}</div>
          ${it.unmonitored > 0 && html`<div style="margin-top: 4px"><${Badge} kind="warn">running unmonitored on ${it.unmonitored}<//>
            ${' '}<span class="muted">${it.unmonitored_agents.slice(0, 6).join(', ')}${it.unmonitored > 6 ? ', …' : ''}</span></div>`}</td>
        <td class="muted" style="white-space: nowrap">collection${it.has_rules ? ' · rules' : ''}${it.has_dashboard ? ' · dashboard' : ''}${it.queries ? ' · saved queries' : ''}</td>
        <td class="num">${it.detected}</td>
        <td class="num">${it.enabled}${it.profiles.length > 0 && html`<div class="muted" style="font-size: 12px">via ${it.profiles.join(', ')}</div>`}</td>
        <td style="text-align: right; white-space: nowrap">
          <button class="small" onClick=${() => setDialog({ kind: 'integration', name: it.name })}>${admin ? 'View / edit' : 'View'}</button>${' '}
          ${admin && html`<button class="small ${it.unmonitored > 0 ? 'primary' : ''}" onClick=${() => enable(it)}>
            ${data.profiles.some(p => p.name === it.name) ? 'Edit profile' : 'Enable where detected'}</button>`}</td>
      </tr>`)}</tbody></table>`}</div>
    <p class="muted">“Found on” counts agents whose host shows the technology (by process name or container image). Agents report this about
      every five minutes. Metrics usually need the technology's exporter running next to it; each integration's parameters say where it is expected.</p>
    ${dialog?.kind === 'profile' && html`<${ProfileEditor} name=${dialog.name} initial=${dialog.text} onClose=${close} />`}
    ${dialog?.kind === 'integration' && html`<${IntegrationEditor} name=${dialog.name} admin=${admin} onClose=${close} />`}`;
}

const NEW_INTEGRATION = `title: My service
description: What this monitors.

# Signs that it runs on a host. Any one is enough.
detect:
  process: [my-service]
  # image: [my-service]

# Values a profile may set; each needs a default. Use them below as \${name}.
params:
  target:
    default: "localhost:9100"
    description: Where its metrics are served.

# A fragment of agent config, as in a profile.
config:
  scrape:
    static:
      - job: my-service
        targets: ["\${target}"]

# Optional: rule groups (as in the rule file) and a dashboard.
rules: []
`;

function IntegrationEditor({ name, admin, onClose }) {
  const [newName, setNewName] = useState('');
  const [info, setInfo] = useState(null);
  const target = name || newName.trim();
  const remove = async () => {
    if (!confirm(info.builtin ? `Discard this server's changes to ${name}? It applies again as shipped.` : `Delete the integration ${name}?`)) return;
    try { await api('/api/v1/fleet/integrations/' + encodeURIComponent(name), { method: 'DELETE' }); onClose(); } catch (e) { alert(e.message); }
  };
  if (!admin) {
    return html`<${Dialog} title=${'Integration ' + name} wide onClose=${onClose}>
      <${ReadOnly} load=${() => api('/api/v1/fleet/integrations/' + encodeURIComponent(name)).then(i => i.doc)} />
      <div class="actions"><button class="primary" onClick=${onClose}>Close</button></div><//>`;
  }
  return html`<${YamlEditor}
    title=${name ? 'Integration ' + name : 'New integration'}
    help=${name
      ? html`${info?.builtin && !info.customised ? 'This one is built in. Saving a change keeps a customised copy on this server. ' : ''}A change is refused if it would leave an agent with an invalid config.`
      : html`<span>Name: <input value=${newName} onInput=${e => setNewName(e.target.value)} pattern="[a-z][a-z0-9_]*" placeholder="lower-case, e.g. shop_api" autofocus /></span>`}
    load=${async () => {
      if (!name) return NEW_INTEGRATION;
      const i = await api('/api/v1/fleet/integrations/' + encodeURIComponent(name));
      setInfo(i);
      return i.doc;
    }}
    save=${text => {
      if (!target) throw new Error('Give the integration a name.');
      return api('/api/v1/fleet/integrations/' + encodeURIComponent(target), { method: 'PUT', body: text });
    }}
    onClose=${onClose}
    extra=${info && (info.customised || !info.builtin) && html`<button class="danger" onClick=${remove}>${info.builtin ? 'Back to the built-in version' : 'Delete'}</button>`} />`;
}

function ReadOnly({ load }) {
  const { data, error } = useLoad(load);
  return html`<${ErrorBox} error=${error} /><pre class="secret" style="user-select: text; max-height: 60vh; overflow: auto">${data ?? 'Loading…'}</pre>`;
}

// diffLines marks which lines of b are new relative to a, and which of a are gone.
function diffLines(a, b) {
  const A = a.split('\n'), B = b.split('\n');
  const lcs = Array.from({ length: A.length + 1 }, () => new Uint16Array(B.length + 1));
  for (let i = A.length - 1; i >= 0; i--) for (let j = B.length - 1; j >= 0; j--)
    lcs[i][j] = A[i] === B[j] ? lcs[i + 1][j + 1] + 1 : Math.max(lcs[i + 1][j], lcs[i][j + 1]);
  const out = [];
  let i = 0, j = 0;
  while (i < A.length || j < B.length) {
    if (i < A.length && j < B.length && A[i] === B[j]) { out.push([' ', A[i]]); i++; j++; }
    else if (j < B.length && (i === A.length || lcs[i][j + 1] >= lcs[i + 1][j])) out.push(['+', B[j++]]);
    else out.push(['-', A[i++]]);
  }
  return out;
}

function ProfileEditor({ name, initial, readOnly, onClose }) {
  const isNew = !name || initial !== undefined;
  const [newName, setNewName] = useState(name || '');
  const [text, setText] = useState(initial ?? (name ? null : STARTER));
  const [saved, setSaved] = useState(null); // the stored row, if any
  const [error, setError] = useState(null);
  const [preview, setPreview] = useState(null);
  const [versions, setVersions] = useState(null);
  const [compare, setCompare] = useState(null); // {version, doc}
  const [busy, setBusy] = useState(false);
  const [atOnce, setAtOnce] = useState(false);
  const timer = useRef(null);
  const target = name || newName.trim();
  const staged = /^rollout:/m.test(text || '');

  useEffect(() => {
    if (!name) return;
    api('/api/v1/fleet/profiles/' + encodeURIComponent(name)).then(row => {
      setSaved(row);
      setText(row.doc); // an existing profile is edited, not replaced by the template
    }, e => { if (initial === undefined) setError(e.message); });
    api(`/api/v1/fleet/profiles/${encodeURIComponent(name)}/versions`).then(setVersions, () => {});
  }, []);

  // While typing, ask the server which agents the draft reaches and whether
  // every agent would still get a valid config. Nothing is saved.
  useEffect(() => {
    if (text === null) return;
    clearTimeout(timer.current);
    timer.current = setTimeout(() => {
      api('/api/v1/fleet/match', { method: 'POST', body: { name: target || 'preview', doc: text } })
        .then(p => setPreview(p), e => setPreview({ invalid: e.message }));
    }, 500);
    return () => clearTimeout(timer.current);
  }, [text, target]);

  const save = async () => {
    setBusy(true); setError(null);
    try { await api('/api/v1/fleet/profiles/' + encodeURIComponent(target) + (staged && atOnce ? '?rollout=skip' : ''), { method: 'PUT', body: text }); onClose(); }
    catch (e) { setError(e.message); setBusy(false); }
  };
  const remove = async () => {
    if (!confirm(`Delete the profile ${name}? Agents it applies to lose these settings. Its history is kept.`)) return;
    try { await api('/api/v1/fleet/profiles/' + encodeURIComponent(name), { method: 'DELETE' }); onClose(); } catch (e) { setError(e.message); }
  };
  const show = async v => {
    try { setCompare({ version: v, doc: await api(`/api/v1/fleet/profiles/${encodeURIComponent(name)}/versions/${v}`) }); } catch (e) { setError(e.message); }
  };
  const tab = e => {
    if (e.key !== 'Tab' || e.shiftKey) return;
    e.preventDefault();
    e.target.setRangeText('  ', e.target.selectionStart, e.target.selectionEnd, 'end');
    setText(e.target.value);
  };

  return html`<${Dialog} title=${isNew && !saved ? 'New profile' : 'Profile ' + name} wide onClose=${onClose}>
    ${!name && html`<div class="field"><label>Name (lower-case letters, digits, . _ -)</label>
      <input value=${newName} onInput=${e => setNewName(e.target.value)} pattern="[a-z0-9][a-z0-9._\\-]*" autofocus placeholder="e.g. base, prod, databases" /></div>`}
    ${saved?.origin === 'files' && html`<div class="notice">This profile was applied from files. A change made here is overwritten the next time those files are applied.</div>`}
    ${text === null && !error ? html`<div class="empty">Loading…</div>` : html`
      <textarea rows="20" spellcheck="false" readonly=${readOnly} value=${text || ''} onInput=${e => setText(e.target.value)} onKeyDown=${tab} aria-label="Profile"></textarea>`}
    ${preview && (preview.invalid
      ? html`<div class="error">${preview.invalid}</div>`
      : html`<p class="muted" style="margin: 8px 0 0">Applies to <strong>${preview.matched}</strong> of ${preview.of} agents${preview.agents.length ? ': ' : '.'}
          ${preview.agents.slice(0, 12).map(a => html`<span class="chip">${a}</span>`)}${preview.matched > 12 ? ` and ${preview.matched - 12} more` : ''}</p>
        ${preview.problem && html`<div class="error">Saving this would be refused: ${preview.problem}</div>`}`)}
    <${ErrorBox} error=${error} />
    ${versions && versions.length > 0 && html`<details style="margin-top: 12px"><summary class="muted" style="cursor: pointer">History (${versions.length} versions)</summary>
      <div class="panel flush" style="margin: 8px 0 0; max-height: 180px; overflow: auto"><table><tbody>${versions.map(v => html`<tr>
        <td>v${v.version}${saved && v.version === saved.version ? ' (current)' : ''}</td>
        <td class="muted">${v.deleted ? 'deleted' : v.origin === 'files' ? 'applied from files' : 'saved'} by ${v.author}, ${ago(v.updated)}</td>
        <td style="text-align: right">${!v.deleted && html`<button class="small" onClick=${() => show(v.version)}>Compare</button>`}</td></tr>`)}</tbody></table></div>
      ${compare && html`<p class="muted" style="margin: 10px 0 4px">What the editor has that v${compare.version} does not (green), and the reverse (red):</p>
        <pre class="secret diff" style="user-select: text; margin: 0; max-height: 260px; overflow: auto">${diffLines(compare.doc, text || '').map(([op, line]) =>
          html`<div class=${op === '+' ? 'add' : op === '-' ? 'del' : ''}>${op} ${line}</div>`)}</pre>
        ${!readOnly && html`<button class="small" style="margin-top: 6px" onClick=${() => { setText(compare.doc); setCompare(null); }}>Put v${compare.version} back into the editor</button>`}`}
    </details>`}
    <div class="actions">
      ${!readOnly && saved && html`<button class="danger" onClick=${remove}>Delete</button>`}
      ${!readOnly && staged && html`<label style="margin: 0" title="For emergencies: skip the steps this profile asks for">
        <input type="checkbox" checked=${atOnce} onChange=${e => setAtOnce(e.target.checked)} /> Apply to every agent at once</label>`}<span class="spacer"></span>
      <button onClick=${onClose}>${readOnly ? 'Close' : 'Cancel'}</button>
      ${!readOnly && html`<button class="primary" disabled=${busy || text === null || !target} onClick=${save}>${busy ? 'Checking…' : staged && !atOnce ? 'Save and roll out' : 'Save'}</button>`}
    </div>
  <//>`;
}

// What the server says about itself: its address and, when it has its own
// certificate authority, the fingerprint agents check it by.
const serverInfo = () => api('/api/v1/server-info').catch(() => ({}));
const caLine = info => info?.ca_pin ? `\n  ca_pin: ${info.ca_pin}` : '';

function snippet(id, token, info) {
  return `server:
  url: ${location.origin}
  token: ${token}${caLine(info)}
agent_id: ${id}
# Labels say what this host is. Profiles on the server select agents by them.
labels:
  env: prod
  role: web
`;
}

function TokenResult({ id, token, onClose }) {
  const { data: info } = useLoad(serverInfo);
  return html`
    <p>Put this in <code>/etc/vigil/agent.yaml</code> on the host and set the labels. The token is shown only this once.</p>
    <div class="secret">${snippet(id, token, info)}</div>
    <p class="muted">Then start the agent: <code>vigil agent -config /etc/vigil/agent.yaml</code>. It appears here on its first report and
      collects what the profiles matching it say.</p>
    <div class="actions">
      <button onClick=${() => navigator.clipboard?.writeText(snippet(id, token, info))}>Copy</button>
      <button class="primary" onClick=${onClose}>Done</button></div>`;
}

function AddAgent({ onClose }) {
  const [issued, setIssued] = useState(null);
  const [error, setError] = useState(null);
  const submit = async e => {
    e.preventDefault();
    const id = new FormData(e.target).get('id').trim();
    try { setIssued(await api(`/api/v1/agents/${encodeURIComponent(id)}/token`, { method: 'POST' })); }
    catch (err) { setError(err.message); }
  };
  return html`<${Dialog} title="Add agent" onClose=${onClose}>
    ${issued ? html`<${TokenResult} id=${issued.agent_id} token=${issued.token} onClose=${onClose} />` : html`<form onSubmit=${submit}>
      <div class="field"><label>Agent id (usually the host name)</label>
        <input name="id" required autofocus pattern="[A-Za-z0-9._\\-]{1,128}" placeholder="web-01" /></div>
      <p class="muted">This creates a token that only this agent id can use. For many hosts at once, use an
        <a href="#/fleet/enrolment" onClick=${onClose}> enrolment token</a> instead.</p>
      <${ErrorBox} error=${error} />
      <div class="actions"><button type="button" onClick=${onClose}>Cancel</button><button class="primary">Create token</button></div>
    </form>`}
  <//>`;
}

function ManageAgent({ agent, onClose }) {
  const [issued, setIssued] = useState(null);
  const [error, setError] = useState(null);
  const call = (fn, after) => async () => { try { const r = await fn(); after ? after(r) : onClose(); } catch (e) { setError(e.message); } };
  const base = '/api/v1/agents/' + encodeURIComponent(agent.id);
  const issue = call(() => {
    if (agent.has_token && !confirm('Issue a new token? The current one stops working, so the agent must be updated.')) throw new Error('Cancelled.');
    return api(base + '/token', { method: 'POST' });
  }, setIssued);
  const revoke = call(() => {
    if (!confirm('Revoke this agent\'s token? It can then only report with the shared token, if one is configured.')) throw new Error('Cancelled.');
    return api(base + '/token', { method: 'DELETE' });
  });
  const forget = call(() => {
    if (!confirm(`Forget ${agent.id}? Its entry and token are removed. Its stored metrics and logs are kept. If the agent is still running with the shared token it will reappear.`)) throw new Error('Cancelled.');
    return api(base, { method: 'DELETE' });
  });
  return html`<${Dialog} title=${agent.id} onClose=${onClose}>
    ${issued ? html`<${TokenResult} id=${issued.agent_id} token=${issued.token} onClose=${onClose} />` : html`
      <p class="muted">First seen ${new Date(agent.first_seen).toLocaleString()} · last report ${ago(agent.last_seen)} · from ${agent.addr}</p>
      ${agent.facts && html`<p><${Chips} labels=${agent.facts} skip=${['agent']} /></p>`}
      <p>${agent.has_token ? 'This agent has its own token.' : 'This agent reports with the shared ingest token.'}</p>
      <${ErrorBox} error=${error} />
      <div class="actions" style="justify-content: flex-start">
        <button onClick=${issue}>${agent.has_token ? 'Replace token' : 'Issue own token'}</button>
        ${agent.has_token && html`<button onClick=${revoke}>Revoke token</button>`}
        <span class="spacer"></span>
        <button class="danger" onClick=${forget}>Forget agent</button>
      </div>`}
  <//>`;
}

// Enrolment tokens: one token for many hosts, each of which exchanges it
// for a token of its own.
function Enrolment() {
  const { data: list, error, reload } = useLoad(() => api('/api/v1/fleet/enrollment'), [], 15000);
  const { data: info } = useLoad(serverInfo);
  const [dialog, setDialog] = useState(false);
  const [failed, setFailed] = useState(null);
  const revoke = async t => {
    if (!confirm(`Revoke "${t.name}"? Hosts that already enrolled with it keep working; new ones are refused.`)) return;
    try { await api('/api/v1/fleet/enrollment/' + t.id, { method: 'DELETE' }); reload(); } catch (e) { setFailed(e.message); }
  };
  const state = t => t.max_uses && t.uses >= t.max_uses ? ['off', 'used up']
    : t.expires && new Date(t.expires) < new Date() ? ['off', 'expired'] : ['ok', 'active'];
  return html`
    <div class="row" style="margin-bottom: 14px">
      <span class="muted">One token for many hosts: each host exchanges it for a token of its own the first time it starts.</span>
      <span class="spacer"></span>
      <button class="primary" onClick=${() => setDialog(true)}>New enrolment token</button>
    </div>
    <${ErrorBox} error=${error || failed} />
    ${info && !info.encrypted && html`<div class="error" role="status">This server is reached without encryption, so tokens cross the network
      in the clear. Set <code>tls: {self_signed: true}</code> in the server's config, or put a TLS proxy in front.${' '}
      <a href="/docs/install#encryption" target="_blank" rel="noopener">How</a></div>`}
    <div class="panel flush">${!list ? html`<div class="empty">Loading…</div>` : list.length === 0
      ? html`<div class="empty">No enrolment tokens. Create one to add hosts without making a token for each by hand.</div>`
      : html`<table>
        <thead><tr><th>Name</th><th>State</th><th>Used</th><th>Expires</th><th>Created</th><th></th></tr></thead>
        <tbody>${list.map(t => { const [kind, label] = state(t); return html`<tr>
          <td><strong>${t.name}</strong></td>
          <td><${Badge} kind=${kind}>${label}<//></td>
          <td>${t.uses}${t.max_uses ? ' of ' + t.max_uses : ''} ${t.uses === 1 && !t.max_uses ? 'host' : t.max_uses ? '' : 'hosts'}</td>
          <td title=${t.expires && !t.expires.startsWith('0001') ? new Date(t.expires).toLocaleString() : ''}>${!t.expires || t.expires.startsWith('0001') ? 'never' : new Date(t.expires) < new Date() ? ago(t.expires) : new Date(t.expires).toLocaleString()}</td>
          <td title=${new Date(t.created).toLocaleString()}>${ago(t.created)} by ${t.author}</td>
          <td style="text-align: right"><button class="small danger" onClick=${() => revoke(t)}>Revoke</button></td>
        </tr>`; })}</tbody></table>`}</div>
    ${info?.ca_pin && html`<p class="muted">This server's certificate authority: <code>${info.ca_pin}</code>. Agents given this
      fingerprint accept only this server.</p>`}
    ${dialog && html`<${NewEnrolToken} info=${info} onClose=${() => { setDialog(false); reload(); }} />`}`;
}

function NewEnrolToken({ info, onClose }) {
  const [issued, setIssued] = useState(null);
  const [error, setError] = useState(null);
  const [how, setHow] = useState('command');
  const submit = async e => {
    e.preventDefault();
    const f = new FormData(e.target);
    try {
      setIssued(await api('/api/v1/fleet/enrollment', { method: 'POST', body: JSON.stringify({
        name: f.get('name').trim(), valid_hours: Number(f.get('valid')), max_uses: Number(f.get('uses') || 0) }) }));
    } catch (err) { setError(err.message); }
  };
  const pin = info?.ca_pin;
  const text = !issued ? '' : {
    command: `vigil enroll -server ${location.origin} \\\n  -token ${issued.token}${pin ? ` \\\n  -ca-pin ${pin}` : ''} \\\n  -label env=prod -label role=web`,
    file: `server:\n  url: ${location.origin}\n  enroll_token: ${issued.token}${caLine(info)}\n# agent_id defaults to the host name\nlabels:\n  env: prod\n  role: web\n`,
    kubernetes: `kubectl -n vigil create secret generic vigil-agent \\\n  --from-literal=enroll-token=${issued.token}${pin ? ` \\\n  --from-literal=ca-pin=${pin}` : ''}`,
  }[how];
  const note = {
    command: html`Run this on each host, as root. It writes <code>/etc/vigil/agent.yaml</code> with the host's own token; the enrolment token is not kept on the host.`,
    file: html`Or ship the same <code>/etc/vigil/agent.yaml</code> to every host (image, Ansible, cloud-init). Each agent enrols when it first starts and keeps its own token in its spool directory.`,
    kubernetes: html`<code>deploy/k8s-agent.yaml</code> reads this secret; set <code>server.url</code> to <code>${location.origin}</code> in its ConfigMap. Each node enrols under its node name and keeps its token on the node.`,
  }[how];
  return html`<${Dialog} title="New enrolment token" wide=${!!issued} onClose=${onClose}>
    ${issued ? html`
      <p>The token is shown only this once.</p>
      <nav class="tabs">${[['command', 'One command'], ['file', 'Config file'], ['kubernetes', 'Kubernetes']].map(([id, label]) =>
        html`<a class=${how === id ? 'on' : ''} href="#" onClick=${e => { e.preventDefault(); setHow(id); }}>${label}</a>`)}</nav>
      <div class="secret">${text}</div>
      <p class="muted">${note}</p>
      <div class="actions"><button onClick=${() => navigator.clipboard?.writeText(text)}>Copy</button>
        <button class="primary" onClick=${onClose}>Done</button></div>`
    : html`<form onSubmit=${submit}>
      <div class="field"><label>Name (what it is for)</label>
        <input name="name" required autofocus maxlength="100" placeholder="e.g. new rack, October" /></div>
      <div class="field"><label>Valid for</label>
        <select name="valid"><option value="1">1 hour</option><option value="24" selected>1 day</option>
          <option value="168">7 days</option><option value="720">30 days</option><option value="0">until revoked</option></select></div>
      <div class="field"><label>How many hosts may use it (empty: any number)</label>
        <input name="uses" type="number" min="1" placeholder="any number" /></div>
      <p class="muted">Anyone holding this token can add a host under a new name until it expires, so keep it short-lived.
        It cannot read data or replace a host that is already enrolled.</p>
      <${ErrorBox} error=${error} />
      <div class="actions"><button type="button" onClick=${onClose}>Cancel</button><button class="primary">Create token</button></div>
    </form>`}
  <//>`;
}
