// Shared helpers for the ONE acceptance tests (not a spec file).
//
// Every test makes its own data and only touches rows that carry its own random
// marker. Items come in through the chat bridge's open address (apps/one/API.md,
// "The chat bridge"), exactly as the real bridge posts them, with the bridge
// token read from the Connectors page at run time. Agents' calls use the agents'
// token, read the same way. Tokens are never printed or stored in a file.
//
// Nothing here presses Approve on mail, touches a real connection, presses
// `New token` or posts to /api/bridge/outbox (that would hand out real replies).
import { test as base, expect, type Locator, type Page } from '@playwright/test';
import * as zlib from 'zlib';

export const mkr = (): string => 'e2e' + Date.now().toString(36) + Math.random().toString(36).slice(2, 7);
export const isPhone = (page: Page): boolean => (page.viewportSize()?.width ?? 1280) < 700;
export const iso = (minsAgo = 0): string => new Date(Date.now() - minsAgo * 60_000).toISOString();
export const days = (n: number): number => n * 24 * 60;

/** Q-0012 from 12, "12" or "Q-0012". */
export function qn(x: unknown): string {
  const s = String(x);
  if (/^Q-\d+$/.test(s)) return s;
  const d = s.replace(/\D/g, '');
  return 'Q-' + d.padStart(4, '0');
}

/** page.goto that survives a navigation still in flight. */
export async function go(page: Page, path: string) {
  for (let i = 0; i < 6; i++) {
    try { await page.goto(path); return; } catch (e) {
      if (i === 5 || !/ERR_ABORTED|NS_BINDING_ABORTED|WebKit encountered an internal error|interrupted by another navigation/.test(String(e))) throw e;
      await page.waitForLoadState('load').catch(() => {});
      await page.waitForTimeout(500);
    }
  }
}

// ---------------------------------------------------------------- tokens and API

// Every test names its own run (API.md, "Test runs"). The run name goes on everything the test creates through
// /api/, and when the test ends, whether it passed or failed, the run is removed and must be gone.
const runs = new Map<string, string>();
/** The run name of the test that is running now (null outside a test). */
export const currentRun = (): string | null => { try { return runs.get(base.info().testId) ?? null; } catch { return null; } };
export const newRun = (): string => 'e2e-' + Math.random().toString(36).slice(2, 8) + Date.now().toString(36);

export const test = base.extend<{ run: string }>({
  run: [async ({ page }, use, info) => {
    const name = newRun();
    runs.set(info.testId, name);
    try {
      await use(name);
    } finally {
      runs.delete(info.testId);
      const gone = await removeRun(page, name);
      expect(gone.status, 'removing the test run ' + name).toBe(200);
      const left = await api(page, 'GET', '/api/test-runs', undefined, 'agent', null);
      expect((left.json?.runs ?? []).map((r: any) => r.run), 'the run is gone').not.toContain(name);
    }
  }, { auto: true }],
});

export async function removeRun(page: Page, name: string) {
  const r = await api(page, 'DELETE', `/api/test-runs/${name}`, undefined, 'agent', null);
  return r;
}

const tokens: Record<string, string> = {};

/** Read the agents' or the bridge's token from the Connectors page. Never printed. */
export async function token(page: Page, which: 'Agent' | 'Bridge' = 'Agent'): Promise<string> {
  const base = String((test.info().project.use as { baseURL?: string }).baseURL ?? '');
  const key = base + '|' + which;
  if (tokens[key]) return tokens[key];
  const p = await page.context().newPage();
  try {
    await openConnectors(p);
    const name = which + ' token';
    // The connection rows are closed by default and open on a click; the token sits in a read-only box inside.
    const region = p.getByRole('region', { name });
    await region.locator('summary').first().click();
    const box = region.getByLabel(name);
    await expect(box).toBeVisible();
    const t = (await box.inputValue()).trim();
    expect(t.length).toBeGreaterThan(10);
    tokens[key] = t;
    return t;
  } finally {
    await p.close();
  }
}

export async function openConnectors(p: Page) {
  await p.goto('/connectors');
  const h = p.getByRole('heading', { name: 'Connectors', exact: true });
  const ok = await h.first().waitFor({ state: 'visible', timeout: 4000 }).then(() => true, () => false);
  if (!ok) {
    await p.goto('/');
    await p.getByRole('link', { name: /^Connectors:/ }).first().click();
  }
  await expect(p.getByRole('heading', { name: 'Connectors', exact: true }).first()).toBeVisible();
}

export type Who = 'agent' | 'bridge' | 'none' | string;

