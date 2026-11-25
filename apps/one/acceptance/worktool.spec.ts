// Acceptance tests for apps/one/SPEC.md point 32: Work tool's own read token and the one ready private list.
//
// Names used here (to be checked against API.md when the builder's version arrives): MCP tool `private_list` and
// `GET /api/private-list`, answering `private`, `made_at`, `note` and `entries` (each `kind`, `title`, `who`, `why`, `date`,
// `url`).
//
// The Work tool token on a preview may be the owner's real one. These tests never press `New token` or `Revoke` when a token
// already exists there: they read it (reading changes nothing) and the tests that need to make or revoke one skip with that
// reason. Where there is no token, a test makes one, uses it and revokes it again in its `finally`, so the section is left
// as it was found.
//
// Not tested, and why:
//  - the agenda part of the list (today and the next 7 days): a test run cannot create a calendar entry.
//  - API.md ("Work tool") says the list holds nothing from test runs. SPEC 32 says an open to-do and a waiting proposal of a
//    run appear; the two tests for them follow the SPEC and fail until the app and the SPEC agree.
//  - Work tool showing the list apart from work and the one-click show or hide: that part is Work tool's, not ONE's.
import { expect, type Page } from '@playwright/test';
import {
  currentRun, test, mkr, go, api, mcp, tool, token, visit, connection, chat, readyBare, newChat, postChats, putFile, findItem, press, act, rowBox, showRow,
  type Chat,
} from './one-helpers';

const SECTION = 'Work tool token';

async function box(page: Page) {
  await visit(page, 'connectors');
  const region = await connection(page, SECTION);
  return { region, field: region.getByLabel(SECTION, { exact: true }) };
}
async function currentValue(page: Page): Promise<string> {
  const { field } = await box(page);
  if (!(await field.count())) return '';
  return ((await field.first().inputValue().catch(() => '')) ?? '').trim();
}
async function pressAndAccept(page: Page, name: 'New token' | 'Revoke') {
  const { region } = await box(page);
  const accept = (d: import('@playwright/test').Dialog) => { d.accept().catch(() => {}); }; // the page may ask once more
  page.on('dialog', accept);
  try { await press(page, region.getByRole('button', { name, exact: true })); } finally { page.off('dialog', accept); }
}

/** Runs `fn` with a Work tool token. `owned` is true when this test made the token (and then revokes it afterwards);
 *  false when one existed already and is only read. */
async function withWorkTool(page: Page, fn: (t: string, owned: boolean) => Promise<void>) {
  const before = await currentValue(page);
  if (before.length > 10) { await fn(before, false); return; }
  await pressAndAccept(page, 'New token');
  let t = '';
  await expect.poll(async () => (t = await currentValue(page)).length, { message: 'New token shows the new token' }).toBeGreaterThan(10);
  try { await fn(t, true); } finally {
    if ((await currentValue(page)).length > 10) await pressAndAccept(page, 'Revoke').catch(() => {});
  }
}
// without `run` the list never holds a record of a test run; with it, only that run's (API.md, Work tool)
const listOf = (page: Page, t: string, run?: string) =>
  api(page, 'GET', '/api/private-list' + (run ? `?run=${encodeURIComponent(run)}` : ''), undefined, t, null);
// API.md: every other address answers the Work tool token exactly as a wrong token: 401 {"ok": false, "error": "wrong token"}
const refused = (r: { status: number; json: any }) => r.status === 401 && r.json?.ok === false && r.json?.error === 'wrong token';
const inOne = (u: string) => { const x = new URL(u); return x.pathname + x.search; }; // the address is BASE_URL plus a path

test('32: Connections has the section Work tool token with Copy, New token and Revoke', async ({ page }) => {
  const { region } = await box(page);
  await expect(region).toBeVisible();
  for (const b of ['Copy', 'New token', 'Revoke']) await expect(region.getByRole('button', { name: b, exact: true }), b).toBeVisible();
});

test('32: the Work tool token is its own token, not the agents\' and not the bridge\'s', async ({ page }) => {
  await withWorkTool(page, async (t) => {
    expect(t).not.toBe(await token(page, 'Agent'));
    expect(t).not.toBe(await token(page, 'Bridge'));
  });
});

test('32: with the Work tool token the private list is read, marked private, and says when it was made', async ({ page }) => {
  await withWorkTool(page, async (t) => {
    const r = await listOf(page, t);
    expect(r.status, r.text.slice(0, 300)).toBe(200);
    expect(r.json.private, 'the list is marked private').toBe(true);
    expect(Number.isNaN(Date.parse(r.json.made_at)), 'it says when it was made').toBe(false);
    expect(Math.abs(Date.now() - Date.parse(r.json.made_at)), 'and it is current').toBeLessThan(60 * 60_000);
    expect(typeof r.json.note).toBe('string');
    expect(r.json.note, 'the note says it is private').toMatch(/private/i);
    expect(Array.isArray(r.json.entries)).toBe(true);
    for (const e of r.json.entries as any[]) {
      for (const k of ['kind', 'title', 'who', 'why', 'date', 'url']) expect(k in e, `an entry has ${k}`).toBe(true);
      for (const k of Object.keys(e)) expect(k, 'no message text and no files in an entry').not.toMatch(/text|body|file|attach/i);
    }
    // the same list through the connector's tool
    const m = await tool(page, 'private_list', {}, t);
    expect(m.isError, m.text).toBe(false);
    expect(m.data?.private).toBe(true);
  });
});

