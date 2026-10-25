import { html, useState, api, useLoad, Dialog, ErrorBox, Badge, ago, until } from './lib.js';

// Secrets: where vigil reads the secrets it needs from (an OpenBao, or a
// Vault: same API), and which secret each thing uses. Values never come
// back from the server; the page only learns whether one is there, its
// version and its last four characters.

const blankStore = { kind: 'openbao', address: '', ca_cert: '', namespace: '', auth: 'approle', role_id: '', auth_mount: 'approle', mount: 'vigil' };

export function Secrets() {
  const { data, error, reload } = useLoad(() => api('/api/v1/secrets'));
  const [editing, setEditing] = useState(null);
  const [choosing, setChoosing] = useState(null);
  const [removing, setRemoving] = useState(false);
  const [tests, setTests] = useState({});
  const test = async name => {
    setTests(t => ({ ...t, [name]: { busy: true } }));
    try { const r = await api(`/api/v1/secrets/uses/${name}/test`, { method: 'POST' }); setTests(t => ({ ...t, [name]: r })); reload(); }
    catch (e) { setTests(t => ({ ...t, [name]: { error: e.message } })); }
  };
  if (!data) return html`<div class="head"><h1>Secrets</h1></div><${ErrorBox} error=${error} />`;
  const st = data.store, h = data.health;
  return html`
    <div class="head"><h1>Secrets</h1><span class="spacer"></span>
      <button class=${st ? '' : 'primary'} onClick=${() => setEditing(st ? { ...blankStore, ...st } : { ...blankStore })}>${st ? 'Change store…' : 'Set up a secret store…'}</button></div>
    <${ErrorBox} error=${error} />
    <div class="panel">
      <h3 style="margin-top: 0">Secret store</h3>
      ${st ? html`<table class="kv"><tbody>
          <tr><td>Kind</td><td>OpenBao / Vault</td></tr>
          <tr><td>Address</td><td><code>${st.address}</code>${st.namespace && html` · namespace <code>${st.namespace}</code>`}${st.ca_cert && ' · own CA'}</td></tr>
          <tr><td>Login</td><td>${st.auth === 'approle' ? html`AppRole <code>${st.role_id}</code> (mount <code>${st.auth_mount}</code>)` : 'token'}
            ${data.has_credential && html` · <span class="muted">secret kept on this server, encrypted</span>`}</td></tr>
          <tr><td>KV mount</td><td><code>${st.mount}</code>${h?.kv_version ? ` · KV v${h.kv_version}` : ''}</td></tr>
          <tr><td>State</td><td>${!h?.checked ? html`<span class="muted">not checked yet</span>` : h.up
            ? html`<${Badge} kind="ok">answers<//> <span class="muted">checked ${ago(h.checked)}${h.token_expiry ? ` · vigil's login ${h.renewable ? 'renews itself' : 'cannot be renewed'} (${until(h.token_expiry)})` : ''}${h.policies?.length ? ` · policies ${h.policies.join(', ')}` : ''}</span>`
            : html`<${Badge} kind="bad">does not answer<//> <span class="muted">${h.error} · the values read last are used</span>`}</td></tr>
        </tbody></table>
        <div class="row" style="margin-top: 8px"><span class="spacer"></span><button class="small danger" onClick=${() => setRemoving(true)}>Remove store…</button></div>`
      : html`<p class="muted" style="margin: 0">None yet. vigil can read its secrets (the AI provider's key first) from an OpenBao or Vault you run:
          it logs in with an AppRole or a token of its own and reads a KV mount. See <a href="/docs/secrets" target="_blank" rel="noopener">Secrets</a> for what to create there.</p>`}
    </div>
    <div class="panel flush"><table>
      <thead><tr><th>Secret</th><th>Comes from</th><th>State</th><th></th></tr></thead>
      <tbody>${data.uses.map(u => html`<tr>
        <td><strong>${u.title}</strong><div class="muted" style="font-size: 12px">${u.used_by}</div></td>
        <td>${u.source === 'off' ? html`<span class="muted">not used: no <code>ai</code> section in the server config</span>`
          : u.source === 'file' ? html`file <code>${u.file}</code> <span class="muted">(set in the server config; remove <code>api_key_file</code> there to use the store)</span>`
          : u.ref ? html`<code>${st?.mount || '?'}/${u.ref.path}</code> field <code>${u.ref.field}</code>` : html`<span class="muted">not chosen</span>`}</td>
        <td>${u.status && (u.status.found ? html`<${Badge} kind="ok">found<//> <span class="muted">${u.status.version ? `version ${u.status.version} · ` : ''}${u.status.hint || ''}</span>`
          : html`<${Badge} kind="bad">missing<//> <span class="muted">${u.status.error}</span>`)}
          ${tests[u.name] && html`<div style="font-size: 12px">${tests[u.name].busy ? 'Testing…' : tests[u.name].error ? html`<span class="error-text">${tests[u.name].error}</span>`
            : tests[u.name].provider ? html`Provider: ${tests[u.name].provider === 'accepted' ? html`<${Badge} kind="ok">key accepted<//>` : html`<span class="error-text">${tests[u.name].provider}</span>`}` : ''}</div>`}</td>
        <td style="text-align: right; white-space: nowrap">${u.source === 'store' && html`
          <button class="small" disabled=${!st} title=${st ? '' : 'Set up a secret store first'} onClick=${() => setChoosing(u)}>${u.ref ? 'Change…' : 'Choose…'}</button>
          ${u.ref && st && html` <button class="small" onClick=${() => test(u.name)}>Test</button>`}`}</td>
      </tr>`)}</tbody></table></div>
    <p class="muted">Values are never shown or sent back by vigil, only whether they are there and their last four characters. vigil keeps them in memory
      and reads them again every few minutes; while the store does not answer it goes on with the values it read last.</p>
    ${editing && html`<${StoreDialog} init=${editing} existing=${st} onClose=${() => setEditing(null)} onSaved=${() => { setEditing(null); reload(); }} />`}
    ${choosing && html`<${ChooseDialog} use=${choosing} store=${st} onClose=${() => setChoosing(null)} onSaved=${() => { setChoosing(null); setTests({}); reload(); }} />`}
    ${removing && html`<${RemoveDialog} onClose=${() => setRemoving(false)} onDone=${() => { setRemoving(false); reload(); }} />`}`;
}

function StoreDialog({ init, existing, onClose, onSaved }) {
  const [f, setF] = useState(init);
  const [cred, setCred] = useState('');
  const [password, setPassword] = useState('');
  const [result, setResult] = useState(null);
  const [error, setError] = useState(null);
  const [busy, setBusy] = useState(false);
  const set = k => e => { setF({ ...f, [k]: e.target.value }); setResult(null); };
  const same = existing && ['address', 'auth', 'role_id', 'auth_mount', 'namespace', 'ca_cert'].every(k => (existing[k] || '') === (f[k] || ''));
  const body = () => ({ store: f, credential: cred });
  const test = async () => {
    setBusy(true); setError(null); setResult(null);
    try { setResult(await api('/api/v1/secrets/store/test', { method: 'POST', body: body() })); } catch (e) { setError(e.message); }
    setBusy(false);
  };
  const save = async e => {
    e.preventDefault(); setBusy(true); setError(null);
    try { await api('/api/v1/secrets/store', { method: 'PUT', body: { ...body(), password } }); onSaved(); } catch (err) { setError(err.message); setBusy(false); }
  };
  const caps = result?.capabilities;
  return html`<${Dialog} title="Secret store" wide onClose=${onClose}><form onSubmit=${save}>
    <p class="muted" style="margin-top: 0">An OpenBao or Vault that vigil logs in to with an AppRole (or a token) of its own, reading one KV mount.</p>
    <div class="field"><label>Address</label><input required placeholder="https://bao.example.com:8200" value=${f.address} onInput=${set('address')} /></div>
    <div class="row" style="gap: 12px; align-items: flex-start">
      <div class="field" style="flex: 1"><label>KV mount</label><input required value=${f.mount} onInput=${set('mount')} placeholder="vigil" /></div>
      <div class="field" style="flex: 1"><label>Namespace <span class="muted">(optional)</span></label><input value=${f.namespace} onInput=${set('namespace')} /></div>
    </div>
    <div class="field"><label>Login</label>
      <label style="display: inline; margin-right: 16px"><input type="radio" checked=${f.auth === 'approle'} onChange=${() => setF({ ...f, auth: 'approle' })} /> AppRole</label>
      <label style="display: inline"><input type="radio" checked=${f.auth === 'token'} onChange=${() => setF({ ...f, auth: 'token' })} /> Token</label></div>
    ${f.auth === 'approle' && html`<div class="row" style="gap: 12px; align-items: flex-start">
      <div class="field" style="flex: 2"><label>role_id</label><input required value=${f.role_id} onInput=${set('role_id')} spellcheck="false" /></div>
      <div class="field" style="flex: 1"><label>Auth mount</label><input value=${f.auth_mount} onInput=${set('auth_mount')} placeholder="approle" /></div></div>`}
    <div class="field"><label>${f.auth === 'approle' ? 'secret_id' : 'Token'}</label>
      <input type="password" autocomplete="off" value=${cred} onInput=${e => { setCred(e.target.value); setResult(null); }}
        required=${!same} placeholder=${same ? 'leave empty to keep the one saved' : ''} spellcheck="false" />
      <div class="muted" style="font-size: 12px">Kept on this server encrypted, never shown again. It is only reused for this same address and login: change those and it has to be entered again.</div></div>
    <details style="margin-bottom: 10px"><summary style="cursor: pointer">The store's certificate authority (optional)</summary>
      <textarea rows="5" spellcheck="false" placeholder="-----BEGIN CERTIFICATE-----" value=${f.ca_cert} onInput=${set('ca_cert')}></textarea>
      <div class="muted" style="font-size: 12px">For a store whose certificate is not signed by an authority this server trusts.</div></details>
    <${ErrorBox} error=${error} />
    ${result && html`<div class="panel" style="margin: 0 0 10px">${result.error
      ? html`<div class="error" style="margin: 0">${result.error}</div>`
      : html`<p style="margin: 0 0 4px"><${Badge} kind="ok">logged in<//> KV v${result.health.kv_version}${result.health.policies?.length ? ` · policies ${result.health.policies.join(', ')}` : ''}
          ${result.health.token_expiry ? ` · token lives until ${new Date(result.health.token_expiry).toLocaleString()}${result.health.renewable ? ', renewable' : ', NOT renewable'}` : ' · token does not expire'}</p>
        <p style="margin: 0">May list secrets: ${caps.list ? 'yes' : 'no'} · may write: ${caps.write ? 'yes' : 'no'}
          ${Object.entries(caps.read || {}).map(([p, ok]) => html` · read <code>${p}</code>: ${ok ? 'yes' : html`<strong>no</strong>`}`)}</p>`}</div>`}
    <div class="field"><label>Your password <span class="muted">(or the admin token, if you signed in with it)</span></label>
      <input type="password" autocomplete="current-password" value=${password} onInput=${e => setPassword(e.target.value)} /></div>
    <div class="actions"><button type="button" onClick=${onClose}>Cancel</button><button type="button" disabled=${busy} onClick=${test}>Test</button>
      <button class="primary" disabled=${busy || !password}>${busy ? 'Working…' : 'Test and save'}</button></div>
  </form><//>`;
}

function ChooseDialog({ use, store, onClose, onSaved }) {
  const [path, setPath] = useState(use.ref?.path || '');
  const [field, setField] = useState(use.ref?.field || 'api_key');
  const [prefix, setPrefix] = useState('');
  const { data: keys, error: listError } = useLoad(() => api('/api/v1/secrets/store/list?prefix=' + encodeURIComponent(prefix)), [prefix]);
  const [error, setError] = useState(null);
  const save = async e => {
    e.preventDefault(); setError(null);
    try { await api(`/api/v1/secrets/uses/${use.name}`, { method: 'PUT', body: { path, field } }); onSaved(); } catch (err) { setError(err.message); }
  };
  return html`<${Dialog} title=${'Where the ' + use.title + ' comes from'} onClose=${onClose}><form onSubmit=${save}>
    <div class="field"><label>Secret <span class="muted">(its path under <code>${store.mount}</code>)</span></label>
      <input required value=${path} onInput=${e => setPath(e.target.value)} placeholder="anthropic" spellcheck="false" /></div>
    ${keys ? html`<div style="margin: -4px 0 10px; font-size: 12.5px">${prefix && html`<a href="#" onClick=${e => { e.preventDefault(); setPrefix(prefix.split('/').slice(0, -2).join('/') + (prefix.split('/').length > 2 ? '/' : '')); }}>↑ up</a> · `}
        ${keys.length === 0 ? html`<span class="muted">nothing under ${prefix || 'the mount'}</span>` : keys.map(k => html`<a href="#" style="margin-right: 10px"
          onClick=${e => { e.preventDefault(); k.endsWith('/') ? setPrefix(prefix + k) : setPath(prefix + k); }}><code>${k}</code></a>`)}</div>`
      : listError && html`<div class="muted" style="margin: -4px 0 10px; font-size: 12.5px">The secrets cannot be listed (${listError}); type the path.</div>`}
    <div class="field"><label>Field <span class="muted">(the key inside the secret)</span></label>
      <input required value=${field} onInput=${e => setField(e.target.value)} spellcheck="false" /></div>
    <${ErrorBox} error=${error} />
    <div class="actions"><button type="button" onClick=${onClose}>Cancel</button><button class="primary">Use it</button></div>
  </form><//>`;
}

function RemoveDialog({ onClose, onDone }) {
  const [password, setPassword] = useState('');
  const [error, setError] = useState(null);
  const remove = async e => {
    e.preventDefault(); setError(null);
    try { await api('/api/v1/secrets/store/remove', { method: 'POST', body: { password } }); onDone(); } catch (err) { setError(err.message); }
  };
  return html`<${Dialog} title="Remove the secret store" onClose=${onClose}><form onSubmit=${remove}>
    <p style="margin-top: 0">vigil forgets the store and its login. What it read from there stops working until a store is set up again; nothing in the store is changed.</p>
    <div class="field"><label>Your password <span class="muted">(or the admin token)</span></label>
      <input type="password" autocomplete="current-password" required value=${password} onInput=${e => setPassword(e.target.value)} /></div>
    <${ErrorBox} error=${error} />
    <div class="actions"><button type="button" onClick=${onClose}>Cancel</button><button class="danger">Remove</button></div>
  </form><//>`;
}
