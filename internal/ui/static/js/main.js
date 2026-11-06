import { html, render, useState, useEffect, api, setSignedOutHandler, parseHash, useLoad, Dialog, ErrorBox, Badge } from './lib.js';
import { Explore } from './explore.js';
import { Alerts } from './alerts.js';
import { Fleet } from './fleet.js';
import { Captures } from './captures.js';
import { Dashboards } from './dashboards.js';
import { Services } from './services.js';
import { Sessions } from './sessions.js';
import { Home } from './home.js';
import { Secrets } from './secrets.js';
import { SLOs } from './slos.js';

const pages = [
  ['home', 'Home', Home],
  ['dashboards', 'Dashboards', Dashboards],
  ['explore', 'Explore', Explore],
  ['services', 'Services', Services],
  ['slos', 'SLOs', SLOs, 'slo'],
  ['sessions', 'Sessions', Sessions, 'ai'],
  ['alerts', 'Alerts', Alerts],
  ['captures', 'Captures', Captures],
  ['fleet', 'Fleet', Fleet],
  ['users', 'Users', Users, 'admin'],
  ['secrets', 'Secrets', Secrets, 'admin'],
];

// Sidebar icons. Plain inline SVG, one per page plus the footer actions, so
// the collapsed rail reads at a glance. They inherit currentColor, so the
// theme's accent lights the active one.
const S = { viewBox: '0 0 24 24', fill: 'none', stroke: 'currentColor', 'stroke-width': 1.9, 'stroke-linecap': 'round', 'stroke-linejoin': 'round', 'aria-hidden': 'true' };
const icon = {
  home: html`<svg ...${S}><path d="M3 11.5 12 4l9 7.5"/><path d="M5.5 10v9.5h13V10"/></svg>`,
  dashboards: html`<svg ...${S}><rect x="3.5" y="3.5" width="7" height="7" rx="1"/><rect x="13.5" y="3.5" width="7" height="7" rx="1"/><rect x="3.5" y="13.5" width="7" height="7" rx="1"/><rect x="13.5" y="13.5" width="7" height="7" rx="1"/></svg>`,
  explore: html`<svg ...${S}><circle cx="11" cy="11" r="6.5"/><path d="m20 20-3.6-3.6"/></svg>`,
  services: html`<svg ...${S}><path d="M12 3 21 8l-9 5-9-5 9-5Z"/><path d="m3 13 9 5 9-5"/></svg>`,
  slos: html`<svg ...${S}><circle cx="12" cy="12" r="8.5"/><circle cx="12" cy="12" r="4"/></svg>`,
  sessions: html`<svg ...${S}><path d="M20 4H4a1 1 0 0 0-1 1v10a1 1 0 0 0 1 1h3v3.5L11.5 16H20a1 1 0 0 0 1-1V5a1 1 0 0 0-1-1Z"/></svg>`,
  alerts: html`<svg ...${S}><path d="M6 9a6 6 0 0 1 12 0c0 5 2 6 2 6H4s2-1 2-6Z"/><path d="M10.4 19a1.8 1.8 0 0 0 3.2 0"/></svg>`,
  captures: html`<svg ...${S}><path d="M12 3 5 6v5c0 4.5 3 7.5 7 9 4-1.5 7-4.5 7-9V6l-7-3Z"/></svg>`,
  fleet: html`<svg ...${S}><rect x="3.5" y="4" width="17" height="7" rx="1.5"/><rect x="3.5" y="13" width="17" height="7" rx="1.5"/><path d="M7 7.5h.01M7 16.5h.01"/></svg>`,
  users: html`<svg ...${S}><circle cx="12" cy="8" r="3.5"/><path d="M5.5 20c0-3.6 2.9-6 6.5-6s6.5 2.4 6.5 6"/></svg>`,
  secrets: html`<svg ...${S}><rect x="5" y="10.5" width="14" height="9.5" rx="2"/><path d="M8 10.5V8a4 4 0 0 1 8 0v2.5"/></svg>`,
  docs: html`<svg ...${S}><path d="M6 4h9a2 2 0 0 1 2 2v12a2 2 0 0 0-2-2H6z"/><path d="M6 4v14"/></svg>`,
  key: html`<svg ...${S}><circle cx="8.5" cy="8.5" r="3.8"/><path d="m11.2 11.2 7 7"/><path d="m15.5 15.5 2 2M17.5 13.5 19.5 15.5"/></svg>`,
  theme: html`<svg ...${S}><circle cx="12" cy="12" r="8.5"/><path d="M12 3.5v17a8.5 8.5 0 0 0 0-17Z" fill="currentColor" stroke="none"/></svg>`,
  signout: html`<svg ...${S}><path d="M14 5V4a1 1 0 0 0-1-1H5a1 1 0 0 0-1 1v16a1 1 0 0 0 1 1h8a1 1 0 0 0 1-1v-1"/><path d="M10 12h10"/><path d="m17 9 3 3-3 3"/></svg>`,
  collapse: html`<svg ...${S}><path d="m13 7-5 5 5 5"/><path d="m19 7-5 5 5 5"/></svg>`,
  expand: html`<svg ...${S}><path d="m11 7 5 5-5 5"/><path d="m5 7 5 5-5 5"/></svg>`,
};

