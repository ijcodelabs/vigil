// Shared pieces: the API client, small hooks and the components every page uses.
import { html, useState, useEffect, useRef, useCallback } from '../vendor/standalone.module.js';

export { html, useState, useEffect, useRef, useCallback };
export { render, useMemo, useErrorBoundary } from '../vendor/standalone.module.js';

let onSignedOut = () => {};
export function setSignedOutHandler(fn) { onSignedOut = fn; }

// api calls the server as the signed-in user. A body that is an object is
// sent as JSON, a string as is, URLSearchParams as a form. The result is
// parsed JSON when the server sends JSON, otherwise text.
export async function api(path, { method = 'GET', body, signal } = {}) {
  const headers = { 'X-Vigil-CSRF': '1' };
  if (body !== undefined && typeof body === 'object' && !(body instanceof URLSearchParams)) {
    headers['Content-Type'] = 'application/json';
    body = JSON.stringify(body);
  }
  const res = await fetch(path, { method, headers, body, signal, credentials: 'same-origin' });
  if (res.status === 401 && path !== '/api/v1/login') {
    onSignedOut();
    throw new Error('signed out');
  }
  const text = await res.text();
  if (!res.ok) throw new Error(errorText(text) || `request failed (${res.status})`);
  const type = res.headers.get('Content-Type') || '';
  // Log queries answer with one JSON object per line (a stream type), which is left as text.
  return type.startsWith('application/json') && text ? JSON.parse(text) : text;
}

// The stores answer errors as JSON, the server as plain text; show either plainly.
function errorText(text) {
  try {
    const j = JSON.parse(text);
    return j.error || j.message || text;
  } catch { return text.trim(); }
}

// useLoad runs an async loader now, whenever deps change, and every
// `every` milliseconds if given. reload() runs it on demand.
export function useLoad(loader, deps = [], every = 0) {
  const [state, setState] = useState({ data: undefined, error: null, loading: true });
  const fn = useRef(loader);
  fn.current = loader;
  const reload = useCallback(() => {
    return fn.current().then(
      data => setState({ data, error: null, loading: false }),
      error => setState(s => ({ ...s, error: error.message, loading: false })));
  }, []);
  useEffect(() => {
    setState(s => ({ ...s, loading: true }));
    reload();
    if (!every) return;
    const t = setInterval(reload, every);
    return () => clearInterval(t);
  }, deps);
  return { ...state, reload };
}