/** `run`: undefined = this test's own run (on calls that create or change something), null = no run name at all. */
export async function api(page: Page, method: 'GET' | 'POST' | 'PUT' | 'DELETE', path: string, body?: unknown, who: Who = 'agent',
  run?: string | null) {
  const headers: Record<string, string> = { 'Content-Type': 'application/json' };
  const r0 = run === undefined ? currentRun() : run;
  if (method !== 'GET' && r0) headers['X-Test-Run'] = r0;
  if (who === 'agent') headers.Authorization = `Bearer ${await token(page, 'Agent')}`;
  else if (who === 'bridge') headers.Authorization = `Bearer ${await token(page, 'Bridge')}`;
  else if (who !== 'none') headers.Authorization = `Bearer ${who}`;
  const res = await page.request.fetch(path, {
    method, headers, data: body === undefined ? undefined : JSON.stringify(body), failOnStatusCode: false,
  });
  const text = await res.text();
  let json: any = null;
  try { json = JSON.parse(text); } catch { /* not json */ }
  return { status: res.status(), json, text, headers: res.headers() };
}

export async function putFile(page: Page, c: Chat, m: Msg, a: Att, bytes: Buffer) {
  const q = `network=${encodeURIComponent(c.net)}&chat_id=${encodeURIComponent(c.chatId)}` +
    `&message_id=${encodeURIComponent(m.id)}&attachment=${encodeURIComponent(a.id)}`;
  const res = await page.request.fetch(`/api/bridge/files?${q}`, {
    method: 'PUT',
    headers: { Authorization: `Bearer ${await token(page, 'Bridge')}`, 'Content-Type': a.type, ...(currentRun() ? { 'X-Test-Run': currentRun()! } : {}) },
    data: bytes, failOnStatusCode: false,
  });
  let json: any = null;
  try { json = await res.json(); } catch { /* not json */ }
  return { status: res.status(), json };
}

// ---------------------------------------------------------------- chats posted like the bridge

export const NETS: Record<string, string> = {
  whatsapp: 'WhatsApp', messenger: 'Messenger', imessage: 'iMessage', sms: 'SMS',
};

export type Att = { id: string; name: string; type: string; size: number };
export type Msg = {
  id: string; text: string; at: string; own: boolean; editedAt?: string;
  reactions: { by: string; emoji: string }[]; attachments: Att[];
};
export type Chat = {
  mk: string; net: string; label: string; chatId: string; name: string; group: boolean; muted: boolean;
  senderId: string; senderName: string; selfId: string; msgs: Msg[]; id: string; number: string;
};
export type MsgOpts = Partial<{ text: string; minsAgo: number; own: boolean; id: string; attachments: Att[] }>;
export type ChatOpts = Partial<{
  net: string; group: boolean; muted: boolean; live: boolean; name: string; senderId: string; senderName: string;
  msgs: MsgOpts[]; text: string; minsAgo: number; mk: string;
}>;

/** A chat that is not posted yet. By default its one message is two hours old, so it is quiet (not live). */
export function newChat(o: ChatOpts = {}): Chat {
  const mk = o.mk ?? mkr();
  const net = o.net ?? 'whatsapp';
  const base = o.minsAgo ?? (o.live ? 1 : 120);
  const list: MsgOpts[] = o.msgs ?? [{ text: o.text ?? `Hello ${mk}` }];
  const c: Chat = {
    mk, net, label: NETS[net] ?? net, chatId: `chat-${mk}`, name: o.name ?? `Chat ${mk}`,
    group: !!o.group, muted: !!o.muted,
    senderId: o.senderId ?? `sender-${mk}`, senderName: o.senderName ?? `Sender ${mk}`, selfId: `self-${mk}`,
    msgs: [], id: '', number: '',
  };
  list.forEach((m, i) => {
    c.msgs.push({
      id: m.id ?? `${mk}-m${i + 1}`, text: m.text ?? `Message ${i + 1} ${mk}`,
      at: iso(m.minsAgo ?? Math.max(base - i, 0)), own: !!m.own, reactions: [], attachments: m.attachments ?? [],
    });
  });
  return c;
}

export function chatBody(cs: Chat[], again = false) {
  const run = currentRun();
  return {
    again,
    ...(run ? { run } : {}),
    chats: cs.map((c) => ({
      network: c.net, chat_id: c.chatId, name: c.name, type: c.group ? 'group' : 'direct', muted: c.muted,
      participants: [
        { handle: c.senderId, name: c.senderName, self: false },
        { handle: c.selfId, name: 'the owner', self: true },
      ],
      messages: c.msgs.map((m) => ({
        id: m.id, sender: m.own ? c.selfId : c.senderId, sender_name: m.own ? 'the owner' : c.senderName,
        at: m.at, text: m.text, own: m.own, read: false, reactions: m.reactions, attachments: m.attachments,
        ...(m.editedAt ? { edited_at: m.editedAt } : {}),
      })),
    })),
  };
}