function App() {
  const [me, setMe] = useState(undefined); // undefined: checking, null: signed out
  const [route, setRoute] = useState({ ...parseHash(), nav: 0 });
  const [dialog, setDialog] = useState(null);
  const [collapsed, setCollapsed] = useState(() => {
    try { return localStorage.getItem('vigil-sidebar') === 'collapsed'; } catch { return false; }
  });

  // Sessions shows while an AI module (an integration named ai_module_…)
  // is switched on somewhere.
  const [ai, setAi] = useState(false);
  // SLOs shows once there is one, and to admins, who make the first.
  const [slos, setSlos] = useState(false);
  const check = () => api('/api/v1/me').then(m => {
    setMe(m);
    api('/api/v1/fleet/integrations').then(list => setAi(list.some(i => i.name.startsWith('ai_module_') && i.enabled > 0)), () => {});
    api('/api/v1/slos').then(list => setSlos(list.length > 0), () => {});
  }, () => setMe(null));
  useEffect(() => {
    setSignedOutHandler(() => setMe(null));
    check();
    // nav counts real navigations (links, back/forward, a pasted URL) so a
    // page can start over from the parameters in the new address.
    let nav = 0;
    const onHash = () => setRoute({ ...parseHash(), nav: ++nav });
    addEventListener('hashchange', onHash);
    return () => removeEventListener('hashchange', onHash);
  }, []);

  if (me === undefined) return null;
  if (me === null) return html`<${Login} onDone=${check} />`;

  const admin = me.role === 'admin';
  const visible = pages.filter(p => !p[3] || (p[3] === 'admin' && admin) || (p[3] === 'ai' && (ai || route.parts[0] === 'sessions')) || (p[3] === 'slo' && (slos || admin || route.parts[0] === 'slos')));
  const [id, , Page] = visible.find(p => p[0] === route.parts[0]) || visible[0];
  const logout = () => api('/api/v1/logout', { method: 'POST' }).then(() => setMe(null));
  const toggleCollapsed = () => setCollapsed(c => {
    const n = !c;
    try { localStorage.setItem('vigil-sidebar', n ? 'collapsed' : 'open'); } catch {}
    return n;
  });

  return html`<div class=${collapsed ? 'shell collapsed' : 'shell'}>
    <aside class="side">
      <div class="brand"><i></i><span class="brandname">vigil</span></div>
      <nav class="nav">${visible.map(([pid, label]) =>
        html`<a class=${pid === id ? 'on' : ''} href="#/${pid}"><span class="ic">${icon[pid]}</span><span class="lbl">${label}</span></a>`)}</nav>
      <span class="spacer"></span>
      <nav class="nav"><a href="/docs/" target="_blank" rel="noopener"><span class="ic">${icon.docs}</span><span class="lbl">Docs</span></a></nav>
      <div class="who">
        <div class="whoami"><strong>${me.user}</strong> <span class="muted">${me.role}</span></div>
        ${me.version && html`<div class="version muted" title="Version of the vigil server">vigil ${me.version}</div>`}
        <div class="sidebtns">
          ${!me.bootstrap && html`<button class="small" title="Change password" onClick=${() => setDialog('password')}><span class="ic">${icon.key}</span><span class="lbl">Password</span></button>`}
          <button class="small" title="Switch theme (dark / light / tempest)" onClick=${cycleTheme}><span class="ic">${icon.theme}</span><span class="lbl">Theme</span></button>
          <button class="small" title="Sign out" onClick=${logout}><span class="ic">${icon.signout}</span><span class="lbl">Sign out</span></button>
          <button class="small" title=${collapsed ? 'Expand sidebar' : 'Collapse sidebar'} onClick=${toggleCollapsed}><span class="ic">${collapsed ? icon.expand : icon.collapse}</span><span class="lbl">Collapse</span></button>
        </div>
      </div>
    </aside>
    <main class="main">
      ${me.bootstrap && id !== 'users' && html`<div class="notice">
        You are signed in with the server's admin token. <a href="#/users">Create a user account</a> to sign in with your own password.</div>`}
      <${Page} route=${route} me=${me} admin=${admin} onAccountsChanged=${check} />
    </main>
    ${dialog === 'password' && html`<${OwnPassword} onClose=${() => setDialog(null)} />`}
  </div>`;
}

// Cycle dark → light → tempest. "tempest" is an ops-console look (see app.css);
// the other two are the original dark and light.
const themes = ['dark', 'light', 'tempest'];
function cycleTheme() {
  const root = document.documentElement;
  const cur = root.dataset.theme || (matchMedia('(prefers-color-scheme: light)').matches ? 'light' : 'dark');
  root.dataset.theme = themes[(themes.indexOf(cur) + 1) % themes.length] || 'dark';
  try { localStorage.setItem('vigil-theme', root.dataset.theme); } catch {}
  dispatchEvent(new Event('themechange'));
}
try {
  const saved = localStorage.getItem('vigil-theme');
  if (saved) document.documentElement.dataset.theme = saved;
} catch {}

function Login({ onDone }) {
  const [error, setError] = useState(null);
  const [busy, setBusy] = useState(false);
  const submit = async e => {
    e.preventDefault();
    setBusy(true); setError(null);
    const f = new FormData(e.target);
    try {
      await api('/api/v1/login', { method: 'POST', body: { username: f.get('username'), password: f.get('password') } });
      onDone();
    } catch (err) { setError(err.message); setBusy(false); }
  };
  return html`<div class="login"><form onSubmit=${submit}>
    <div class="brand"><i></i>vigil</div>
    <div class="field"><label for="u">User name</label><input id="u" name="username" autocomplete="username" autofocus required /></div>
    <div class="field"><label for="p">Password</label><input id="p" name="password" type="password" autocomplete="current-password" required /></div>
    <${ErrorBox} error=${error} />
    <button class="primary" disabled=${busy}>Sign in</button>
    <p class="muted" style="margin: 14px 0 0; font-size: 12.5px">On a new server, sign in as <code>admin</code> with the admin token.</p>
  </form></div>`;
}

function OwnPassword({ onClose }) {
  const [error, setError] = useState(null);
  const submit = async e => {
    e.preventDefault();
    const f = new FormData(e.target);
    try {
      await api('/api/v1/me/password', { method: 'POST', body: { old: f.get('old'), new: f.get('new') } });
      onClose();
    } catch (err) { setError(err.message); }
  };
  return html`<${Dialog} title="Change your password" onClose=${onClose}><form onSubmit=${submit}>
    <div class="field"><label>Current password</label><input name="old" type="password" autocomplete="current-password" required /></div>
    <div class="field"><label>New password (at least 10 characters)</label><input name="new" type="password" autocomplete="new-password" minlength="10" required /></div>
    <${ErrorBox} error=${error} />
    <div class="actions"><button type="button" onClick=${onClose}>Cancel</button><button class="primary">Change password</button></div>
  </form><//>`;
}

