import { html, useState, useEffect, useMemo, api, useLoad, setParams, Badge, ErrorBox, fmtNum, fmtUnit } from './lib.js';
import { useRange, rangeParams, RANGES, parseLines } from './explore.js';

// Sessions: what AI coding tools did, one session at a time. Everything
// comes from the events the tool sends (Claude Code: one log line per API
// call, prompt, tool call and finished subagent), so the page works
// without a trace store; a prompt links to its trace where one is kept.

const BASE = 'service:~"^claude-code"';
// Events that describe the program, not the work.
const NOISE = ['plugin_loaded', 'mcp_server_connection', 'managed_settings_resolved', 'api_request_body', 'api_response_body',
  'retention_sweep', 'hook_execution_start', 'hook_execution_complete'].map(n => '"claude_code.' + n + '"');
const form = o => ({ method: 'POST', body: new URLSearchParams(o) });
const q = s => JSON.stringify(String(s));
const num = v => { const n = Number(v); return isFinite(n) ? n : 0; };
const usd = v => '$' + (v >= 1 ? v.toFixed(2) : v >= 0.01 ? v.toFixed(3) : v.toFixed(4));
const secs = (a, b) => fmtUnit(Math.max(0, (Date.parse(b) - Date.parse(a)) / 1000), 'seconds');
const isSub = qs => (qs || '').startsWith('agent:');
// Claude Code sends this in place of text it was not told to send.
const shown = v => (v && v !== '<REDACTED>' ? v : null);

async function lines(query, from, to, limit = 1000) {
  return parseLines(await api('/api/v1/logs/query', form({ query, start: Math.floor(from), end: Math.ceil(to), limit })));
}

// who names the part of the tool that made an API call.
function who(e) {
  const s = e.query_source || '';
  if (isSub(s)) return 'subagent ' + (e.agent_name || s.split(':').pop());
  if (['sdk', 'main', 'repl_main_thread', ''].includes(s) || s.startsWith('repl_main')) return 'main conversation';
  return s.replaceAll('_', ' ');
}

function RangePicker({ range, setRange }) {
  return range.last
    ? html`<select aria-label="Time range" value=${range.last} onChange=${e => setRange({ last: +e.target.value })}>
        ${RANGES.map(([l, s]) => html`<option value=${s}>Last ${l}</option>`)}</select>`
    : html`<span class="badge info">${new Date(range.from * 1000).toLocaleString()} – ${new Date(range.to * 1000).toLocaleString()}</span>
        <button class="small" onClick=${() => setRange({ last: 86400 })}>Back to last 24h</button>`;
}

export function Sessions({ route }) {
  const [, id] = route.parts;
  if (id) return html`<${SessionView} key=${id} id=${decodeURIComponent(id)} params=${route.params} />`;
  return html`<${SessionList} params=${route.params} />`;
}

