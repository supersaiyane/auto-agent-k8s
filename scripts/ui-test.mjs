// Drives headless Chrome over the DevTools protocol (no npm packages) against
// a running dashboard and fails on any console error, script exception,
// Content-Security-Policy violation, view that does not load, or injected
// markup that runs (PLAN-002 11.2, ISS-060).
//
// Usage: node scripts/ui-test.mjs <base-url> <dashboard-token>
import { spawn } from 'node:child_process';
import { existsSync, mkdtempSync, rmSync } from 'node:fs';
import { tmpdir } from 'node:os';
import { join } from 'node:path';

const [base, token] = process.argv.slice(2);
if (!base || !token) { console.error('usage: ui-test.mjs <base-url> <token>'); process.exit(2); }
const TABS = ['events', 'audit', 'dryrun', 'actions', 'compliance', 'deploys', 'baselines', 'k8sevents',
  'charts', 'report', 'cluster', 'nodes', 'cost', 'resources', 'terminal'];

const candidates = [process.env.CHROME, '/Applications/Google Chrome.app/Contents/MacOS/Google Chrome',
  '/usr/bin/google-chrome', '/usr/bin/google-chrome-stable', '/usr/bin/chromium', '/usr/bin/chromium-browser'].filter(Boolean);
const chrome = candidates.find((c) => existsSync(c));
if (!chrome) { console.error('ui-test: no Chrome found; set CHROME'); process.exit(2); }

const profile = mkdtempSync(join(tmpdir(), 'ui-test-'));
const proc = spawn(chrome, ['--headless=new', '--disable-gpu', '--no-sandbox', '--no-first-run',
  '--remote-debugging-port=0', `--user-data-dir=${profile}`, 'about:blank'], { stdio: ['ignore', 'ignore', 'pipe'] });
const failures = [];
const fail = (m) => failures.push(m);
const done = (code) => {
  proc.once('exit', () => {
    try { rmSync(profile, { recursive: true, force: true }); } catch (e) { /* best effort cleanup of a temp profile */ }
    process.exit(code);
  });
  proc.kill();
};
setTimeout(() => { console.error('ui-test: timed out'); done(1); }, 90000);

const wsURL = await new Promise((resolve, reject) => {
  let buf = '';
  proc.stderr.on('data', (d) => {
    buf += d;
    const m = buf.match(/DevTools listening on (ws:\/\/\S+)/);
    if (m) resolve(m[1]);
  });
  proc.on('exit', () => reject(new Error('chrome exited: ' + buf)));
});

const ws = new WebSocket(wsURL);
await new Promise((r) => ws.addEventListener('open', r, { once: true }));
let nextId = 0;
const pending = new Map();
let session = '';
ws.addEventListener('message', (ev) => {
  const msg = JSON.parse(ev.data);
  if (msg.id && pending.has(msg.id)) {
    const { resolve, reject } = pending.get(msg.id);
    pending.delete(msg.id);
    msg.error ? reject(new Error(JSON.stringify(msg.error))) : resolve(msg.result);
    return;
  }
  if (msg.method === 'Runtime.exceptionThrown') fail('exception: ' + JSON.stringify(msg.params.exceptionDetails.exception?.description || msg.params.exceptionDetails.text));
  if (msg.method === 'Runtime.consoleAPICalled' && msg.params.type === 'error') fail('console error: ' + JSON.stringify(msg.params.args.map((a) => a.value ?? a.description)));
  if (msg.method === 'Log.entryAdded' && msg.params.entry.level === 'error') fail('browser log: ' + msg.params.entry.text);
});
const send = (method, params = {}) => new Promise((resolve, reject) => {
  const id = ++nextId;
  pending.set(id, { resolve, reject });
  ws.send(JSON.stringify({ id, method, params, ...(session ? { sessionId: session } : {}) }));
});
const evaluate = async (expr) => (await send('Runtime.evaluate', { expression: expr, awaitPromise: true, returnByValue: true })).result.value;
const sleep = (ms) => new Promise((r) => setTimeout(r, ms));
async function waitFor(expr, ms = 5000) {
  for (const end = Date.now() + ms; Date.now() < end; await sleep(100)) if (await evaluate(expr)) return true;
  return false;
}

const { targetId } = await send('Target.createTarget', { url: 'about:blank' });
({ sessionId: session } = await send('Target.attachToTarget', { targetId, flatten: true }));
await send('Runtime.enable');
await send('Log.enable');
await send('Page.enable');

// Without a token the sign-in form shows and nothing else is asked for.
await send('Page.navigate', { url: base + '/#events' });
if (!(await waitFor(`document.getElementById('login') && !document.getElementById('login').hidden`))) fail('sign-in form not shown without a token');

await evaluate(`sessionStorage.setItem('autoAgentToken', ${JSON.stringify(token)})`);
await send('Page.navigate', { url: base + '/#events' });
await waitFor(`window.__autoAgent && window.__autoAgent.renders > 0`);

for (const tab of TABS) {
  const before = await evaluate(`window.__autoAgent.renders`);
  await evaluate(`document.querySelector('[role=tab][data-tab="${tab}"]').click()`);
  if (tab === 'terminal') {
    if (!(await waitFor(`!document.getElementById('terminal').hidden`))) fail('terminal did not open');
    continue;
  }
  if (!(await waitFor(`window.__autoAgent.renders > ${before}`))) { fail(`${tab}: never rendered`); continue; }
  const text = await evaluate(`document.getElementById('view').innerText`);
  if (/Could not load/.test(text)) fail(`${tab}: ${text.slice(0, 200)}`);
  const selected = await evaluate(`document.querySelector('[role=tab][aria-selected=true]').dataset.tab`);
  if (selected !== tab) fail(`${tab}: tab not marked selected (${selected})`);
}

// A deep link selects its tab.
await evaluate(`location.hash = '#compliance'`);
if (!(await waitFor(`document.querySelector('[role=tab][aria-selected=true]').dataset.tab === 'compliance'`))) fail('deep link #compliance did not select its tab');

// Hostile values from the API stay text: nothing injected runs or renders.
await evaluate(`document.querySelector('[role=tab][data-tab="events"]').click()`);
await sleep(800);
if (await evaluate(`window.__pwned !== undefined`)) fail('injected script ran (window.__pwned set)');
if (await evaluate(`document.querySelector('#view img') !== null`)) fail('injected <img> was rendered as markup');
if (!(await evaluate(`document.getElementById('view').innerText.includes('<img src=x')`))) fail('hostile reason not shown as text');
if (await evaluate(`[...document.querySelectorAll('#view a')].some((a) => a.href.startsWith('javascript:'))`)) fail('javascript: link rendered');

if (failures.length) {
  console.error('ui-test: FAIL\n  ' + failures.join('\n  '));
  done(1);
}
console.log(`ui-test: PASS (${TABS.length} tabs, no console error, no CSP violation, hostile values inert)`);
done(0);