export async function postChats(page: Page, cs: Chat[], again = false) {
  const r = await api(page, 'POST', '/api/bridge/batch', chatBody(cs, again), 'bridge');
  expect(r.status, r.text.slice(0, 300)).toBe(200);
  return r.json;
}

/** The item a posted chat became, found through full-text search on the marker. */
export async function findItem(page: Page, c: Chat, needle: string = c.mk): Promise<string> {
  for (let i = 0; i < 20; i++) {
    const r = await api(page, 'GET', `/api/search?q=${encodeURIComponent(needle)}&limit=50`);
    const hit = (r.json?.results ?? []).find((x: any) => x.item !== undefined && x.item !== null &&
      (x.kind === 'chat' || x.kind === undefined) && JSON.stringify(x).includes(c.mk));
    if (hit) return qn(hit.item);
    await page.waitForTimeout(400);
  }
  throw new Error('the posted chat did not become an item');
}

/** Post a chat; it exists as an item. */
export async function chat(page: Page, o: ChatOpts = {}): Promise<Chat> {
  const c = newChat(o);
  await postChats(page, [c]);
  c.number = await findItem(page, c);
  c.id = c.number;
  return c;
}

/** A chat that Claude has looked at: it stands in New, with nothing added to its thread. A phone does not list it
 *  (point 17); use `ready` where the test looks at the list on a phone. */
export async function readyBare(page: Page, o: ChatOpts = {}): Promise<Chat> {
  const c = await chat(page, o);
  const r = await api(page, 'POST', `/api/items/${c.number}/seen`, {});
  expect(r.status, r.text.slice(0, 300)).toBe(200);
  return c;
}

/** A chat that Claude has looked at: it stands in New. On a phone it also has a comment from Claude, so that the phone
 *  lists it (point 17); a laptop lists every chat and gets nothing added. */
export async function ready(page: Page, o: ChatOpts = {}): Promise<Chat> {
  const c = await readyBare(page, o);
  await ensureListed(page, c);
  return c;
}

/** Add one more message to an already posted chat (the whole chat is posted again; unchanged messages write nothing). */
export async function addMsg(page: Page, c: Chat, m: MsgOpts = {}) {
  const i = c.msgs.length + 1;
  c.msgs.push({
    id: m.id ?? `${c.mk}-m${i}`, text: m.text ?? `Message ${i} ${c.mk}`, at: iso(m.minsAgo ?? 60),
    own: !!m.own, reactions: [], attachments: m.attachments ?? [],
  });
  return postChats(page, [c]);
}

/** What the API says about an item. */
export async function itemOf(page: Page, c: Chat) {
  const r = await api(page, 'GET', `/api/items/${c.number}`);
  expect(r.status, r.text.slice(0, 300)).toBe(200);
  return r.json;
}
export async function stageOf(page: Page, c: Chat): Promise<string> {
  return String((await itemOf(page, c)).item?.stage ?? '');
}

/** The queue entries (what waits for Claude) for this item, all pages. */
export async function queueEntries(page: Page, c: Chat): Promise<any[]> {
  const out: any[] = [];
  let after: string | undefined;
  for (let i = 0; i < 40; i++) {
    const r = await api(page, 'GET', `/api/queue?limit=100${after ? `&after=${encodeURIComponent(after)}` : ''}`);
    expect(r.status, r.text.slice(0, 200)).toBe(200);
    for (const it of r.json.items ?? r.json.queue ?? []) if (qn(it.id) === c.number) out.push(it);
    if (!r.json.more) break;
    after = r.json.next_after;
  }
  return out;
}
export const waitsForClaude = async (page: Page, c: Chat): Promise<boolean> => (await queueEntries(page, c)).length > 0;

/** Agent skip (a clear skip) with a reason: the item goes to Closed. Only works on an item in New. */
export async function skip(page: Page, c: Chat, reason = 'e2e skip', kind: 'noise' | 'finished' | 'old' = 'finished') {
  return api(page, 'POST', `/api/items/${c.number}/skip`, { reason: `${reason} ${c.mk}`, kind });
}

// ---------------------------------------------------------------- the list

export const GROUPS = ['New', 'In progress', 'Pending', 'Resolved'] as const;

