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

const pages = [
  ['home', 'Home', Home],
  ['dashboards', 'Dashboards', Dashboards],
  ['explore', 'Explore', Explore],
  ['services', 'Services', Services],
  ['sessions', 'Sessions', Sessions, 'ai'],
  ['alerts', 'Alerts', Alerts],
  ['captures', 'Captures', Captures],
  ['fleet', 'Fleet', Fleet],
  ['users', 'Users', Users, 'admin'],
  ['secrets', 'Secrets', Secrets, 'admin'],
];

function App() {
  const [me, setMe] = useState(undefined); // undefined: checking, null: signed out
  const [route, setRoute] = useState({ ...parseHash(), nav: 0 });
  const [dialog, setDialog] = useState(null);

  // Sessions shows while an AI module (an integration named ai_module_…)
  // is switched on somewhere.
  const [ai, setAi] = useState(false);
  const check = () => api('/api/v1/me').then(m => {
    setMe(m);
    api('/api/v1/fleet/integrations').then(list => setAi(list.some(i => i.name.startsWith('ai_module_') && i.enabled > 0)), () => {});
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
  const visible = pages.filter(p => !p[3] || (p[3] === 'admin' && admin) || (p[3] === 'ai' && (ai || route.parts[0] === 'sessions')));
  const [id, , Page] = visible.find(p => p[0] === route.parts[0]) || visible[0];
  const logout = () => api('/api/v1/logout', { method: 'POST' }).then(() => setMe(null));

  return html`<div class="shell">
    <aside class="side">
      <div class="brand"><i></i>vigil</div>
      <nav class="nav">${visible.map(([pid, label]) =>
        html`<a class=${pid === id ? 'on' : ''} href="#/${pid}">${label}</a>`)}</nav>
      <span class="spacer"></span>
      <nav class="nav"><a href="/docs/" target="_blank" rel="noopener">Docs</a></nav>
      <div class="who">
        <div><strong>${me.user}</strong> <span class="muted">${me.role}</span></div>
        ${!me.bootstrap && html`<button class="small" onClick=${() => setDialog('password')}>Password</button>`}
        <button class="small" onClick=${toggleTheme}>Theme</button>
        <button class="small" onClick=${logout}>Sign out</button>
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

function toggleTheme() {
  const root = document.documentElement;
  const dark = root.dataset.theme ? root.dataset.theme === 'dark' : !matchMedia('(prefers-color-scheme: light)').matches;
  root.dataset.theme = dark ? 'light' : 'dark';
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
      setIssued(await api('/api/v1/tokens', { method: 'POST', body: { name: f.get('name').trim(), valid_days: Number(f.get('valid')) } }));
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
      : html`<table><thead><tr><th>Name</th><th>State</th><th>Last used</th><th>Expires</th><th>Created</th><th></th></tr></thead>
        <tbody>${list.map(t => html`<tr>
          <td><strong>${t.name}</strong></td>
          <td><${Badge} kind=${expired(t) ? 'off' : 'ok'}>${expired(t) ? 'expired' : 'active'}<//></td>
          <td class="muted" title=${t.last_used ? new Date(t.last_used).toLocaleString() : ''}>${t.last_used ? new Date(t.last_used).toLocaleString() : 'never'}</td>
          <td class="muted">${t.expires ? new Date(t.expires).toLocaleDateString() : 'never'}</td>
          <td class="muted" title=${new Date(t.created).toLocaleString()}>${new Date(t.created).toLocaleDateString()} by ${t.author}</td>
          <td style="text-align: right"><button class="small danger" onClick=${() => revoke(t)}>Revoke</button></td>
        </tr>`)}</tbody></table>`}</div>
    ${dialog && html`<${Dialog} title="New read-only token" onClose=${close}>${issued ? html`
      <p>This is the token. It is shown only this once.</p>
      <div class="secret">${issued.token}</div>
      <p class="muted">A tool sends it as the header <code>Authorization: Bearer ${'<token>'}</code>.
        For Grafana, <a href="/docs/grafana" target="_blank" rel="noopener">the guide</a> has a file to start from.</p>
      <div class="actions"><button onClick=${() => navigator.clipboard?.writeText(issued.token)}>Copy</button>
        <button class="primary" onClick=${close}>Done</button></div>`
      : html`<form onSubmit=${create}>
        <div class="field"><label>Name (what will use it)</label><input name="name" required maxlength="100" placeholder="e.g. grafana" /></div>
        <div class="field"><label>Valid for</label><select name="valid">
          <option value="0">until revoked</option><option value="30">30 days</option><option value="90">90 days</option><option value="365">1 year</option></select></div>
        <${ErrorBox} error=${formError} />
        <div class="actions"><button type="button" onClick=${close}>Cancel</button><button class="primary">Create</button></div>
      </form>`}<//>`}`;
}

render(html`<${App} />`, document.getElementById('app'));