function Users({ me, onAccountsChanged }) {
  const { data: users, error, reload } = useLoad(() => api('/api/v1/users'));
  const [edit, setEdit] = useState(null); // {name, role, isNew}
  const [formError, setFormError] = useState(null);

  const save = async e => {
    e.preventDefault();
    const f = new FormData(e.target);
    const name = edit.isNew ? f.get('name').trim() : edit.name;
    try {
      await api('/api/v1/users/' + encodeURIComponent(name), { method: 'PUT', body: { password: f.get('password'), role: f.get('role') } });
      setEdit(null); setFormError(null);
      await reload();
      onAccountsChanged(); // the first account ends the admin-token sign-in
    } catch (err) { setFormError(err.message); }
  };
  const remove = async name => {
    if (!confirm(`Delete user ${name}? They are signed out immediately.`)) return;
    try { await api('/api/v1/users/' + encodeURIComponent(name), { method: 'DELETE' }); reload(); }
    catch (err) { alert(err.message); }
  };
  const first = users && users.length === 0;

  return html`
    <div class="head"><h1>Users</h1><span class="spacer"></span>
      <button class="primary" onClick=${() => { setFormError(null); setEdit({ isNew: true, role: first ? 'admin' : 'viewer' }); }}>Add user</button></div>
    ${first && html`<div class="notice">No accounts yet. Create an admin account for yourself; after that, the admin token
      no longer signs in to this interface and you will be asked to sign in again.</div>`}
    <${ErrorBox} error=${error} />
    <div class="panel flush">${users && users.length ? html`<table>
      <thead><tr><th>User</th><th>Role</th><th></th></tr></thead>
      <tbody>${users.map(u => html`<tr>
        <td>${u.name} ${u.name === me.user && html`<span class="muted">(you)</span>`}</td>
        <td><${Badge} kind=${u.role === 'admin' ? 'info' : 'off'}>${u.role}<//></td>
        <td style="text-align: right"><button class="small" onClick=${() => { setFormError(null); setEdit(u); }}>Edit</button>${' '}
          <button class="small danger" onClick=${() => remove(u.name)}>Delete</button></td>
      </tr>`)}</tbody></table>` : html`<div class="empty">No user accounts.</div>`}</div>
    <p class="muted">Admins can change rules, contacts, silences, agents and users. Viewers can look at everything except contact settings.</p>
    <${Tokens} />
    <${EvidenceGrants} />
    ${edit && html`<${Dialog} title=${edit.isNew ? 'Add user' : 'Edit ' + edit.name} onClose=${() => setEdit(null)}><form onSubmit=${save}>
      ${edit.isNew && html`<div class="field"><label>User name</label><input name="name" required autofocus pattern="[A-Za-z0-9._@\\-]{1,64}" /></div>`}
      <div class="field"><label>${edit.isNew ? 'Password (at least 10 characters)' : 'New password (leave empty to keep the current one)'}</label>
        <input name="password" type="password" autocomplete="new-password" minlength="10" required=${!!edit.isNew} /></div>
      <div class="field"><label>Role</label><select name="role">
        <option value="viewer" selected=${edit.role === 'viewer'}>viewer</option>
        <option value="admin" selected=${edit.role === 'admin'}>admin</option></select></div>
      <${ErrorBox} error=${formError} />
      <div class="actions"><button type="button" onClick=${() => setEdit(null)}>Cancel</button><button class="primary">Save</button></div>
    </form><//>`}`;
}