/** A laptop shows three panes (SPEC 1): the stages stand in the left pane, one chosen. A phone shows folded groups. */
export const isLaptop = (page: Page): boolean => !isPhone(page);

/** The left pane of a laptop. */
export const stagesPane = (page: Page): Locator => page.getByRole('navigation', { name: 'Stages' });
/** The ribbon on a laptop: what can be done with the chosen row. */
export const ribbon = (page: Page): Locator => page.getByRole('toolbar', { name: 'Selected item' });
/** The Connections block at the bottom of the left pane (laptop). */
export const connectionsPane = (page: Page): Locator => page.getByRole('region', { name: 'Connections', exact: true });
/** The middle pane's heading of the chosen stage (laptop). */
export const stageHeading = (page: Page): Locator => page.getByRole('main').getByRole('heading', { level: 2 }).first();

/** A stage in the left pane (laptop) or a group header (phone): its name starts with the stage's name and holds the count. */
export function groupHeader(page: Page, g: string): Locator {
  const name = new RegExp('^[^A-Za-z]*' + g + '\\b');
  if (isLaptop(page)) return stagesPane(page).getByRole('button', { name }).first();
  return page.getByRole('button', { name }).first();
}

async function isExpanded(h: Locator): Promise<boolean> {
  return h.evaluate((el) => el.getAttribute('aria-expanded') === 'true' || el.getAttribute('aria-pressed') === 'true' ||
    (el.closest('details') as HTMLDetailsElement | null)?.open === true).catch(() => false);
}

export async function expandGroup(page: Page, g: string) {
  const h = groupHeader(page, g);
  // a header that is late (a slow page) is not an absent one; an app with nothing in it has none
  await h.waitFor({ state: 'attached', timeout: 3000 }).catch(() => {});
  if (!(await h.count())) return false;
  if (!(await isExpanded(h))) await h.click();
  return true;
}

/** What a strip under the phone's top line says, matched by `re`: the strip itself where it is visible; where the top line
 *  is crowded and the strip is folded behind `+<n> more`, what that link says for the strips it hides (its name, which is
 *  also its hover text). Null when neither says it. Waits a moment for a strip that is drawn late. */
export async function stripValue(page: Page, re: RegExp): Promise<RegExpExecArray | null> {
  const strip = page.getByText(re).locator('visible=true').first();
  await strip.waitFor({ state: 'visible', timeout: 1500 }).catch(() => {});
  if (await strip.isVisible().catch(() => false)) {
    const m = re.exec(await strip.innerText().catch(() => ''));
    if (m) return m;
  }
  const more = page.getByRole('link', { name: /^\+\d+ more/ }).or(page.getByRole('button', { name: /^\+\d+ more/ }));
  for (const l of await more.all()) {
    const said = [await l.getAttribute('aria-label'), await l.getAttribute('title'), await l.innerText().catch(() => '')].join(' | ');
    const m = re.exec(said);
    if (m) return m;
  }
  return null;
}

/** How many items wait for Claude, as the page says it: the line `Claude` of Connections on a laptop (`<n> waiting`), the
 *  strip on a phone (`<n> waiting for Claude`, or, folded behind `+<n> more` on a crowded top line, what that link says).
 *  The page is opened fresh. */
export async function waitingCount(page: Page): Promise<number> {
  await go(page, '/');
  // wait until the page has been drawn (its footer says where it runs), then read until two reads a moment apart agree
  await page.getByText(/^Environment: /).first().waitFor({ state: 'visible', timeout: 10_000 }).catch(() => {});
  const read = async (): Promise<number> => {
    if (isLaptop(page)) {
      const link = connectionsPane(page).getByRole('link', { name: /^Claude/ });
      await link.waitFor({ state: 'visible', timeout: 5000 }).catch(() => {});
      const t = await link.innerText().catch(() => '');
      return Number(/(\d+) waiting/.exec(t)?.[1] ?? 0);
    }
    const m = await stripValue(page, /(\d+) waiting for Claude/);
    return Number(m?.[1] ?? 0);
  };
  const deadline = Date.now() + 10_000;
  let last = await read();
  for (;;) {
    await page.waitForTimeout(600);
    const now = await read();
    if (now === last || Date.now() > deadline) return now;
    last = now;
  }
}

/** The element that opens and closes a row: its top line, found by the item's number. */
export const toggleOf = (page: Page, c: Chat): Locator =>
  page.locator('[aria-expanded], summary').filter({ hasText: c.number }).first();

export const rowBox = (t: Locator): Locator =>
  t.locator('xpath=ancestor-or-self::*[self::li or self::article or self::details or self::section or @role="listitem" or @role="row" or @role="article"][1]');