test('32: with the Work tool token nothing else can be done: no write, no approval, no reading of messages, no search', async ({ page }) => {
  const c = await readyBare(page);
  await withWorkTool(page, async (t) => {
    const n = c.number;
    const before = JSON.stringify((await api(page, 'GET', `/api/items/${n}/thread`)).json);
    // the API: writes, approvals, reads of messages and search
    const calls: [string, string, unknown?][] = [
      ['POST', `/api/items/${n}/comments`, { text: `work tool ${c.mk}` }],
      ['POST', `/api/items/${n}/proposals`, { action: 'pending', reason: `work tool ${c.mk}` }],
      ['POST', `/api/items/${n}/skip`, { reason: `work tool ${c.mk}`, kind: 'noise' }],
      ['POST', `/api/items/${n}/seen`, {}],
      ['POST', `/api/items/${n}/approve`, { approved: true }],
      ['POST', `/api/proposals/1/approve`, {}],
      ['POST', `/api/items/${n}/reply-proposals`, { text: `work tool ${c.mk}`, reason: 'x' }],
      ['POST', `/api/setting-proposals`, { kind: 'setting', name: 'Review window', value: '1 month', reason: 'x' }],
      ['POST', `/api/refresh`, {}],
      ['GET', `/api/agenda`],
      ['GET', `/api/people`],
      ['GET', `/api/log`],
      ['DELETE', `/api/test-runs/e2e-${c.mk.replace(/[^a-z0-9]/g, '').slice(0, 20).padEnd(8, 'x')}`],
      ['POST', `/api/notes`, { title: 'x', text: `work tool ${c.mk}` }],
      ['POST', `/api/agenda/todos`, { item: n, text: `work tool ${c.mk}`, reason: 'x' }],
      ['GET', `/api/items/${n}`],
      ['GET', `/api/items/${n}/thread`],
      ['GET', `/api/search?q=${encodeURIComponent(c.mk)}`],
      ['GET', `/api/queue?limit=5`],
    ];
    for (const [method, path, body] of calls) {
      const r = await api(page, method as 'GET' | 'POST', path, body, t);
      expect(refused(r), `${method} ${path} with the Work tool token is refused (got ${r.status})`).toBe(true);
      expect(r.text, `${method} ${path} gave nothing of the item`).not.toContain(c.mk);
    }
    // the connector's tools
    for (const [name, args] of [
      ['add_comment', { id: n, text: `work tool ${c.mk}` }], ['propose_change', { id: n, action: 'pending', reason: `work tool ${c.mk}` }],
      ['resolve_item', { id: n, reason: `work tool ${c.mk}`, kind: 'noise' }], ['mark_seen', { id: n }],
      ['get_item', { id: n }], ['search_items', { query: c.mk }], ['list_review_queue', {}], ['list_agenda', {}],
      ['save_note', { title: 'x', text: `work tool ${c.mk}` }], ['propose_setting', { kind: 'setting', name: 'Chat quiet time', value: '1 hour', reason: 'x' }],
    ] as [string, Record<string, unknown>][]) {
      const r = await mcp(page, 'tools/call', { name, arguments: args }, t);
      // a tool error with the same words as a wrong token
      expect(r.json?.result?.isError, `${name} with the Work tool token is a tool error: ${r.text.slice(0, 200)}`).toBe(true);
      expect(r.text, `${name} says wrong token`).toContain('wrong token');
      expect(r.text, `${name} gave nothing of the item`).not.toContain(c.mk);
    }
    // and nothing was written
    expect(JSON.stringify((await api(page, 'GET', `/api/items/${n}/thread`)).json)).toBe(before);
  });
});

test('32: New token makes the old token useless, and Revoke makes the token useless', async ({ page }) => {
  const before = await currentValue(page);
  test.skip(before.length > 10, 'a Work tool token exists here: it is not replaced or revoked by a test');
  await pressAndAccept(page, 'New token');
  let a = '';
  await expect.poll(async () => (a = await currentValue(page)).length).toBeGreaterThan(10);
  try {
    expect((await listOf(page, a)).status).toBe(200);
    await pressAndAccept(page, 'New token');
    let b = '';
    await expect.poll(async () => (b = await currentValue(page)), { message: 'a new token is shown' }).not.toBe(a);
    expect(b.length).toBeGreaterThan(10);
    const old = await listOf(page, a);
    expect(refused(old), 'the old token no longer works').toBe(true);
    expect((await listOf(page, b)).status, 'the new one does').toBe(200);
    await pressAndAccept(page, 'Revoke');
    await expect.poll(async () => (await listOf(page, b)).status, { message: 'a revoked token no longer works' }).toBeGreaterThanOrEqual(400);
    expect(refused(await listOf(page, a))).toBe(true);
  } finally {
    if ((await currentValue(page)).length > 10) await pressAndAccept(page, 'Revoke').catch(() => {});
  }
});