// Routing is in the hash: #/alerts/rules?q=... so links can be shared and
// the server never needs to know about pages.
export function parseHash() {
  const [path, query = ''] = location.hash.replace(/^#\/?/, '').split('?');
  return { parts: path.split('/').filter(Boolean), params: new URLSearchParams(query) };
}
export function setParams(parts, params) {
  const q = new URLSearchParams(params).toString();
  const next = '#/' + parts.join('/') + (q ? '?' + q : '');
  if (next !== location.hash) history.replaceState(null, '', next);
}

export function ago(iso) {
  const s = Math.max(0, (Date.now() - new Date(iso).getTime()) / 1000);
  if (s < 90) return `${Math.round(s)}s ago`;
  if (s < 5400) return `${Math.round(s / 60)}m ago`;
  if (s < 129600) return `${Math.round(s / 3600)}h ago`;
  return `${Math.round(s / 86400)}d ago`;
}
export function until(iso) {
  const s = (new Date(iso).getTime() - Date.now()) / 1000;
  if (s <= 0) return 'ended';
  if (s < 5400) return `${Math.round(s / 60)}m left`;
  if (s < 129600) return `${Math.round(s / 3600)}h left`;
  return `${Math.round(s / 86400)}d left`;
}
export function fmtNum(v) {
  const n = Number(v);
  if (!isFinite(n)) return String(v);
  const a = Math.abs(n);
  if (a !== 0 && (a >= 1e15 || a < 1e-4)) return n.toExponential(3);
  return n.toLocaleString('en-US', { maximumFractionDigits: a >= 100 ? 2 : 4 });
}

export const Badge = ({ kind = 'off', children }) => html`<span class="badge ${kind}">${children}</span>`;
export const Chips = ({ labels, skip = [] }) => Object.entries(labels || {})
  .filter(([k]) => !skip.includes(k)).sort()
  .map(([k, v]) => html`<span class="chip" title="${k}=${v}">${k}=${v}</span>`);
export const ErrorBox = ({ error }) => error ? html`<div class="error" role="alert">${error}</div>` : null;

export function Tabs({ base, tabs, current }) {
  return html`<nav class="tabs">${tabs.map(([id, label]) =>
    html`<a class=${id === current ? 'on' : ''} href="#/${base}/${id}">${label}</a>`)}</nav>`;
}

export function Dialog({ title, wide, onClose, children }) {
  useEffect(() => {
    const esc = e => e.key === 'Escape' && onClose();
    addEventListener('keydown', esc);
    return () => removeEventListener('keydown', esc);
  }, []);
  return html`<div class="overlay" onMouseDown=${e => e.target === e.currentTarget && onClose()}>
    <div class="dialog ${wide ? 'wide' : ''}" role="dialog" aria-label=${title}>
      <h2>${title}</h2>${children}
    </div></div>`;
}

// diffLines marks which lines of b are new relative to a, and which of a are gone.
export function diffLines(a, b) {
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

// YamlEditor edits one YAML document: load() fetches it, save(text) stores
// it. The server validates on save; its reason is shown and nothing changes
// when it refuses. tools, when given, is called with the text being edited
// and shown under it, for checks on what is not saved yet. history, when
// given, is the URL of the document's saved versions (base/versions): they
// are listed, compared with the editor's text, and one can be put back into
// the editor, to be saved through the same checks.
export function YamlEditor({ title, help, load, save, onClose, extra, tools, history }) {
  const [text, setText] = useState(null);
  const [error, setError] = useState(null);
  const [busy, setBusy] = useState(false);
  useEffect(() => { load().then(t => setText(t || ''), e => setError(e.message)); }, []);
  const submit = async () => {
    setBusy(true); setError(null);
    try { await save(text); onClose(true); } catch (e) { setError(e.message); setBusy(false); }
  };
  const tab = e => {
    if (e.key !== 'Tab' || e.shiftKey) return;
    e.preventDefault();
    const el = e.target, at = el.selectionStart;
    el.setRangeText('  ', at, el.selectionEnd, 'end');
    setText(el.value);
  };
  return html`<${Dialog} title=${title} wide onClose=${() => onClose(false)}>
    ${help && html`<p class="muted">${help}</p>`}
    ${text === null && !error ? html`<div class="empty">Loading…</div>` : html`
      <textarea rows="24" spellcheck="false" value=${text || ''} onInput=${e => setText(e.target.value)} onKeyDown=${tab}
        aria-label=${title}></textarea>`}
    ${tools && text !== null && tools(text)}
    ${history && text !== null && html`<${History} url=${history} text=${text} onPick=${setText} />`}
    <${ErrorBox} error=${error} />
    <div class="actions">
      ${extra}<span class="spacer"></span>
      <button onClick=${() => onClose(false)}>Cancel</button>
      <button class="primary" disabled=${busy || text === null} onClick=${submit}>${busy ? 'Checking…' : 'Save'}</button>
    </div>
  <//>`;
}

// History lists a document's saved versions, newest first, and compares one
// with the text being edited.
function History({ url, text, onPick }) {
  const { data: versions, error } = useLoad(() => api(url), [url]);
  const [compare, setCompare] = useState(null); // {version, doc}
  const [err, setErr] = useState(null);
  const show = async v => {
    setErr(null);
    try { setCompare({ version: v, doc: await api(`${url}/${v}`) }); } catch (e) { setErr(e.message); }
  };
  if (error) return html`<${ErrorBox} error=${error} />`;
  if (!versions || versions.length === 0) return null;
  const how = v => v.origin === 'before-history' ? 'as it was before history was kept'
    : `${v.origin === 'files' ? 'applied from files' : 'saved'} by ${v.author}, ${ago(v.time)}`;
  return html`<details style="margin-top: 12px"><summary class="muted" style="cursor: pointer">History (${versions.length} versions)</summary>
    <div class="panel flush" style="margin: 8px 0 0; max-height: 180px; overflow: auto"><table><tbody>${versions.map((v, i) => html`<tr>
      <td style="white-space: nowrap">v${v.version}${i === 0 ? ' (saved now)' : ''}</td>
      <td class="muted" title=${new Date(v.time).toLocaleString()}>${how(v)}</td>
      <td style="text-align: right"><button class="small" onClick=${() => show(v.version)}>Compare</button></td></tr>`)}</tbody></table></div>
    <${ErrorBox} error=${err} />
    ${compare && html`<p class="muted" style="margin: 10px 0 4px">What the editor has that v${compare.version} does not (green), and the reverse (red):</p>
      <pre class="secret diff" style="user-select: text; margin: 0; max-height: 260px; overflow: auto">${diffLines(compare.doc, text || '').map(([op, line]) =>
        html`<div class=${op === '+' ? 'add' : op === '-' ? 'del' : ''}>${op} ${line}</div>`)}</pre>
      <button class="small" style="margin-top: 6px" onClick=${() => { onPick(compare.doc); setCompare(null); }}>Put v${compare.version} back into the editor</button>
      <span class="muted" style="font-size: 12px"> then Save to use it; it is checked like any other save.</span>`}
  </details>`;
}

// fmtUnit shows a number in a panel's unit.
export function fmtUnit(v, unit) {
  const n = Number(v);
  if (v == null || !isFinite(n)) return v == null ? '–' : String(v);
  const scaled = (steps, base, digits = 1) => {
    let x = n, i = 0;
    while (Math.abs(x) >= base && i < steps.length - 1) { x /= base; i++; }
    return (i === 0 ? fmtNum(x) : x.toFixed(Math.abs(x) >= 100 ? 0 : digits)) + ' ' + steps[i];
  };
  switch (unit) {
    case 'yesno': return n >= 1 ? 'yes' : 'no';
    case 'percent': return n.toFixed(Math.abs(n) >= 100 ? 0 : 1) + '%';
    case 'ratio': return (n * 100).toFixed(1) + '%';
    case 'bytes': return scaled(['B', 'KiB', 'MiB', 'GiB', 'TiB', 'PiB'], 1024);
    case 'bytes/s': return scaled(['B/s', 'KiB/s', 'MiB/s', 'GiB/s', 'TiB/s'], 1024);
    case 'ops/s': return scaled(['/s', 'k/s', 'M/s'], 1000);
    case 'cores': return n.toFixed(Math.abs(n) >= 10 ? 1 : 2);
    case 'ms': return Math.abs(n) >= 1000 ? (n / 1000).toFixed(2) + ' s' : n.toFixed(Math.abs(n) >= 10 ? 0 : 1) + ' ms';
    case 'seconds': {
      const a = Math.abs(n);
      if (a < 1) return (n * 1000).toFixed(a < 0.01 ? 2 : 0) + ' ms';
      if (a < 120) return n.toFixed(a < 10 ? 2 : 0) + ' s';
      if (a < 7200) return (n / 60).toFixed(1) + ' min';
      if (a < 172800) return (n / 3600).toFixed(1) + ' h';
      return (n / 86400).toFixed(1) + ' d';
    }
  }
  return scaled(['', 'k', 'M', 'B', 'T'], 1000, 2).trim();
}