export async function isOpen(t: Locator): Promise<boolean> {
  return t.evaluate((el) => el.getAttribute('aria-expanded') === 'true' ||
    (el.parentElement as HTMLDetailsElement | null)?.open === true).catch(() => false);
}

/** On a phone the agenda is a view of its own, and the page remembers it was open: close it so that the list shows. */
export async function settle(page: Page) {
  if (!isPhone(page)) return;
  const side = page.getByRole('complementary', { name: 'Agenda' }).or(page.getByRole('dialog', { name: 'Agenda' })).first();
  await page.getByText(/^Environment: /).first().waitFor({ state: 'visible', timeout: 5000 }).catch(() => {});
  if (await side.isVisible().catch(() => false)) {
    const close = side.getByRole('button', { name: 'Close', exact: true });
    if (await close.isVisible().catch(() => false)) await close.click();
    else await page.getByRole('button', { name: 'Agenda', exact: true }).first().click();
    await expect(side).toBeHidden();
  }
}

/** A stage shows a page of rows at a time: when the row is not there, press `Show more` (in the chosen stage on a laptop,
 *  in the open group on a phone) until it appears or there is no more. A test never expects its own row on the first
 *  page: the app may hold hundreds of items, and a chat dated days back lands behind many newer rows. */
export async function moreUntil(page: Page, t: Locator, wait = 1500): Promise<boolean> {
  if (await t.waitFor({ state: 'visible', timeout: wait }).then(() => true, () => false)) return true;
  const more = page.getByRole('button', { name: /^Show more\b/ }).locator('visible=true');
  for (let i = 0; i < 120; i++) {
    if (!(await more.first().isVisible().catch(() => false))) return false;
    await more.first().click().catch(() => {});
    if (await t.waitFor({ state: 'visible', timeout: 1200 }).then(() => true, () => false)) return true;
  }
  return false;
}

/** The row is on the page, looking past the first page of its stage if needed. */
export async function expectRow(page: Page, c: Chat) {
  await moreUntil(page, toggleOf(page, c));
  await expect(toggleOf(page, c)).toBeVisible();
}

const listedOnPhone = new Set<string>();
/** A phone lists a chat only when its thread holds something besides the chat's own messages (point 17): so that a test
 *  can find its chat in the phone's list, a comment from Claude is added to it once (a laptop lists every chat, and
 *  nothing is added there). Call it before looking for a chat in the list. */
export async function ensureListed(page: Page, c: Chat) {
  if (!isPhone(page) || listedOnPhone.has(c.mk)) return;
  const r = await api(page, 'POST', `/api/items/${c.number}/comments`, { text: `Claude looked at this chat ${c.mk}` });
  expect(r.status, r.text.slice(0, 200)).toBe(200);
  listedOnPhone.add(c.mk);
}

const STAGE_GROUP: Record<string, string> = { new: 'New', in_progress: 'In progress', pending: 'Pending', resolved: 'Resolved' };
const stageNow = async (page: Page, c: Chat): Promise<string> => {
  const r = await api(page, 'GET', `/api/items/${c.number}`);
  return r.status === 200 ? String(r.json?.item?.stage ?? '') : '';
};

/** Open the groups until this row shows. Returns the row's top line. On a busy app (thousands of rows, a slower server,
 *  the app's own model looking at a new item) the row is looked for where the app says the item is now, with a fresh
 *  load between tries as a user would do, paging with `Show more`, for up to 40 s. */
export async function showRow(page: Page, c: Chat): Promise<Locator> {
  await ensureListed(page, c);
  const t = toggleOf(page, c);
  const deadline = Date.now() + 40_000;
  let stage = '';
  for (let round = 0; ; round++) {
    stage = await stageNow(page, c);
    await go(page, '/');
    await settle(page);
    const order = STAGE_GROUP[stage] ? [STAGE_GROUP[stage], ...GROUPS.filter((g) => g !== STAGE_GROUP[stage])] : [...GROUPS];
    for (const g of order) {
      if (await t.isVisible().catch(() => false)) return t;
      if (!(await expandGroup(page, g))) continue;
      if (await moreUntil(page, t, round === 0 ? 1500 : 3000)) return t;
      if (g === order[0] && STAGE_GROUP[stage]) break; // the app says it is here: do not wander through the other groups
    }
    if (Date.now() > deadline) break;
    await page.waitForTimeout(1500);
  }
  if (stage === 'closed') throw new Error(`${c.number} is Closed (something closed it before the test acted: ${stage})`);
  await expect(t, `${c.number} is listed (the app says it is ${stage || 'unknown'})`).toBeVisible();
  return t;
}