test('32: without a token (or with a wrong one) the private list is refused', async ({ page }) => {
  for (const who of ['none', 'not-a-token-' + mkr()]) {
    const r = await listOf(page, who);
    expect(refused(r), who).toBe(true);
  }
});

async function entryOf(page: Page, t: string, number: string, run: string | undefined = currentRun() ?? undefined): Promise<any | undefined> {
  const id = number.replace(/\D/g, '').replace(/^0+/, '');
  const re = new RegExp(`(?<!\\d)0*${id}(?!\\d)`);
  for (let i = 0; i < 8; i++) {
    const r = await listOf(page, t, run);
    const hit = (r.json?.entries ?? []).find((e: any) => re.test(String(e.url)) || String(e.title).includes(number));
    if (hit) return hit;
    await page.waitForTimeout(700);
  }
  return undefined;
}

test('32: a proposal of the run waiting for Approve is on the list, with its address in ONE, and no text of the message', async ({ page }) => {
  const mk = mkr();
  const secret = `bodysecret${mk}`, fileName = `fn-${mk}.txt`, agentFile = `agentfile-${mk}.txt`;
  const att = { id: 'f1', name: fileName, type: 'text/plain', size: 8 };
  const c = newChat({ mk, msgs: [{ text: `message ${secret}`, attachments: [att], minsAgo: 120 }] });
  await postChats(page, [c]);
  expect((await putFile(page, c, c.msgs[0], att, Buffer.from('12345678'))).status).toBe(200);
  c.number = await findItem(page, c);
  expect((await api(page, 'POST', `/api/items/${c.number}/seen`, {})).status).toBe(200);
  expect((await tool(page, 'add_file', { id: c.number, name: agentFile, text: 'agent words' })).isError).toBe(false);
  expect((await api(page, 'POST', `/api/items/${c.number}/proposals`, { action: 'pending', reason: `waiting ${mk}` })).status).toBe(200);
  await withWorkTool(page, async (t) => {
    const e = await entryOf(page, t, c.number);
    expect(e, 'the waiting proposal is on the list').toBeTruthy();
    for (const k of ['kind', 'title', 'who', 'why', 'date', 'url']) expect(String(e[k] ?? '').length, `entry ${k} is filled`).toBeGreaterThan(0);
    // its address opens the item in ONE
    await go(page, inOne(String(e.url)));
    await expect(page.getByText(c.number).first(), 'the address opens the item').toBeVisible();
    // no message text and no file name anywhere in the list
    const all = (await listOf(page, t, currentRun() ?? undefined)).text;
    for (const s of [secret, fileName, agentFile]) expect(all, `the list never carries ${s}`).not.toContain(s);
  });
});

test('32: an open to-do of the run is on the list with its address in ONE, and the address opens the item', async ({ page }) => {
  const c = await readyBare(page);
  const text = `Ring back ${c.mk}`;
  expect((await api(page, 'POST', '/api/agenda/todos', { item: c.number, text, reason: `e2e ${c.mk}` })).status).toBe(200);
  // The owner approves it on the item's row: only then it is a to-do
  await act(rowBox(await showRow(page, c)), 'Approve');
  await expect.poll(async () => JSON.stringify((await api(page, 'GET', '/api/agenda')).json.todos ?? [])).toContain(c.mk);
  await withWorkTool(page, async (t) => {
    const r = await listOf(page, t, currentRun() ?? undefined);
    const e = (r.json.entries as any[]).find((x) => JSON.stringify(x).includes(c.mk) || JSON.stringify(x).includes(text));
    const hit = e ?? (await entryOf(page, t, c.number));
    expect(hit, 'the open to-do is on the list').toBeTruthy();
    await go(page, inOne(String(hit.url)));
    await expect(page.getByText(c.number).first(), 'its address opens the item').toBeVisible();
    // the plain list never shows a record of a test run, and the tool takes `run` like the address
    const plain = await listOf(page, t);
    expect(plain.status).toBe(200);
    expect(plain.text, 'the plain list has no record of a test run').not.toContain(c.mk);
    const viaTool = await tool(page, 'private_list', { run: currentRun() }, t);
    expect(viaTool.isError, viaTool.text).toBe(false);
    expect(viaTool.text, 'with run the tool holds the run\'s to-do').toContain(c.mk);
    expect((await tool(page, 'private_list', {}, t)).text, 'without run it does not').not.toContain(c.mk);
    expect((await listOf(page, t, 'Bad Run!')).status, 'a bad run name is refused').toBe(400);
  });
});

test.fixme('32: the agenda for today and the next 7 days is on the list, and an entry beyond 7 days is not', async () => {
  // A test run cannot create a calendar entry (SPEC point 18 and API.md): nothing to put on the agenda.
});
void ({} as Chat);
