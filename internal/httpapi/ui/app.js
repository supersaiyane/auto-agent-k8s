// auto-agent dashboard (PLAN-002 11.2). No inline script or style is used,
// so the Content-Security-Policy can forbid both (ISS-060). Every value from
// the API goes through esc() before it reaches markup.
'use strict';
(() => {
  const $ = (id) => document.getElementById(id);
  const TOKEN_KEY = 'autoAgentToken';
  const RATE_KEY = 'autoAgentRefresh';
  const PALETTE = ['#ef4444', '#f97316', '#eab308', '#22c55e', '#38bdf8', '#a78bfa', '#ec4899', '#14b8a6'];
  const RUNGS = { R0: 'alert only', R1: 'guided fix', R2: 'pull request', R3: 'approve to fix', R4: 'automatic fix' };
  const hooks = { renders: 0 }; // read by the browser test
  const state = { tab: 'events', type: '', ns: '', sev: '', result: '', q: '', fixes: 'all', days: 30,
    detail: null, refreshMs: 5000, timer: null, startedAt: null, namespaces: new Set(), busy: false };

  // ---------- escaping and formatting ----------
  const ESC = { '&': '&amp;', '<': '&lt;', '>': '&gt;', '"': '&quot;', "'": '&#39;' };
  const esc = (v) => String(v ?? '').replace(/[&<>"']/g, (c) => ESC[c]);
  const num = (v) => (Number.isFinite(+v) ? +v : 0);
  const trunc = (s, n) => { s = String(s ?? ''); return s.length > n ? s.slice(0, n) + '...' : s; };
  const oneOf = (v, allowed, dflt) => (allowed.includes(v) ? v : dflt);
  const safeURL = (u) => (/^(https?:|s3:)/i.test(String(u || '')) ? String(u) : '');
  const when = (ts) => { const d = new Date(ts); return isNaN(d) ? '' : esc(d.toLocaleDateString()) + '<br>' + esc(d.toLocaleTimeString()); };
  const money = (v, digits = 0) => '$' + num(v).toFixed(digits);
  const pill = (text, color) => `<span class="pill pill-${oneOf(color, ['green', 'red', 'yellow', 'blue', 'muted', 'purple'], 'muted')}">${esc(text)}</span>`;
  const empty = (text) => `<div class="empty">${esc(text)}</div>`;

  // ---------- token and API ----------
  const store = (area, key, value) => { try { value === undefined ? area.removeItem(key) : area.setItem(key, value); } catch (e) { /* storage blocked */ } };
  const load = (area, key) => { try { return area.getItem(key) || ''; } catch (e) { return ''; } };
  class AuthError extends Error {}
  function showLogin(message) {
    $('login').hidden = false;
    $('login-error').textContent = message || '';
    $('token-input').focus();
  }
  async function api(path, opts = {}) {
    const token = load(sessionStorage, TOKEN_KEY);
    if (!token) { showLogin(); throw new AuthError('signed out'); }
    const r = await fetch(path, { ...opts, headers: { ...(opts.headers || {}), Authorization: 'Bearer ' + token } });
    if (r.status === 401) { store(sessionStorage, TOKEN_KEY); showLogin('The token was refused.'); throw new AuthError('refused'); }
    if (!r.ok) throw new Error(`${path} answered ${r.status}`);
    return r.json();
  }

  // ---------- filters ----------
  const FILTERS = { events: ['ns', 'sev', 'search'], audit: ['ns', 'sev', 'result', 'search'], dryrun: ['ns', 'search'],
    deploys: ['ns', 'search'], baselines: ['ns', 'search'], k8sevents: ['ns', 'search'] };
  function showToolbar() {
    const on = FILTERS[state.tab] || [];
    $('toolbar').hidden = on.length === 0;
    for (const f of ['ns', 'sev', 'result', 'search']) {
      const ctl = $('f-' + f);
      ctl.hidden = !on.includes(f);
      document.querySelector(`label[for="f-${f}"]`).hidden = !on.includes(f);
    }
  }
  function rememberNamespaces(items) {
    let added = false;
    for (const it of items) if (it.namespace && !state.namespaces.has(it.namespace)) { state.namespaces.add(it.namespace); added = true; }
    if (!added) return;
    const sel = $('f-ns');
    sel.innerHTML = '<option value="">all</option>' + [...state.namespaces].sort()
      .map((n) => `<option value="${esc(n)}">${esc(n)}</option>`).join('');
    sel.value = state.ns;
  }
  function filtered(items, fields) {
    const q = state.q.toLowerCase();
    return items.filter((e) => (!state.ns || e.namespace === state.ns)
      && (!state.sev || e.severity === state.sev)
      && (!state.result || e.result === state.result)
      && (!q || fields.some((f) => String(e[f] ?? '').toLowerCase().includes(q))));
  }
  const setCount = (shown, total) => { $('f-count').textContent = total ? `${shown} of ${total}` : ''; };

  // ---------- rows ----------
  function eventRow(e) {
    const sev = oneOf(e.severity, ['critical', 'warning', 'info'], 'info');
    const type = oneOf(e.type, ['incident', 'action', 'scaling', 'anomaly', 'info', 'audit'], 'info');
    const rung = /^R[0-4]$/.test(e.rung || '') ? `<span class="pill pill-purple" title="${esc(RUNGS[e.rung])}">${esc(e.rung)}</span> ` : '';
    const result = e.result ? pill(e.result, { success: 'green', failed: 'red', blocked: 'yellow', simulated: 'purple', suggested: 'blue' }[e.result]) + ' ' : '';
    const act = e.action ? pill(e.action, 'green') + ' ' : '';
    const log = safeURL(e.logUrl) ? `<a class="link" href="${esc(safeURL(e.logUrl))}" rel="noopener noreferrer">logs</a>` : '';
    const wl = e.workload ? ` <button type="button" class="link" data-action="workload" data-ns="${esc(e.namespace)}" data-wl="${esc(e.workload)}">${esc(e.workload)}</button>` : '';
    const node = e.node ? ` <span class="muted mono">${esc(e.node)}</span>` : '';
    return `<div class="event sev-${sev}"><div class="time">${when(e.timestamp)}</div>
      <div><span class="dot dot-${sev}"></span><span class="type type-${type}">${esc(e.type)}</span></div>
      <div>${e.namespace ? `<span class="ns">${esc(e.namespace)}</span> ` : ''}<span class="reason">${esc(e.reason)}</span>${esc(trunc(e.message, 140))}${wl}${node}</div>
      <div>${rung}${result}${act}${log}</div></div>`;
  }
  const list = (rows, none) => (rows.length ? `<div class="list">${rows.join('')}</div>` : empty(none));

  // ---------- views ----------
  async function viewEvents() {
    const evts = await api('/api/events?limit=500' + (state.type ? '&type=' + encodeURIComponent(state.type) : ''));
    rememberNamespaces(evts);
    const shown = filtered(evts, ['reason', 'workload', 'message', 'pod', 'node']);
    setCount(shown.length, evts.length);
    return list(shown.map(eventRow), state.type ? `No ${state.type} events.` : 'No events yet.');
  }
  async function viewAudit(onlySimulated) {
    const evts = await api('/api/events?limit=500&type=audit');
    rememberNamespaces(evts);
    let shown = filtered(evts, ['reason', 'workload', 'message', 'action', 'node']);
    if (onlySimulated) shown = shown.filter((e) => e.result === 'simulated');
    setCount(shown.length, evts.length);
    const intro = onlySimulated
      ? '<p class="muted small">What the agent would have done in dry-run mode, on every node.</p>'
      : '<p class="muted small">Every decision of the mutation gate: applied, simulated, suggested, blocked or failed.</p>';
    return intro + list(shown.map(eventRow), onlySimulated ? 'No simulated actions.' : 'No gate decisions yet.');
  }
  async function viewFixes() {
    const d = await api('/api/fixes');
    const box = (key, label, value, color) => `<button type="button" class="stat-box${state.fixes === key ? ' active' : ''}" data-action="fixes" data-arg="${key}">
      <span class="label">${label}</span><span class="val ${color}">${num(value)}</span></button>`;
    let h = `<div class="mini-row">${box('all', 'All', num(d.fixedCount) + num(d.pendingCount) + num(d.failedCount), 'accent')}
      ${box('fixed', 'Verified fixed', d.fixedCount, 'green')}${box('pending', 'Verifying', d.pendingCount, 'yellow')}${box('failed', 'Not fixed', d.failedCount, 'red')}</div>`;
    const group = (items, label, color) => (items || []).map((f) => `<div class="event sev-${color === 'red' ? 'critical' : color === 'yellow' ? 'warning' : 'info'}">
      <div class="time">${when(f.timestamp)}</div><div>${pill(label, color)}</div>
      <div><span class="ns">${esc(f.namespace)}</span> <span class="reason">${esc(f.reason)}</span>${esc(f.detail || f.action || '')}</div>
      <div class="muted">${esc(f.workload)}</div></div>`);
    const rows = [];
    if (state.fixes === 'all' || state.fixes === 'fixed') rows.push(...group(d.fixed, 'fixed', 'green'));
    if (state.fixes === 'all' || state.fixes === 'pending') rows.push(...group(d.pending, 'verifying', 'yellow'));
    if (state.fixes === 'all' || state.fixes === 'failed') rows.push(...group(d.failed, 'not fixed', 'red'));
    return h + list(rows, 'No remediation yet.');
  }
  async function viewCompliance() {
    const r = await api('/api/compliance?days=' + num(state.days));
    const days = [7, 30, 90].map((n) => `<button type="button" class="btn${state.days === n ? ' primary' : ''}" data-action="days" data-arg="${n}">${n} days</button>`).join(' ');
    const stat = (label, value, color) => `<div class="stat-box static"><span class="label">${label}</span><span class="val ${color}">${esc(value)}</span></div>`;
    const table = (title, obj) => {
      const rows = Object.entries(obj || {}).sort((a, b) => b[1] - a[1]);
      return `<h4>${title}</h4>` + (rows.length ? `<table><tr><th>Name</th><th>Count</th></tr>${rows.map(([k, v]) => `<tr><td>${esc(k)}</td><td>${num(v)}</td></tr>`).join('')}</table>` : empty('None'));
    };
    return `<p class="muted small">Period ${esc(r.period)}. ${days}</p><div class="mini-row">
      ${stat('Incidents', num(r.totalIncidents), 'red')}${stat('Remediated', num(r.autoRemediated), 'green')}
      ${stat('Manual', num(r.manualRequired), 'yellow')}${stat('Blocked', num(r.blocked), 'purple')}
      ${stat('Remediation rate', num(r.remediationRate).toFixed(1) + '%', 'accent')}${stat('Mean time to recover', Math.round(num(r.avgMttrSeconds)) + 's', 'accent')}</div>
      <div class="chart-row"><div>${table('Incidents by reason', r.incidentsByReason)}</div><div>${table('Incidents by namespace', r.incidentsByNamespace)}</div><div>${table('Actions applied', r.actionsByType)}</div></div>`;
  }
  async function viewDeploys() {
    const all = await api('/api/deploys');
    rememberNamespaces(all);
    const shown = filtered(all, ['deployment', 'image']).sort((a, b) => new Date(b.timestamp) - new Date(a.timestamp));
    setCount(shown.length, all.length);
    if (!shown.length) return empty('No rollouts recorded yet. The leader records them on its job loop.');
    return `<table><tr><th>When</th><th>Namespace</th><th>Deployment</th><th>Revision</th><th>Image</th><th>Replicas</th></tr>${shown.map((d) => `<tr>
      <td class="time">${when(d.timestamp)}</td><td><span class="ns">${esc(d.namespace)}</span></td><td><strong>${esc(d.deployment)}</strong></td>
      <td>${num(d.revision)}</td><td class="mono">${esc(d.image)}</td><td>${num(d.replicas)}</td></tr>`).join('')}</table>`;
  }
  async function viewBaselines() {
    const d = await api('/api/baselines');
    const all = Object.values(d.baselines || {});
    rememberNamespaces(all);
    const shown = filtered(all, ['workload']);
    setCount(shown.length, all.length);
    const banner = d.learning ? pill('learning', 'yellow') + ' <span class="muted">Baselines are still being learned; anomalies are not reported yet.</span>' : '';
    if (!shown.length) return banner + empty('No baselines. Learning mode is off or has no samples yet.');
    return `<p class="small">${banner}</p><table><tr><th>Namespace</th><th>Workload</th><th>Samples</th><th>Avg CPU</th><th>CPU std dev</th><th>Avg restarts</th><th>Max restarts</th><th>Normal replicas</th></tr>${shown.map((b) => `<tr>
      <td><span class="ns">${esc(b.namespace)}</span></td><td><strong>${esc(b.workload)}</strong></td><td>${num(b.sampleCount)}</td>
      <td>${num(b.avgCpu).toFixed(3)}</td><td>${num(b.stdDevCpu).toFixed(3)}</td><td>${num(b.avgRestarts).toFixed(1)}</td><td>${num(b.maxRestarts)}</td><td>${num(b.normalReplicas)}</td></tr>`).join('')}</table>`;
  }
  async function viewK8sEvents() {
    const evts = await api('/api/k8s-events' + (state.ns ? '?namespace=' + encodeURIComponent(state.ns) : ''));
    rememberNamespaces(evts);
    const shown = filtered(evts, ['reason', 'name', 'message', 'kind']);
    setCount(shown.length, evts.length);
    return list(shown.map((e) => {
      const warn = e.type === 'Warning';
      return `<div class="event sev-${warn ? 'warning' : 'info'}"><div class="time">${when(e.timestamp)}</div>
        <div><span class="dot dot-${warn ? 'warning' : 'info'}"></span><span class="type">${esc(e.type)}</span></div>
        <div><span class="ns">${esc(e.namespace)}</span> <span class="muted">${esc(e.kind)}/</span><span class="reason">${esc(e.name)}</span>${esc(trunc(e.message, 140))}${num(e.count) > 1 ? ` <span class="muted">x${num(e.count)}</span>` : ''}</div>
        <div class="muted">${esc(e.reason)}</div></div>`;
    }), 'No Kubernetes events.');
  }

  // ---------- charts (SVG attributes only: the policy forbids inline style) ----------
  function pie(slices) {
    const total = slices.reduce((s, x) => s + num(x.value), 0);
    if (!total) return empty('No data');
    let off = 0;
    const arcs = slices.filter((s) => num(s.value)).map((s) => {
      const len = (num(s.value) / total) * 251.33;
      const arc = `<circle cx="50" cy="50" r="40" fill="none" stroke="${s.color}" stroke-width="18" stroke-dasharray="${len} ${251.33 - len}" stroke-dashoffset="${-off}" transform="rotate(-90 50 50)"/>`;
      off += len;
      return arc;
    }).join('');
    const legend = slices.filter((s) => num(s.value)).map((s) => `<span><svg width="8" height="8"><circle cx="4" cy="4" r="4" fill="${s.color}"/></svg>${esc(s.label)}: ${num(s.value)}</span>`).join('');
    return `<svg viewBox="0 0 100 100" width="170" height="170" role="img" aria-label="pie chart">${arcs}<text x="50" y="55" text-anchor="middle" class="pie-total">${total}</text></svg><div class="legend">${legend}</div>`;
  }
  function bars(entries, color) {
    if (!entries.length) return empty('No data');
    const max = Math.max(...entries.map((e) => num(e[1]))) || 1;
    const w = Math.max(entries.length * 46, 200);
    const cols = entries.map(([label, value], i) => {
      const h = (num(value) / max) * 120;
      const x = i * 46 + 6;
      return `<rect x="${x}" y="${140 - h}" width="34" height="${h}" rx="3" fill="${color || PALETTE[i % PALETTE.length]}"/>
        <text x="${x + 17}" y="${134 - h}" text-anchor="middle" class="bar-val">${num(value) % 1 ? num(value).toFixed(1) : num(value)}</text>
        <text x="${x + 17}" y="154" text-anchor="middle" class="bar-label">${esc(trunc(label, 9))}</text>`;
    }).join('');
    return `<svg viewBox="0 0 ${w} 160" width="100%" height="170" role="img" aria-label="bar chart">${cols}</svg>`;
  }
  async function viewCharts() {
    const [evts, fixes, resources, cost] = await Promise.all([api('/api/events?limit=500'), api('/api/fixes'), api('/api/resources'), api('/api/cost')]);
    const byType = {}; const byReason = {}; const byNs = {};
    for (const e of evts) {
      byType[e.type] = (byType[e.type] || 0) + 1;
      if (e.type === 'incident') byReason[e.reason] = (byReason[e.reason] || 0) + 1;
      if (e.namespace) byNs[e.namespace] = (byNs[e.namespace] || 0) + 1;
    }
    const sizing = { right: 0, over: 0, under: 0, none: 0 };
    for (const n of resources || []) { sizing.right += num(n.healthy); sizing.over += num(n.overuse); sizing.under += num(n.underuse); sizing.none += num(n.noLimits); }
    const top = (o, n) => Object.entries(o).sort((a, b) => b[1] - a[1]).slice(0, n);
    const card = (title, body) => `<div class="chart-card"><h4>${title}</h4>${body}</div>`;
    let h = '<div class="chart-row">'
      + card('Events by type', pie([{ label: 'incidents', value: byType.incident, color: '#ef4444' }, { label: 'actions', value: byType.action, color: '#22c55e' },
        { label: 'gate decisions', value: byType.audit, color: '#a78bfa' }, { label: 'scaling', value: byType.scaling, color: '#38bdf8' }]))
      + card('Remediation', pie([{ label: 'fixed', value: fixes.fixedCount, color: '#22c55e' }, { label: 'verifying', value: fixes.pendingCount, color: '#eab308' },
        { label: 'not fixed', value: fixes.failedCount, color: '#ef4444' }]))
      + card('Pod sizing', pie([{ label: 'right-sized', value: sizing.right, color: '#22c55e' }, { label: 'overuse', value: sizing.over, color: '#ef4444' },
        { label: 'underuse', value: sizing.under, color: '#eab308' }, { label: 'no limits', value: sizing.none, color: '#f97316' }]))
      + '</div><div class="chart-row">'
      + card('Top incident reasons', bars(top(byReason, 10)))
      + card('Events by namespace', bars(top(byNs, 8), '#38bdf8')) + '</div>';
    const costNs = (cost && cost.namespaces || []).filter((n) => num(n.monthly) > 0).sort((a, b) => b.monthly - a.monthly).slice(0, 8);
    if (costNs.length) h += `<div class="chart-row">${card('Monthly cost by namespace', bars(costNs.map((n) => [n.name, num(n.monthly)]), '#22c55e'))}</div>`;
    return h;
  }
  async function viewReport() {
    const [evts, fixes] = await Promise.all([api('/api/events?limit=500'), api('/api/fixes')]);
    const services = {}; const reasons = {};
    let incidents = 0;
    for (const e of evts) {
      if (e.type !== 'incident' && e.type !== 'action') continue;
      const k = e.namespace + '/' + e.workload;
      const s = services[k] || (services[k] = { namespace: e.namespace, workload: e.workload, incidents: 0, actions: 0 });
      if (e.type === 'incident') { s.incidents++; incidents++; reasons[e.reason] = (reasons[e.reason] || 0) + 1; } else s.actions++;
    }
    const svc = Object.values(services).sort((a, b) => b.incidents - a.incidents);
    let h = `<h3>Incident report</h3><div class="mini-row">
      <div class="stat-box static"><span class="label">Incidents</span><span class="val red">${incidents}</span></div>
      <div class="stat-box static"><span class="label">Verified fixed</span><span class="val green">${num(fixes.fixedCount)}</span></div>
      <div class="stat-box static"><span class="label">Verifying</span><span class="val yellow">${num(fixes.pendingCount)}</span></div>
      <div class="stat-box static"><span class="label">Not fixed</span><span class="val red">${num(fixes.failedCount)}</span></div></div>`;
    h += '<h4>By service</h4>' + (svc.length ? `<table><tr><th>Namespace</th><th>Workload</th><th>Incidents</th><th>Actions</th></tr>${svc.map((s) => `<tr class="clickable" data-action="workload" data-ns="${esc(s.namespace)}" data-wl="${esc(s.workload)}">
      <td><span class="ns">${esc(s.namespace)}</span></td><td><strong>${esc(s.workload)}</strong></td><td>${s.incidents}</td><td>${s.actions}</td></tr>`).join('')}</table>` : empty('No incidents'));
    const rs = Object.entries(reasons).sort((a, b) => b[1] - a[1]);
    h += '<h4>By reason</h4>' + (rs.length ? `<table><tr><th>Reason</th><th>Count</th><th>Share</th></tr>${rs.map(([r, c]) => `<tr class="clickable" data-action="reason" data-arg="${esc(r)}">
      <td><strong>${esc(r)}</strong></td><td>${c}</td><td>${(c / incidents * 100).toFixed(1)}%</td></tr>`).join('')}</table>` : '');
    return h;
  }
  async function viewCluster() {
    if (state.detail) return viewNamespace(state.detail);
    const data = await api('/api/cluster');
    if (!data.length) return empty('No namespaces');
    const n = (v, color) => (num(v) ? pill(num(v), color) : '0');
    return `<table><tr><th>Namespace</th><th>Pods</th><th>Running</th><th>Pending</th><th>Failed</th><th>Crashloop</th><th>Not ready</th><th>Deployments</th><th>Services</th><th>Jobs</th></tr>${data.map((ns) => `<tr class="clickable" data-action="ns" data-arg="${esc(ns.name)}">
      <td><strong>${esc(ns.name)}</strong></td><td>${num(ns.pods.total)}</td><td>${n(ns.pods.running, 'green')}</td><td>${n(ns.pods.pending, 'yellow')}</td>
      <td>${n(ns.pods.failed, 'red')}</td><td>${n(ns.pods.crashLoop, 'red')}</td><td>${n(ns.pods.notReady, 'yellow')}</td>
      <td>${num(ns.deployments)}</td><td>${num(ns.services)}</td><td>${num(ns.jobs)}</td></tr>`).join('')}</table>`;
  }
  async function viewNamespace(ns) {
    const d = await api('/api/namespace/' + encodeURIComponent(ns));
    const status = (p) => (['CrashLoopBackOff', 'Error', 'OOMKilled'].includes(p.status) || p.phase === 'Failed' ? 'red'
      : (p.status === 'Pending' || String(p.status).startsWith('Init:') || p.status === 'ContainerCreating') ? 'yellow' : p.status === 'Running' ? 'green' : 'muted');
    let h = `<button type="button" class="link back" data-action="ns-back">&#8592; all namespaces</button><h3>Namespace ${esc(ns)}</h3>`;
    h += `<h4>Pods (${(d.pods || []).length})</h4><table><tr><th>Name</th><th>Ready</th><th>Status</th><th>Restarts</th><th>Node</th><th>IP</th><th>Age</th></tr>${(d.pods || []).map((p) => `<tr>
      <td class="mono">${esc(p.name)}</td><td>${esc(p.ready)}</td><td>${pill(p.status, status(p))}</td><td>${num(p.restarts)}</td>
      <td class="muted">${esc(p.node)}</td><td class="mono muted">${esc(p.ip)}</td><td>${esc(p.age)}</td></tr>`).join('')}</table>`;
    if ((d.deployments || []).length) h += `<h4>Deployments</h4><table><tr><th>Name</th><th>Ready</th><th>Up to date</th><th>Available</th><th>Age</th></tr>${d.deployments.map((x) => `<tr>
      <td><strong>${esc(x.name)}</strong></td><td>${esc(x.ready)}</td><td>${num(x.upToDate)}</td><td>${num(x.available)}</td><td>${esc(x.age)}</td></tr>`).join('')}</table>`;
    if ((d.services || []).length) h += `<h4>Services</h4><table><tr><th>Name</th><th>Type</th><th>Cluster IP</th><th>Ports</th><th>Age</th></tr>${d.services.map((x) => `<tr>
      <td><strong>${esc(x.name)}</strong></td><td>${pill(x.type, 'blue')}</td><td class="mono">${esc(x.clusterIP)}</td><td>${esc(x.ports)}</td><td>${esc(x.age)}</td></tr>`).join('')}</table>`;
    return h;
  }
  async function viewNodes() {
    const nodes = await api('/api/nodes');
    if (!nodes.length) return empty('No nodes');
    return nodes.map((n) => {
      const flags = [n.status, n.unschedulable && 'cordoned', n.memoryPressure && 'memory pressure', n.diskPressure && 'disk pressure'].filter(Boolean).join(', ');
      const color = n.status !== 'Ready' || n.memoryPressure || n.diskPressure ? 'red' : n.unschedulable ? 'yellow' : 'green';
      return `<div class="node-card"><div><div class="nn">${esc(n.name)}</div><div class="muted">${esc(n.roles)} &middot; ${esc(n.version)}</div><div class="muted">${esc(n.os)}</div></div>
        <div>${pill(flags, color)}<div class="muted">Pods ${num(n.pods)} &middot; age ${esc(n.age)}</div></div>
        <div>CPU <strong>${esc(n.cpu)}</strong><br>Memory <strong>${esc(n.memory)}</strong></div></div>`;
    }).join('');
  }
  async function viewCost() {
    const d = await api('/api/cost');
    const c = d.config || {};
    const pct = num(d.totalMonthly) > 0 ? (num(d.workloadCost) / num(d.totalMonthly) * 100).toFixed(0) : '0';
    let h = `<p class="muted small">Source ${pill(c.source || 'default', 'blue')} &middot; ${esc(c.currency || 'USD')} &middot; ${money(c.cpuPerHour, 3)} per vCPU hour &middot; ${money(c.memPerGiBHour, 4)} per GiB hour</p>
      <div class="mini-row"><div class="stat-box static"><span class="label">Total monthly</span><span class="val green">${money(d.totalMonthly)}</span></div>
      <div class="stat-box static"><span class="label">Workloads</span><span class="val accent">${money(d.workloadCost)}</span></div>
      <div class="stat-box static"><span class="label">Unused</span><span class="val red">${money(d.wastedCost)}</span></div>
      <div class="stat-box static"><span class="label">Efficiency</span><span class="val yellow">${pct}%</span></div></div>`;
    const usage = (v) => pill(num(v) + '%', num(v) > 80 ? 'red' : num(v) > 50 ? 'yellow' : 'green');
    if ((d.nodes || []).length) h += `<h4>Nodes</h4><table><tr><th>Node</th><th>Instance</th><th>CPU</th><th>Memory</th><th>Pods</th><th>CPU used</th><th>Memory used</th><th>Per hour</th><th>Per month</th></tr>${d.nodes.map((n) => `<tr>
      <td><strong>${esc(n.name)}</strong></td><td class="mono">${esc(n.instanceType || '-')}</td><td>${num(n.cpuCores)}</td><td>${num(n.memoryGiB)}Gi</td><td>${num(n.pods)}</td>
      <td>${usage(n.cpuUsedPct)}</td><td>${usage(n.memUsedPct)}</td><td class="mono">${money(n.hourlyRate, 4)}</td><td><strong>${money(n.monthly)}</strong></td></tr>`).join('')}</table>`;
    if ((d.topWorkloads || []).length) h += `<h4>Top workloads</h4><table><tr><th>Namespace</th><th>Name</th><th>Kind</th><th>Replicas</th><th>CPU</th><th>Memory</th><th>Per month</th></tr>${d.topWorkloads.map((w) => `<tr>
      <td><span class="ns">${esc(w.namespace)}</span></td><td><strong>${esc(w.name)}</strong></td><td>${pill(w.kind, 'blue')}</td><td>${num(w.replicas)}</td>
      <td>${esc(w.cpuRequests)}</td><td>${num(w.memRequestsMiB)}Mi</td><td><strong>${money(w.monthly, 2)}</strong></td></tr>`).join('')}</table>`;
    return h;
  }
  async function viewResources() {
    if (state.detail) {
      const d = await api('/api/resources/' + encodeURIComponent(state.detail));
      const s = d.summary || {};
      return `<button type="button" class="link back" data-action="ns-back">&#8592; all namespaces</button><h3>${esc(state.detail)} <span class="muted small">${num(s.pods)} pods &middot; ${money(s.monthly)} per month</span></h3>
        <p>${pill('right-sized ' + num(s.healthy), 'green')} ${pill('overuse ' + num(s.overuse), 'red')} ${pill('underuse ' + num(s.underuse), 'yellow')} ${pill('no limits ' + num(s.noLimits), 'red')}</p>
        <table><tr><th>Pod</th><th>Status</th><th>Efficiency</th><th>CPU request</th><th>CPU limit</th><th>Memory request</th><th>Memory limit</th><th>Per month</th><th>Advice</th></tr>${(d.pods || []).map((p) => `<tr>
        <td class="mono">${esc(trunc(p.name, 40))}</td><td>${pill(p.status, p.status === 'Running' ? 'green' : 'red')}</td><td>${pill(p.efficiency, oneOf(p.effColor, ['green', 'red', 'yellow'], 'green'))}</td>
        <td>${esc(p.cpuRequest)}</td><td>${esc(p.cpuLimit)}</td><td>${esc(p.memRequest)}</td><td>${esc(p.memLimit)}</td><td>${money(p.monthly, 2)}</td><td class="small">${esc(p.advice)}</td></tr>`).join('')}</table>`;
    }
    const data = (await api('/api/resources')).filter((n) => num(n.pods) > 0).sort((a, b) => num(b.monthly) - num(a.monthly));
    const n = (v, color) => (num(v) ? pill(num(v), color) : '0');
    return data.length ? `<table><tr><th>Namespace</th><th>Pods</th><th>CPU requests</th><th>CPU limits</th><th>Memory requests</th><th>Memory limits</th><th>Right</th><th>Over</th><th>Under</th><th>No limits</th><th>Per month</th></tr>${data.map((x) => `<tr class="clickable" data-action="ns" data-arg="${esc(x.name)}">
      <td><strong>${esc(x.name)}</strong></td><td>${num(x.pods)}</td><td>${esc(x.cpuRequests)}</td><td>${esc(x.cpuLimits)}</td><td>${num(x.memRequestsMi)}Mi</td><td>${num(x.memLimitsMi)}Mi</td>
      <td>${n(x.healthy, 'green')}</td><td>${n(x.overuse, 'red')}</td><td>${n(x.underuse, 'yellow')}</td><td>${n(x.noLimits, 'red')}</td><td><strong>${money(x.monthly)}</strong></td></tr>`).join('')}</table>` : empty('No pods');
  }

  const VIEWS = { events: viewEvents, audit: () => viewAudit(false), dryrun: () => viewAudit(true), actions: viewFixes,
    compliance: viewCompliance, deploys: viewDeploys, baselines: viewBaselines, k8sevents: viewK8sEvents, charts: viewCharts,
    report: viewReport, cluster: viewCluster, nodes: viewNodes, cost: viewCost, resources: viewResources };

  // ---------- header, render loop ----------
  async function header() {
    const [s, st] = await Promise.all([api('/api/status'), api('/api/stats')]);
    $('version').textContent = 'v' + (s.version || '');
    $('node-name').textContent = s.nodeName || '';
    const mode = oneOf(s.mode, ['fix', 'suggest', 'observe', 'dry-run'], 'observe');
    $('mode-badge').textContent = s.mode || '';
    $('mode-badge').className = 'badge badge-' + mode;
    $('leader-badge').textContent = s.isLeader ? 'leader' : 'standby';
    $('leader-badge').className = 'badge ' + (s.isLeader ? 'badge-leader' : 'badge-follower');
    if (s.startedAt && !state.startedAt) state.startedAt = new Date(s.startedAt).getTime();
    const t = st.byType || {};
    $('stat-total').textContent = num(st.total);
    for (const k of ['incident', 'action', 'audit']) $('stat-' + k).textContent = num(t[k]);
  }
  async function render() {
    if (state.busy || state.tab === 'terminal') return;
    state.busy = true;
    const view = $('view');
    const scroller = view.querySelector('.list');
    const top = scroller ? scroller.scrollTop : 0;
    try {
      await header();
      view.innerHTML = await VIEWS[state.tab]();
      const again = view.querySelector('.list');
      if (again) again.scrollTop = top;
    } catch (e) {
      if (!(e instanceof AuthError)) view.innerHTML = empty('Could not load this view: ' + e.message);
    } finally {
      state.busy = false;
      hooks.renders++;
    }
  }
  function schedule() {
    clearTimeout(state.timer);
    if (state.refreshMs > 0) state.timer = setTimeout(async () => { if (!document.hidden) await render(); schedule(); }, state.refreshMs);
  }

  // ---------- navigation ----------
  function selectTab(tab, push = true) {
    if (!VIEWS[tab] && tab !== 'terminal') tab = 'events';
    state.tab = tab;
    state.detail = null;
    document.querySelectorAll('[role=tab]').forEach((b) => {
      const on = b.dataset.tab === tab;
      b.setAttribute('aria-selected', on ? 'true' : 'false');
      b.tabIndex = on ? 0 : -1;
    });
    $('view').hidden = tab === 'terminal';
    $('terminal').hidden = tab !== 'terminal';
    if (push && location.hash !== '#' + tab) history.replaceState(null, '', '#' + tab);
    showToolbar();
    setCount(0, 0);
    if (tab === 'terminal') $('term-input').focus(); else render();
  }
  function markTypeBoxes() {
    document.querySelectorAll('.stat-row [data-action="filter-type"]').forEach((b) => b.classList.toggle('active', b.dataset.arg === state.type));
  }
  const ACTIONS = {
    'filter-type': (el) => { state.type = el.dataset.arg; markTypeBoxes(); selectTab('events'); },
    workload: (el) => { state.ns = el.dataset.ns || ''; state.q = el.dataset.wl || ''; $('f-ns').value = state.ns; $('f-search').value = state.q; selectTab('events'); },
    reason: (el) => { state.type = 'incident'; state.q = el.dataset.arg; $('f-search').value = state.q; markTypeBoxes(); selectTab('events'); },
    ns: (el) => { state.detail = el.dataset.arg; render(); },
    'ns-back': () => { state.detail = null; render(); },
    fixes: (el) => { state.fixes = el.dataset.arg; render(); },
    days: (el) => { state.days = num(el.dataset.arg) || 30; render(); },
    'refresh-now': () => render(),
    'sign-out': () => { store(sessionStorage, TOKEN_KEY); showLogin('Signed out.'); },
  };
  document.addEventListener('click', (ev) => {
    const tab = ev.target.closest('[role=tab]');
    if (tab) { selectTab(tab.dataset.tab); return; }
    const el = ev.target.closest('[data-action]');
    if (el && ACTIONS[el.dataset.action]) ACTIONS[el.dataset.action](el);
  });
  document.querySelector('[role=tablist]').addEventListener('keydown', (ev) => {
    if (ev.key !== 'ArrowRight' && ev.key !== 'ArrowLeft') return;
    const tabs = [...document.querySelectorAll('[role=tab]')];
    const i = tabs.findIndex((t) => t.dataset.tab === state.tab);
    const next = tabs[(i + (ev.key === 'ArrowRight' ? 1 : tabs.length - 1)) % tabs.length];
    next.focus();
    selectTab(next.dataset.tab);
  });
  for (const [id, key] of [['f-ns', 'ns'], ['f-sev', 'sev'], ['f-result', 'result']]) {
    $(id).addEventListener('change', (ev) => { state[key] = ev.target.value; render(); });
  }
  let searchTimer = null;
  $('f-search').addEventListener('input', (ev) => {
    clearTimeout(searchTimer);
    searchTimer = setTimeout(() => { state.q = ev.target.value.trim(); render(); }, 250);
  });
  $('refresh-rate').addEventListener('change', (ev) => {
    state.refreshMs = num(ev.target.value);
    store(localStorage, RATE_KEY, String(state.refreshMs));
    schedule();
  });
  $('login-form').addEventListener('submit', (ev) => {
    ev.preventDefault();
    const t = $('token-input').value.trim();
    if (!t) return;
    store(sessionStorage, TOKEN_KEY, t);
    $('token-input').value = '';
    $('login').hidden = true;
    render();
  });
  window.addEventListener('hashchange', () => selectTab(location.hash.slice(1), false));

  // ---------- terminal ----------
  const history_ = [];
  let histIdx = -1;
  function termPrint(text, cls) {
    const out = $('term-output');
    const line = document.createElement('div');
    if (cls) line.className = cls;
    line.textContent = text;
    out.appendChild(line);
    out.scrollTop = out.scrollHeight;
  }
  async function termExec(cmd) {
    if (!cmd) return;
    history_.unshift(cmd);
    histIdx = -1;
    termPrint('$ kubectl ' + cmd, 'cmd');
    try {
      const d = await api('/api/kubectl', { method: 'POST', headers: { 'Content-Type': 'application/json' }, body: JSON.stringify({ command: cmd }) });
      if (d.error) termPrint('Error: ' + d.error, 'err');
      if (d.output) termPrint(d.output);
    } catch (e) {
      if (!(e instanceof AuthError)) termPrint('Failed: ' + e.message, 'err');
    }
  }
  $('term-input').addEventListener('keydown', (ev) => {
    const inp = ev.target;
    if (ev.key === 'Enter') { const c = inp.value.trim(); inp.value = ''; termExec(c); } else if (ev.key === 'ArrowUp') {
      ev.preventDefault();
      if (histIdx < history_.length - 1) inp.value = history_[++histIdx];
    } else if (ev.key === 'ArrowDown') {
      ev.preventDefault();
      if (histIdx > 0) inp.value = history_[--histIdx]; else { histIdx = -1; inp.value = ''; }
    }
  });
  termPrint('auto-agent terminal. Type help for commands.', 'hint');

  // ---------- start ----------
  setInterval(() => {
    if (!state.startedAt) return;
    const d = Math.floor((Date.now() - state.startedAt) / 1000);
    $('stat-uptime').textContent = [Math.floor(d / 3600), Math.floor((d % 3600) / 60), d % 60].map((x) => String(x).padStart(2, '0')).join(':');
  }, 1000);
  const savedRate = load(localStorage, RATE_KEY);
  if (savedRate !== '') { state.refreshMs = num(savedRate); $('refresh-rate').value = String(state.refreshMs); }
  window.__autoAgent = hooks; // read by scripts/ui-test.mjs
  if (!load(sessionStorage, TOKEN_KEY)) showLogin();
  selectTab(location.hash.slice(1) || 'events', false);
  schedule();
})();