function SessionList({ params }) {
  const p = new URLSearchParams(params);
  if (!p.get('last') && !p.get('from')) p.set('last', '86400'); // sessions are long; a day is the useful default
  const [range, setRange, win] = useRange(p);
  const [person, setPerson] = useState(params.get('person') || '');
  const [order, setOrder] = useState(params.get('order') || 'recent');
  useEffect(() => { setParams(['sessions'], { ...rangeParams(range), ...(person ? { person } : {}), ...(order !== 'recent' ? { order } : {}) }); }, [range, person, order]);

  const { data, error } = useLoad(async () => {
    const [from, to] = win();
    const [calls, prompts, errors] = await Promise.all([
      lines(`${BASE} _msg:"claude_code.api_request" | stats by (session_id, user_email, agent) sum(cost_usd) as usd,
        sum(cost_usd) if (query_source:~"^agent:") as sub_usd, count() as calls, sum(input_tokens) as input, sum(output_tokens) as output,
        sum(cache_read_tokens) as cache_read, sum(cache_creation_tokens) as cache_write, min(_time) as started, max(_time) as last
        | sort by (last desc) | limit 500`, from, to, 500),
      lines(`${BASE} _msg:"claude_code.user_prompt" | stats by (session_id) count() as prompts`, from, to, 5000),
      lines(`${BASE} _msg:"claude_code.api_error" | stats by (session_id) count() as errors`, from, to, 5000),
    ]);
    const pr = Object.fromEntries(prompts.map(r => [r.session_id, num(r.prompts)]));
    const er = Object.fromEntries(errors.map(r => [r.session_id, num(r.errors)]));
    return calls.filter(r => r.session_id).map(r => ({
      id: r.session_id, person: r.user_email || '', agent: r.agent || '', usd: num(r.usd), sub: num(r.sub_usd), calls: num(r.calls),
      tokens: num(r.input) + num(r.output) + num(r.cache_read) + num(r.cache_write), cached: num(r.cache_read),
      started: r.started, last: r.last, prompts: pr[r.session_id] || 0, errors: er[r.session_id] || 0,
    }));
  }, [range], 30000);

  const people = useMemo(() => [...new Set((data || []).map(s => s.person).filter(Boolean))].sort(), [data]);
  const shown = useMemo(() => {
    const rows = (data || []).filter(s => !person || s.person === person);
    return order === 'cost' ? [...rows].sort((a, b) => b.usd - a.usd) : rows;
  }, [data, person, order]);
  const total = shown.reduce((t, s) => t + s.usd, 0);
  const open = s => {
    const from = Math.floor(Date.parse(s.started) / 1000) - 300, to = Math.ceil(Date.parse(s.last) / 1000) + 300;
    location.hash = '#/sessions/' + encodeURIComponent(s.id) + '?' + new URLSearchParams({ from, to });
  };

  return html`
    <div class="head"><h1>Sessions</h1><span class="spacer"></span>
      ${people.length > 1 && html`<select aria-label="Person" value=${person} onChange=${e => setPerson(e.target.value)}>
        <option value="">Everyone</option>${people.map(x => html`<option value=${x}>${x}</option>`)}</select>`}
      <select aria-label="Order" value=${order} onChange=${e => setOrder(e.target.value)}>
        <option value="recent">Most recent first</option><option value="cost">Most expensive first</option></select>
      <${RangePicker} range=${range} setRange=${setRange} />
      <a class="btn" href="/docs/claude-code" target="_blank" rel="noopener">Help</a></div>
    <${ErrorBox} error=${error} />
    ${data && shown.length > 0 && html`<p class="muted">${shown.length} sessions, ${usd(total)} at API prices.</p>`}
    <div class="panel flush">${!data ? html`<div class="empty">Loading…</div>` : shown.length === 0
      ? html`<div class="empty">No sessions in this time range. Claude Code reports to the agent on its machine once the
          <code>ai_module_claude</code> integration is switched on and Claude Code's own settings point at the agent.
          <a href="/docs/claude-code" target="_blank" rel="noopener">How</a></div>`
      : html`<table>
        <thead><tr><th>Last active</th><th>Person</th><th>Machine</th><th style="text-align: right">Prompts</th><th style="text-align: right">API calls</th>
          <th style="text-align: right">Tokens</th><th style="text-align: right">Cost</th><th style="text-align: right">By subagents</th><th>Ran for</th><th></th></tr></thead>
        <tbody>${shown.map(s => html`<tr class="click" onClick=${() => open(s)}>
          <td style="white-space: nowrap" title=${s.last}>${new Date(s.last).toLocaleString()}</td>
          <td>${s.person}</td><td class="muted">${s.agent}</td>
          <td class="num">${s.prompts}</td><td class="num">${s.calls}</td>
          <td class="num" title=${`${fmtNum(s.cached)} read from cache`}>${fmtNum(s.tokens)}</td>
          <td class="num"><strong>${usd(s.usd)}</strong></td>
          <td class="num">${s.sub > 0 ? Math.round((100 * s.sub) / s.usd) + '%' : '–'}</td>
          <td class="muted">${secs(s.started, s.last)}</td>
          <td>${s.errors > 0 && html`<${Badge} kind="warn">${s.errors} API errors<//>`}</td></tr>`)}</tbody></table>`}</div>
    <p class="muted">Cost is what Claude Code computes at API prices; on a subscription it is what the same use would cost through the API.
      Sessions that made no API call in the shown time are not listed.</p>`;
}

function SessionView({ id, params }) {
  const p = new URLSearchParams(params);
  if (!p.get('last') && !p.get('from')) p.set('last', '604800');
  const [range, setRange, win] = useRange(p);
  const [open, setOpen] = useState(null);
  useEffect(() => { setParams(['sessions', encodeURIComponent(id)], rangeParams(range)); }, [range]);

  const { data: events, error } = useLoad(async () => {
    const [from, to] = win();
    const rows = await lines(`${BASE} session_id:=${q(id)} -_msg:in(${NOISE.join(',')}) | sort by (_time) | limit 5000`, from, to, 5000);
    // By time; Claude Code's sequence number orders its own events that
    // share a millisecond (hook lines have none).
    return rows.sort((a, b) => Date.parse(a._time) - Date.parse(b._time) || num(a.event_sequence) - num(b.event_sequence));
  }, [range, id], 30000);

  const view = useMemo(() => {
    if (!events || events.length === 0) return null;
    const kind = e => e._msg.replace(/^claude_code\./, '');
    const calls = events.filter(e => kind(e) === 'api_request');
    const sum = (rows, f) => rows.reduce((t, e) => t + num(e[f]), 0);
    // One group per prompt, in the order the prompts came.
    const groups = [], byPrompt = new Map();
    for (const e of events) {
      const key = e.prompt_id || '';
      if (!byPrompt.has(key)) { const g = { key, events: [] }; byPrompt.set(key, g); groups.push(g); }
      byPrompt.get(key).events.push(e);
    }
    const prompts = groups.map(g => {
      const ev = g.events, c = ev.filter(e => kind(e) === 'api_request');
      const prompt = ev.find(e => kind(e) === 'user_prompt');
      const hooked = ev.find(e => e.hook_event === 'UserPromptSubmit' && e.prompt); // vigil claude-hook -content
      const text = (prompt && (shown(prompt.prompt_text) || shown(prompt.prompt))) || (hooked && hooked.prompt);
      return {
        key: g.key, events: ev, start: ev[0]._time, end: ev[ev.length - 1]._time,
        text: text || null, length: prompt ? num(prompt.prompt_length) : hooked ? num(hooked.prompt_length) : 0,
        usd: sum(c, 'cost_usd'), subUsd: sum(c.filter(e => isSub(e.query_source)), 'cost_usd'), calls: c.length,
        tools: ev.filter(e => kind(e) === 'tool_result').length, failed: ev.filter(e => kind(e) === 'tool_result' && e.success === 'false').length,
        subagents: ev.filter(e => kind(e) === 'subagent_completed').map(e => e.agent_type || 'subagent'),
        errors: ev.filter(e => kind(e) === 'api_error').length, trace: (ev.find(e => e.trace_id) || {}).trace_id,
      };
    });
    const parts = new Map();
    for (const e of calls) {
      const k = who(e), r = parts.get(k) || { name: k, usd: 0, calls: 0, output: 0 };
      r.usd += num(e.cost_usd); r.calls++; r.output += num(e.output_tokens);
      parts.set(k, r);
    }
    // Claude Code's own events say whose session it is; hook lines know
    // only the operating-system account.
    const first = events.find(e => e.user_email) || events[0];
    const osUser = (events.find(e => e.os_user) || {}).os_user;
    return {
      person: first.user_email || osUser || '', agent: first.agent, service: first.service, start: events[0]._time, end: events[events.length - 1]._time,
      osUser, cwd: (events.find(e => e.cwd) || {}).cwd,
      usd: sum(calls, 'cost_usd'), calls: calls.length, input: sum(calls, 'input_tokens'), output: sum(calls, 'output_tokens'),
      cacheRead: sum(calls, 'cache_read_tokens'), cacheWrite: sum(calls, 'cache_creation_tokens'),
      models: [...new Set(calls.map(e => e.model).filter(Boolean))],
      prompts, parts: [...parts.values()].sort((a, b) => b.usd - a.usd),
    };
  }, [events]);

  const back = html`<a href="#/sessions">← Sessions</a>`;
  if (error) return html`<div class="head">${back}<h1>Session</h1></div><${ErrorBox} error=${error} />`;
  if (!events) return html`<div class="empty">Loading…</div>`;
  if (!view) return html`<div class="head">${back}<h1>Session</h1><span class="spacer"></span><${RangePicker} range=${range} setRange=${setRange} /></div>
    <div class="panel empty">Nothing from session <span class="mono">${id}</span> in this time range. Choose a longer one, or the events may be older than logs are kept.</div>`;
  const read = view.input + view.cacheRead + view.cacheWrite;
  return html`
    <div class="head">${back}<h1>Session</h1><span class="mono muted">${id}</span><span class="spacer"></span>
      <${RangePicker} range=${range} setRange=${setRange} />
      <a class="btn" href=${`#/explore/logs?q=${encodeURIComponent(`${BASE} session_id:=${q(id)}`)}&${new URLSearchParams(rangeParams(range))}`}>All its events</a></div>
    <div class="grid">
      ${[['Cost', usd(view.usd)], ['API calls', view.calls], ['Prompts', view.prompts.filter(x => x.key).length],
        ['Tokens out', fmtNum(view.output)], ['Read from cache', read > 0 ? Math.round((100 * view.cacheRead) / read) + '%' : '–'],
        ['Ran for', secs(view.start, view.end)]].map(([t, v]) => html`
        <section class="panel tile" style="grid-column: span 2; height: 110px"><header><h2>${t}</h2></header>
          <div class="tile-body"><div class="stat">${v}</div></div></section>`)}
    </div>
    <p class="muted">${view.person} on ${view.agent} (${view.service})${view.osUser ? ` · account ${view.osUser}` : ''}${view.cwd ? ` · in ${view.cwd}` : ''} · ${new Date(view.start).toLocaleString()} · ${view.models.join(', ')}</p>

    <h2 class="section">Who spent it</h2>
    <div class="panel flush"><table>
      <thead><tr><th>Part</th><th style="text-align: right">API calls</th><th style="text-align: right">Tokens out</th><th style="text-align: right">Cost</th><th style="text-align: right">Share</th></tr></thead>
      <tbody>${view.parts.map(r => html`<tr><td>${r.name}</td><td class="num">${r.calls}</td><td class="num">${fmtNum(r.output)}</td>
        <td class="num">${usd(r.usd)}</td><td class="num">${view.usd > 0 ? Math.round((100 * r.usd) / view.usd) + '%' : '–'}</td></tr>`)}</tbody></table></div>

    <h2 class="section">Prompts</h2>
    <div class="panel flush"><table>
      <thead><tr><th>At</th><th>Prompt</th><th style="text-align: right">Cost</th><th style="text-align: right">API calls</th><th style="text-align: right">Tools</th>
        <th>Subagents</th><th>Took</th><th></th></tr></thead>
      <tbody>${view.prompts.map(x => html`
        <tr class="click" onClick=${() => setOpen(open === x.key ? null : x.key)}>
          <td style="white-space: nowrap">${new Date(x.start).toLocaleTimeString()}</td>
          <td>${!x.key ? html`<span class="muted">start of the session</span>`
            : x.text ? html`<span title=${x.text}>${x.text.length > 140 ? x.text.slice(0, 140) + '…' : x.text}</span>`
            : html`<span class="muted">${x.length} characters (prompt text is not sent)</span>`}</td>
          <td class="num">${x.calls ? usd(x.usd) : '–'}${x.subUsd > 0 && html`<div class="muted">${usd(x.subUsd)} subagents</div>`}</td>
          <td class="num">${x.calls || '–'}</td>
          <td class="num">${x.tools || '–'}${x.failed > 0 && html` <${Badge} kind="warn">${x.failed} failed<//>`}</td>
          <td>${x.subagents.map(s => html`<span class="chip">${s}</span>`)}</td>
          <td class="muted">${secs(x.start, x.end)}</td>
          <td>${x.errors > 0 && html`<${Badge} kind="warn">${x.errors} API errors<//>`}
            ${x.trace && html` <a href=${`#/services/~trace/${x.trace}?t=${Math.round(Date.parse(x.start) / 1000)}`} onClick=${e => e.stopPropagation()}>trace</a>`}</td>
        </tr>
        ${open === x.key && html`<tr><td colspan="8" style="padding: 0"><${Events} events=${x.events} /></td></tr>`}`)}</tbody></table></div>
    <p class="muted">Click a prompt for what happened in it, in order. A trace, where one is kept, shows subagents nested under the tool call that started them.</p>`;
}

// hookLine describes a line from vigil claude-hook: the audit trail, with
// who ran what where, next to Claude Code's own telemetry.
function hookLine(e) {
  const what = e.tool_command || e.tool_file_path || e.tool_pattern || e.tool_url || e.tool_query || e.tool_description || '';
  const by = e.agent_type ? ` (in subagent ${e.agent_type})` : '';
  switch (e.hook_event) {
    case 'PostToolUse': return ['audit: ran', `${e.tool_name} ${what}${by} · ${e.os_user || ''} in ${e.cwd || ''}`];
    case 'PostToolUseFailure': return ['audit: failed', `${e.tool_name} ${what}${by} · ${e.error || ''}`, 'warn'];
    case 'PermissionRequest': return ['audit: asked', `${e.tool_name} ${what}${by}`];
    case 'PermissionDenied': return ['audit: denied', `${e.tool_name} ${what}${by} · ${e.reason || ''}`, 'warn'];
    case 'SubagentStart': return ['audit: subagent', `${e.agent_type} started`];
    case 'SubagentStop': return ['audit: subagent', `${e.agent_type} stopped`];
    case 'UserPromptSubmit': return ['audit: prompt', e.prompt || `${e.prompt_length} characters`];
    case 'SessionStart': return ['audit: session', `started (${e.start_source || ''}) by ${e.os_user || ''} in ${e.cwd || ''}`];
    case 'SessionEnd': return ['audit: session', `ended (${e.reason || ''})`];
    default: return ['audit', e.hook_event || ''];
  }
}

// Events lists what happened within one prompt, one line each.
function Events({ events }) {
  const t0 = Date.parse(events[0]._time);
  const line = e => {
    switch (e._msg) {
      case 'claude_code.user_prompt': return ['prompt', shown(e.prompt_text) || shown(e.prompt) || `${e.prompt_length} characters`];
      case 'claude_code.api_request': return ['API call', `${who(e)} · ${e.model} · ${fmtNum(num(e.input_tokens) + num(e.cache_read_tokens) + num(e.cache_creation_tokens))} in, ${fmtNum(num(e.output_tokens))} out · ${usd(num(e.cost_usd))} · ${fmtUnit(num(e.duration_ms) / 1000, 'seconds')}`];
      case 'claude_code.api_error': return ['API error', `${e.status_code || ''} ${e.error || ''} (attempt ${e.attempt || 1})`, 'bad'];
      case 'claude_code.assistant_response': return ['reply', `${who(e)}: ${shown(e.response) ? e.response.slice(0, 300) : num(e.response_length) + ' characters'}`];
      case 'claude_code.tool_decision': return ['permission', `${e.tool_name} ${e.decision === 'reject' ? 'refused' : 'allowed'}${e.attr_source ? ' (' + e.attr_source + ')' : ''}`, e.decision === 'reject' ? 'warn' : ''];
      case 'claude_code.tool_result': return ['tool', `${e.tool_name}${e.tool_input ? ' ' + e.tool_input.slice(0, 200) : ''} · ${fmtUnit(num(e.duration_ms) / 1000, 'seconds')}${e.success === 'false' ? ' · failed ' + (e.error || '') : ''}`, e.success === 'false' ? 'warn' : ''];
      case 'claude_code.subagent_completed': return ['subagent done', `${e.agent_type} · ${fmtNum(num(e.total_tokens))} tokens · ${e.total_tool_uses} tool uses · ${fmtUnit(num(e.duration_ms) / 1000, 'seconds')}`];
      case 'claude_code.api_refusal': return ['refusal', `${e.model} ${e.category || ''}`, 'warn'];
      default: return e._msg.startsWith('claude_code.hook.') ? hookLine(e) : [e._msg.replace(/^claude_code\./, '').replaceAll('_', ' '), ''];
    }
  };
  return html`<table class="events">${events.map(e => { const [k, text, lvl] = line(e); return html`<tr>
    <td class="muted num" style="width: 70px">+${((Date.parse(e._time) - t0) / 1000).toFixed(1)}s</td>
    <td style="width: 120px">${lvl ? html`<${Badge} kind=${lvl}>${k}<//>` : k}</td>
    <td class="mono" style="white-space: pre-wrap; word-break: break-word">${text}</td></tr>`; })}</table>`;
}