/** Is this item in this group of the owner's list (fresh load, only that group opened)? Where the app itself says the item is
 *  in that stage, the row is waited for (a slow server, a long list); where it says otherwise, the answer is no at once. */
export async function groupHas(page: Page, c: Chat, g: string): Promise<boolean> {
  const here = STAGE_GROUP[await stageNow(page, c)] === g;
  const deadline = Date.now() + (here ? 30_000 : 0);
  for (;;) {
    await go(page, '/');
    await settle(page);
    if (!(await expandGroup(page, g))) { if (Date.now() > deadline) return false; await page.waitForTimeout(1500); continue; }
    if (await moreUntil(page, toggleOf(page, c), 2500)) return true;
    if (Date.now() > deadline) return false;
    await page.waitForTimeout(1500);
  }
}
export async function inList(page: Page, c: Chat): Promise<boolean> {
  await go(page, '/');
  await settle(page);
  const t = toggleOf(page, c);
  for (const g of GROUPS) {
    await expandGroup(page, g);
    if (await moreUntil(page, t, 1200)) return true;
  }
  return false;
}

/** Open the row (in its group) and return the row's box. */
export async function openRow(page: Page, c: Chat): Promise<Locator> {
  const t = await showRow(page, c);
  for (let attempt = 0; attempt < 2; attempt++) {
    if (!(await isOpen(t))) await t.click();
    // the page may redraw the row on its own live update between the click and the look: look for a while, click once more
    const opened = await expect.poll(() => isOpen(t), { timeout: attempt === 0 ? 7000 : 15_000 }).toBe(true).then(() => true, () => false);
    if (opened) return rowBox(t);
  }
  throw new Error(`${c.number}: the row never opened after two taps on its top line`);
}

export const btn = (row: Locator, name: string): Locator =>
  row.getByRole('button', { name, exact: true }).locator('visible=true').first();

/** Click a form button and wait until the server answered. */
export async function press(page: Page, b: Locator) {
  const answered = page.waitForResponse((r) => r.request().method() !== 'GET', { timeout: 10_000 }).catch(() => null);
  for (let i = 0; ; i++) {
    try { await b.click({ timeout: 8000 }); break; } catch (e) {
      if (i >= 2) throw e; // the list may have been redrawn under the click: look again
      await page.waitForTimeout(400);
    }
  }
  await answered;
  await page.waitForLoadState('load').catch(() => {});
}

/** The words of the row's actions in the ribbon on a laptop (SPEC 1) for the words of the row's buttons on a phone. */
export const RIBBON_WORD: Record<string, string> = { 'Mark pending': 'Pending', 'Resolve': 'Resolve', 'Reopen': 'Reopen', 'To do': 'To do' };

/** Choose a row on a laptop: a click on its top line opens it and makes it the one the ribbon acts on. */
export async function choose(row: Locator) {
  const t = row.locator('[aria-expanded]').first();
  if ((await t.getAttribute('aria-expanded').catch(() => null)) !== 'true') await t.click();
  await expect.poll(() => t.getAttribute('aria-expanded')).toBe('true');
}

/** Press a row action: through the ribbon on a laptop (the row is chosen first), through the row's own icon buttons on a
 *  phone (one bar, no three-dots menu). A proposal's Approve and Dismiss stay in the row on both. */
export async function act(row: Locator, name: string) {
  const page = row.page();
  if (isLaptop(page) && RIBBON_WORD[name]) {
    await choose(row);
    await press(page, ribbon(page).getByRole('button', { name: RIBBON_WORD[name], exact: true }));
    return;
  }
  // On a phone the row's actions are icons in the one bar of the open row, each named by its word (point 5): no menu.
  const b = btn(row, name);
  await press(row.page(), b);
}

export const noteBox = (row: Locator): Locator => row.getByPlaceholder('Add a note or ask Claude…').first();

/** The form of the note box (a chat row has a second box and a second Send above it: that one is for the chat itself). */
export const noteForm = (row: Locator): Locator => noteBox(row).locator('xpath=ancestor::form[1]');
export const chatBox = (row: Locator, label: string): Locator => row.getByPlaceholder(`Write in ${label}…`).first();
export const chatForm = (row: Locator, label: string): Locator => chatBox(row, label).locator('xpath=ancestor::form[1]');

export async function say(row: Locator, text: string) {
  await noteBox(row).fill(text);
  await noteForm(row).getByRole('button', { name: 'Send', exact: true }).click();
}

/** Visible text inside the row (closed copies of the same text do not count). */
export const seen = (row: Locator, text: string | RegExp): Locator => row.getByText(text).locator('visible=true').first();

