import { html, useState, useEffect, useRef } from './lib.js';

// TimeRange picks the time range of Explore, a dashboard or the Sessions
// page: a quick "last N", or an absolute From-To typed or picked in the
// browser's own date and time picker, in the browser's time zone. « and »
// step the range back and forward by its own length; − doubles it around
// its middle. Absolute ranges used recently are offered again (kept in this
// browser only).

export const RANGES = [['5m', 300], ['15m', 900], ['30m', 1800], ['1h', 3600], ['3h', 10800], ['6h', 21600], ['12h', 43200], ['24h', 86400], ['2d', 172800], ['7d', 604800]];
const LONG = { m: 'minutes', h: 'hours', d: 'days' };
const longName = l => { const n = parseInt(l, 10); const u = LONG[l.slice(-1)]; return `Last ${n} ${n === 1 ? u.slice(0, -1) : u}`; };

const RECENT_KEY = 'vigil.recentRanges';
const recentRanges = () => { try { return JSON.parse(localStorage.getItem(RECENT_KEY)) || []; } catch { return []; } };
function rememberRange(r) {
  try {
    const list = [r, ...recentRanges().filter(x => x.from !== r.from || x.to !== r.to)].slice(0, 5);
    localStorage.setItem(RECENT_KEY, JSON.stringify(list));
  } catch {}
}

const pad = n => String(n).padStart(2, '0');
// A Date as the browser's datetime-local input wants it: local, to the minute.
const toInput = d => `${d.getFullYear()}-${pad(d.getMonth() + 1)}-${pad(d.getDate())}T${pad(d.getHours())}:${pad(d.getMinutes())}`;
const show = d => toInput(d).replace('T', ' ');
const sameDay = (a, b) => a.toDateString() === b.toDateString();

// label says what a range is, short enough for a button.
export function rangeLabel(range) {
  if (range.last) {
    const q = RANGES.find(([, s]) => s === range.last);
    return q ? longName(q[0]) : `Last ${Math.round(range.last / 60)} minutes`;
  }
  const a = new Date(range.from * 1000), b = new Date(range.to * 1000);
  return `${show(a)} – ${sameDay(a, b) ? show(b).slice(11) : show(b)}`;
}

function zone() {
  const off = -new Date().getTimezoneOffset();
  const name = Intl.DateTimeFormat().resolvedOptions().timeZone || '';
  return `${name} · UTC${off >= 0 ? '+' : '−'}${pad(Math.floor(Math.abs(off) / 60))}:${pad(Math.abs(off) % 60)}`;
}

// window is [from, to] in seconds for any range.
const windowOf = r => (r.last ? [Date.now() / 1000 - r.last, Date.now() / 1000] : [r.from, r.to]);

export function TimeRange({ range, setRange, quick = RANGES }) {
  const [open, setOpen] = useState(false);
  const [right, setRight] = useState(false); // open leftwards: no room to the right
  const box = useRef(null);
  const toggle = () => {
    const r = box.current?.getBoundingClientRect();
    setRight(!!r && r.left + Math.min(620, innerWidth * 0.92) > innerWidth - 8);
    setOpen(!open);
  };
  useEffect(() => {
    if (!open) return;
    const away = e => { if (!box.current?.contains(e.target)) setOpen(false); };
    const esc = e => { if (e.key === 'Escape') setOpen(false); };
    addEventListener('mousedown', away);
    addEventListener('keydown', esc);
    return () => { removeEventListener('mousedown', away); removeEventListener('keydown', esc); };
  }, [open]);

  const absolute = (from, to) => {
    const now = Date.now() / 1000;
    if (to >= now - 1) { // reaching now: a quick range when one fits, so it keeps moving
      const q = quick.find(([, s]) => Math.abs(s - (to - from)) < 1);
      if (q) return setRange({ last: q[1] });
      to = now;
    }
    setRange({ from: Math.round(from), to: Math.round(to) });
  };
  const shift = dir => {
    const [f, t] = windowOf(range);
    const len = t - f;
    if (dir > 0 && t >= Date.now() / 1000 - 1) return; // already at now
    absolute(f + dir * len, t + dir * len);
  };
  const zoomOut = () => {
    const [f, t] = windowOf(range);
    const mid = (f + t) / 2, len = t - f;
    absolute(mid - len, Math.min(mid + len, Date.now() / 1000));
  };
  const atNow = !!range.last;

  return html`<span class="timerange">
    <button type="button" class="small" title="Earlier, by the length of the range" onClick=${() => shift(-1)}>«</button>
    <span class="menu" ref=${box}>
      <button type="button" aria-haspopup="true" aria-expanded=${open} title="Time range" onClick=${toggle}>
        ${rangeLabel(range)} ▾</button>
      ${open && html`<${Picker} right=${right} range=${range} quick=${quick} onQuick=${s => { setRange({ last: s }); setOpen(false); }}
        onAbsolute=${(f, t) => { rememberRange({ from: f, to: t }); absolute(f, t); setOpen(false); }} />`}
    </span>
    <button type="button" class="small" title="Later, by the length of the range" disabled=${atNow} onClick=${() => shift(1)}>»</button>
    <button type="button" class="small" title="Zoom out: twice as long" onClick=${zoomOut}>−</button>
  </span>`;
}

function Picker({ range, quick, right, onQuick, onAbsolute }) {
  const [f0, t0] = windowOf(range);
  const [from, setFrom] = useState(toInput(new Date(f0 * 1000)));
  const [to, setTo] = useState(toInput(new Date(t0 * 1000)));
  const [error, setError] = useState(null);
  const [search, setSearch] = useState('');
  const recent = recentRanges();
  const apply = e => {
    e.preventDefault();
    const f = new Date(from).getTime() / 1000, t = new Date(to).getTime() / 1000;
    if (!from || !to || Number.isNaN(f) || Number.isNaN(t)) return setError('Both times are needed.');
    if (f >= t) return setError('From must be before To.');
    if (f > Date.now() / 1000) return setError('From is in the future.');
    onAbsolute(f, t);
  };
  const needle = search.trim().toLowerCase();
  const shown = quick.filter(([l]) => !needle || longName(l).toLowerCase().includes(needle) || l.includes(needle));
  return html`<div class=${'pop timepop' + (right ? ' right' : '')} role="dialog" aria-label="Time range">
    <form class="abs" onSubmit=${apply}>
      <div class="group">Absolute time range</div>
      <label>From<input type="datetime-local" aria-label="From" required value=${from} max=${toInput(new Date())} onInput=${e => { setFrom(e.target.value); setError(null); }} /></label>
      <label>To<input type="datetime-local" aria-label="To" required value=${to} onInput=${e => { setTo(e.target.value); setError(null); }} /></label>
      ${error && html`<div class="error" style="margin: 0">${error}</div>`}
      <button class="primary">Apply time range</button>
      ${recent.length > 0 && html`<div class="group">Recently used absolute ranges</div>
        ${recent.map(r => html`<button type="button" class="link recent" onClick=${() => onAbsolute(r.from, r.to)}>${rangeLabel(r)}</button>`)}`}
    </form>
    <div class="quick">
      <input type="search" placeholder="Search quick ranges" value=${search} onInput=${e => setSearch(e.target.value)} aria-label="Search quick ranges" />
      ${shown.map(([l, s]) => html`<button type="button" class=${'q' + (range.last === s ? ' on' : '')} onClick=${() => onQuick(s)}>${longName(l)}</button>`)}
      ${shown.length === 0 && html`<div class="muted" style="padding: 6px 8px">No quick range matches.</div>`}
    </div>
    <div class="zone muted">Browser time: ${zone()}</div>
  </div>`;
}