// Read-only tokens: for Grafana and for scripts, which cannot sign in the
// way a person does.
function Tokens() {
  const { data: list, error, reload } = useLoad(() => api('/api/v1/tokens'), [], 30000);
  const [dialog, setDialog] = useState(false);
  const [issued, setIssued] = useState(null);
  const [formError, setFormError] = useState(null);
  const create = async e => {
    e.preventDefault();
    const f = new FormData(e.target);
    try {
      setIssued(await api('/api/v1/tokens', { method: 'POST', body: { name: f.get('name').trim(), valid_days: Number(f.get('valid')), scope: f.get('scope') } }));
      setFormError(null); reload();
    } catch (err) { setFormError(err.message); }
  };
  const revoke = async t => {
    if (!confirm(`Revoke the token “${t.name}”? Whatever uses it stops getting answers at once.`)) return;
    try { await api('/api/v1/tokens/' + t.id, { method: 'DELETE' }); reload(); } catch (err) { alert(err.message); }
  };
  const close = () => { setDialog(false); setIssued(null); setFormError(null); };
  const expired = t => t.expires && new Date(t.expires) < new Date();
  return html`
    <div class="head" style="margin-top: 28px"><h1 style="font-size: 17px">Read-only tokens</h1><span class="spacer"></span>
      <a class="btn" href="/docs/grafana" target="_blank" rel="noopener">Using Grafana</a>
      <button class="primary" onClick=${() => setDialog(true)}>New token</button></div>
    <${ErrorBox} error=${error} />
    <div class="panel flush">${!list ? html`<div class="empty">Loading…</div>` : list.length === 0
      ? html`<div class="empty">No tokens. A token lets Grafana or a script read what a viewer can see, and change nothing.</div>`
      : html`<table><thead><tr><th>Name</th><th>May</th><th>State</th><th>Last used</th><th>Expires</th><th>Created</th><th></th></tr></thead>
        <tbody>${list.map(t => html`<tr>
          <td><strong>${t.name}</strong></td>
          <td class="muted">${t.scope === 'issue' ? 'issue evidence grants' : 'read'}</td>
          <td><${Badge} kind=${expired(t) ? 'off' : 'ok'}>${expired(t) ? 'expired' : 'active'}<//></td>
          <td class="muted" title=${t.last_used ? new Date(t.last_used).toLocaleString() : ''}>${t.last_used ? new Date(t.last_used).toLocaleString() : 'never'}</td>
          <td class="muted">${t.expires ? new Date(t.expires).toLocaleDateString() : 'never'}</td>
          <td class="muted" title=${new Date(t.created).toLocaleString()}>${new Date(t.created).toLocaleDateString()} by ${t.author}</td>
          <td style="text-align: right"><button class="small danger" onClick=${() => revoke(t)}>Revoke</button></td>
        </tr>`)}</tbody></table>`}</div>
    ${dialog && html`<${Dialog} title="New token" onClose=${close}>${issued ? html`
      <p>This is the token. It is shown only this once.</p>
      <div class="secret">${issued.token}</div>
      <p class="muted">A tool sends it as the header <code>Authorization: Bearer ${'<token>'}</code>.
        For Grafana, <a href="/docs/grafana" target="_blank" rel="noopener">the guide</a> has a file to start from.</p>
      <div class="actions"><button onClick=${() => navigator.clipboard?.writeText(issued.token)}>Copy</button>
        <button class="primary" onClick=${close}>Done</button></div>`
      : html`<form onSubmit=${create}>
        <div class="field"><label>Name (what will use it)</label><input name="name" required maxlength="100" placeholder="e.g. grafana" /></div>
        <div class="field"><label>It may</label><select name="scope">
          <option value="read">read what a viewer sees</option>
          <option value="issue">only issue evidence grants (for Boop Chat)</option></select></div>
        <div class="field"><label>Valid for</label><select name="valid">
          <option value="0">until revoked</option><option value="30">30 days</option><option value="90">90 days</option><option value="365">1 year</option></select></div>
        <${ErrorBox} error=${formError} />
        <div class="actions"><button type="button" onClick=${close}>Cancel</button><button class="primary">Create</button></div>
      </form>`}<//>`}`;
}