export async function absTop(l: Locator): Promise<number> {
  return l.evaluate((el) => el.getBoundingClientRect().top + window.scrollY);
}

// ---------------------------------------------------------------- other pages

const PAGES: Record<string, { path: string; heading: string }> = {
  settings: { path: '/settings', heading: 'Settings' },
  notes: { path: '/notes', heading: 'Notes' },
  people: { path: '/people', heading: 'People' },
  closed: { path: '/closed', heading: 'Closed' },
  connectors: { path: '/connectors', heading: 'Connectors' },
};

/** Open a page of the app by its address, or else by its link. */
export async function visit(page: Page, key: keyof typeof PAGES | string) {
  const p = PAGES[key];
  const h = page.getByRole('heading', { name: p.heading, exact: true });
  await go(page, p.path);
  if (await h.first().waitFor({ state: 'visible', timeout: 3000 }).then(() => true, () => false)) return;
  await go(page, '/');
  if (key === 'people') {
    await page.getByRole('link', { name: 'Settings', exact: true }).first().click();
    await page.getByRole('link', { name: 'People', exact: true }).first().click();
  } else if (key === 'closed') {
    await page.getByRole('link', { name: /^Closed \(\d+(, \d+ not seen)?\)$/ }).first().click();
  } else if (key === 'connectors') {
    await page.getByRole('link', { name: /^Connectors:/ }).first().click();
  } else {
    await page.getByRole('link', { name: p.heading, exact: true }).first().click();
  }
  await expect(h.first()).toBeVisible();
}

/** Open the People page and find a person the way a user does: through the page's own search box (the list can hold
 *  hundreds of people and shows a page at a time, so a test never expects its own person on the first page). */
export async function findPerson(page: Page, text: string) {
  await visit(page, 'people');
  const box = page.getByLabel('Find a person', { exact: true }).first();
  await box.waitFor({ state: 'visible', timeout: 2500 }).catch(() => {});
  if (await box.isVisible().catch(() => false)) {
    await box.fill(text);
    const answered = page.waitForLoadState('load').catch(() => {});
    await page.getByRole('button', { name: 'Find', exact: true }).first().click();
    await answered;
    await expect(page.getByText(text).first()).toBeVisible();
  }
}

/** The agenda: a side panel on a laptop, a view of its own (dialog) on a phone. */
export const sidebar = (page: Page): Locator =>
  page.getByRole('complementary', { name: 'Agenda' })
    .or(page.getByRole('dialog', { name: 'Agenda' }))
    .or(page.getByRole('region', { name: 'Agenda' })).first();
export const agendaButton = (page: Page): Locator => page.getByRole('button', { name: 'Agenda', exact: true }).first();

export async function openAgenda(page: Page): Promise<Locator> {
  await go(page, '/');
  const side = sidebar(page);
  // the page remembers the choice: give a late drawn agenda a moment before pressing the button that would hide it
  await side.waitFor({ state: 'visible', timeout: 2500 }).catch(() => {});
  if (!(await side.isVisible().catch(() => false))) await agendaButton(page).click();
  await expect(side).toBeVisible();
  return side;
}

/** One connection's row on the Connectors page, opened (the rows are closed until they are clicked). */
export async function connection(page: Page, name: string): Promise<Locator> {
  const region = page.getByRole('region', { name, exact: true });
  await expect(region, name).toBeVisible();
  const summary = region.locator('summary').first();
  const open = await summary.evaluate((el) => (el.parentElement as HTMLDetailsElement | null)?.open === true).catch(() => true);
  if (!open) {
    // The connection's name is a link (it shows only that connection's rows, point 23): press the status, not the name.
    const status = summary.getByText(/^(OK|Stale|Error|Needs login|Not configured)$/).first();
    if (await status.count()) await status.click(); else await summary.click();
  }
  return region;
}

// ---------------------------------------------------------------- bytes