// Evidence grants: one incident's evidence for one run of an agent, short
// and counted. Admins see every grant and can end one.
function EvidenceGrants() {
  const { data: list, error, reload } = useLoad(() => api('/api/v1/evidence-grants'), [], 30000);
  const revoke = async g => {
    if (!confirm(`End the grant for “${g.purpose}”? Whatever holds it reads nothing more.`)) return;
    try { await api('/api/v1/evidence-grants/' + g.id, { method: 'DELETE' }); reload(); } catch (err) { alert(err.message); }
  };
  const live = g => new Date(g.expires) > new Date() && g.uses < g.max_uses;
  return html`
    <div class="head" style="margin-top: 28px"><h1 style="font-size: 17px">Evidence grants</h1><span class="spacer"></span>
      <a class="btn" href="/docs/api#evidence-grants" target="_blank" rel="noopener">What they are</a></div>
    <${ErrorBox} error=${error} />
    <div class="panel flush">${!list ? html`<div class="empty">Loading…</div>` : list.length === 0
      ? html`<div class="empty">No grants in the last 30 days. A grant lets one run of an agent read one incident's evidence for a short time.</div>`
      : html`<table><thead><tr><th>For</th><th>Incident</th><th>State</th><th>Reads</th><th>Issued</th><th>Expires</th><th></th></tr></thead>
        <tbody>${list.map(g => html`<tr>
          <td><strong>${g.purpose}</strong></td>
          <td class="muted">#${g.incident}</td>
          <td><${Badge} kind=${live(g) ? 'ok' : 'off'}>${live(g) ? 'active' : 'ended'}<//></td>
          <td class="muted">${g.uses} of ${g.max_uses}</td>
          <td class="muted" title=${new Date(g.created).toLocaleString()}>${new Date(g.created).toLocaleString()} by ${g.issuer}</td>
          <td class="muted">${new Date(g.expires).toLocaleTimeString()}</td>
          <td style="text-align: right">${live(g) && html`<button class="small danger" onClick=${() => revoke(g)}>End</button>`}</td>
        </tr>`)}</tbody></table>`}</div>`;
}

render(html`<${App} />`, document.getElementById('app'));