function crc32(buf: Buffer): number {
  let c: number, crc = 0xffffffff;
  for (let n = 0; n < buf.length; n++) {
    c = (crc ^ buf[n]) & 0xff;
    for (let k = 0; k < 8; k++) c = c & 1 ? 0xedb88320 ^ (c >>> 1) : c >>> 1;
    crc = (crc >>> 8) ^ c;
  }
  return (crc ^ 0xffffffff) >>> 0;
}
function chunk(type: string, data: Buffer): Buffer {
  const len = Buffer.alloc(4); len.writeUInt32BE(data.length);
  const body = Buffer.concat([Buffer.from(type, 'ascii'), data]);
  const crc = Buffer.alloc(4); crc.writeUInt32BE(crc32(body));
  return Buffer.concat([len, body, crc]);
}
/** A real PNG of the given size (red), so a test can tell its picture from every other. */
export function makePng(w: number, h: number): Buffer {
  const ihdr = Buffer.alloc(13);
  ihdr.writeUInt32BE(w, 0); ihdr.writeUInt32BE(h, 4); ihdr[8] = 8; ihdr[9] = 2;
  const row = Buffer.concat([Buffer.from([0]), Buffer.alloc(w * 3, 0)]);
  for (let x = 0; x < w; x++) row[1 + x * 3] = 255;
  const raw = Buffer.concat(Array.from({ length: h }, () => row));
  return Buffer.concat([
    Buffer.from([0x89, 0x50, 0x4e, 0x47, 0x0d, 0x0a, 0x1a, 0x0a]),
    chunk('IHDR', ihdr), chunk('IDAT', zlib.deflateSync(raw)), chunk('IEND', Buffer.alloc(0)),
  ]);
}

/** Give a file to a file input inside `scope`, whether the input is on the page or opens a chooser. */
export async function chooseFile(page: Page, scope: Locator, buttonName: string,
  files: { name: string; mimeType: string; buffer: Buffer } | { name: string; mimeType: string; buffer: Buffer }[]) {
  const input = scope.locator('input[type=file]');
  if (await input.count()) {
    await input.first().setInputFiles(files);
    if (buttonName === 'Set picture') {
      // A picture form is sent with its own submit button, after the file is chosen.
      const answered = page.waitForResponse((r) => r.request().method() !== 'GET', { timeout: 10_000 }).catch(() => null);
      await scope.getByRole('button', { name: buttonName, exact: true }).first().click({ timeout: 3000 }).catch(() => {});
      await answered;
      await page.waitForLoadState('load').catch(() => {});
    }
    return;
  }
  const [fc] = await Promise.all([
    page.waitForEvent('filechooser', { timeout: 5000 }),
    scope.getByRole('button', { name: buttonName, exact: true }).first().click(),
  ]);
  await fc.setFiles(files);
}

export const mcpHeaders = { 'Content-Type': 'application/json', Accept: 'application/json, text/event-stream' };
export async function mcp(page: Page, method: string, params?: unknown, who: Who = 'agent') {
  const headers: Record<string, string> = { ...mcpHeaders };
  if (who === 'agent') headers.Authorization = `Bearer ${await token(page, 'Agent')}`;
  else if (who !== 'none') headers.Authorization = `Bearer ${who}`;
  const res = await page.request.fetch('/api/mcp', {
    method: 'POST', headers, data: JSON.stringify({ jsonrpc: '2.0', id: 2, method, params }), failOnStatusCode: false,
  });
  const text = await res.text();
  let json: any = null;
  try { json = JSON.parse(text); } catch {
    const m = /data:\s*(\{.*\})/.exec(text); // an event stream with one message
    if (m) { try { json = JSON.parse(m[1]); } catch { /* not json */ } }
  }
  return { status: res.status(), json, text };
}
export const isReading = (n: string) => /^(list_|get_|search_)/.test(n) || ['history_numbers', 'service_status', 'private_list'].includes(n);
export async function tool(page: Page, name: string, args: Record<string, unknown>, who: Who = 'agent') {
  const run = currentRun();
  const a = run && !isReading(name) && !('run' in args) ? { ...args, run } : args;
  const r = await mcp(page, 'tools/call', { name, arguments: a }, who);
  expect(r.status, r.text.slice(0, 200)).toBe(200);
  const res = r.json?.result;
  const text = String(res?.content?.[0]?.text ?? '');
  let data: any = null;
  try { data = JSON.parse(text); } catch { /* plain text */ }
  return { isError: !!res?.isError, text, data, res };
}

/** A refusal by a guard (point 22): `Refused by guard: <guard name>` followed by one sentence saying why. */
export function expectRefusal(error: unknown, guard: string) {
  const s = String(error);
  const esc = guard.replace(/[.*+?^${}()|[\]\\]/g, '\\$&');
  expect(s).toMatch(new RegExp('^Refused by guard: ' + esc + '[.:]? +\\S[^]*[.!?]$'));
  const why = s.replace(new RegExp('^Refused by guard: ' + esc + '[.:]? +'), '');
  expect(why.length, 'one sentence saying why: ' + s).toBeGreaterThan(8);
  expect(why.slice(0, -1), 'one sentence, not several: ' + s).not.toMatch(/[.!?] +[A-Z]/);
}
