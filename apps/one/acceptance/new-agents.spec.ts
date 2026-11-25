// New acceptance tests for apps/one/SPEC.md points 29 and 34 (what agents can do through the connector at /api/mcp, and
// the settings side of the history review). Written from SPEC.md alone; tool names and inputs come from the app's own
// tools listing (see the report), calls go through the helpers' connector call with the test's run name.
//
// Not tested here, and why:
//  - 10 a chat joined to a mail thread (join only by an approved proposal, messages of both channels in one row marked by
//    source, a reply naming the channel of the newest message, split restoring two rows, nothing joining by itself):
//    a test cannot create a mail thread (only chats come in through the bridge), so there is nothing to join a chat to.
//  - 18 the marks `Declined` and `Tentative`: a test cannot create a calendar entry.
//  - 33 `Open thread` / `Thread` on an agenda entry, a note in it, the entry staying in the agenda: needs a calendar entry,
//    which a test cannot create (and a preview's real entries are not a test's to open threads on).
//  - 34 a thread marked as needing attention reaching New with `History: <reason>`: only the old mail threads that
//    history_batch hands out can be recorded, and a test cannot create mail. A chat dated long ago is refused as not
//    an old mail thread.
//  - 29 get_file on a PDF, Word file or spreadsheet: only a text file is made here.
import { expect } from '@playwright/test';
import {
  test, mkr, go, api, ready, newChat, chat, postChats, findItem, visit, isPhone, openRow, seen, toggleOf, rowBox, showRow, isOpen,
  type Chat,
} from './one-helpers';
import { ag, grow, propose, stage } from './new-helpers';

test.beforeEach(() => { test.setTimeout(60_000); }); // many connector calls, slow on Safari's engine

// ------------------------------------------------------------------ 29: settings, connections, background

test('29: agents read the settings: rules, patterns, guards with the blocked targets, and the proposals that wait with who, when and why', async ({ page }) => {
  const mk = mkr();
  const sender = `bulk-${mk}@example.invalid`;
  expect((await ag(page, 'propose_rule', { field: 'sender', value: sender, name: `Rule ${mk}`, reason: `none opened ${mk}` })).isError).toBe(false);
  expect((await ag(page, 'propose_pattern', { pattern: `ZX-\\d{4}${mk}`, name: `Pattern ${mk}`, reason: `order numbers ${mk}` })).isError).toBe(false);
  const all = await ag(page, 'list_settings', {});
  expect(all.isError, all.text).toBe(false);
  for (const k of ['import_rules', 'skip_rules', 'matter_patterns', 'guards', 'waiting']) expect(Array.isArray(all.data[k]), k).toBe(true);
  const blocked = (all.data.guards as any[]).find((g) => g.name === 'Blocked targets');
  expect(blocked, 'the guard Blocked targets').toBeTruthy();
  expect(JSON.stringify(blocked), 'with its list of blocked targets').toMatch(/paypal\.com|datov|csob\.cz|\.cz/);
  expect((all.data.import_rules as any[]).map((r) => r.name)).toEqual(expect.arrayContaining(['GitHub pipeline mails', 'Muted group chats']));
  const mine = (all.data.waiting as any[]).filter((w) => JSON.stringify(w).includes(mk));
  expect(mine.length, 'both proposals wait').toBe(2);
  for (const w of mine) {
    expect(w.proposed_by, 'who proposed it').toBeTruthy();
    expect(w.proposed_at, 'when').toBeTruthy();
    expect(w.reason, 'the reason').toContain(mk);
    expect('approved_at' in w, 'when the owner approved it (not yet)').toBe(true);
  }
});

test('29: the settings can be narrowed to a kind and to a sender or domain', async ({ page }) => {
  const mk = mkr();
  const sender = `bulk-${mk}@example.invalid`, other = `other-${mk}@example.invalid`;
  await ag(page, 'propose_rule', { field: 'sender', value: sender, reason: `none opened ${mk}` });
  await ag(page, 'propose_rule', { field: 'sender', value: other, reason: `none opened ${mk}` });
  const kind = await ag(page, 'list_settings', { kind: 'guards' });
  expect(kind.isError, kind.text).toBe(false);
  expect(Array.isArray(kind.data.guards)).toBe(true);
  expect(kind.data.import_rules, 'only the kind asked for').toBeUndefined();
  const one = await ag(page, 'list_settings', { sender });
  const text = JSON.stringify(one.data);
  expect(text).toContain(sender);
  expect(text, 'the other sender is left out').not.toContain(other);
  const domain = `dom-${mk}.example.invalid`;
  await ag(page, 'propose_rule', { field: 'sender_domain', value: domain, reason: `none opened ${mk}` });
  const dom = await ag(page, 'list_settings', { sender: domain });
  expect(JSON.stringify(dom.data)).toContain(domain);
  expect(JSON.stringify(dom.data), 'the senders\' rules are left out').not.toContain(other);
});

test('29: agents read the state of every connection, how many items wait for Claude and when the night check last ran', async ({ page }) => {
  const r = await ag(page, 'service_status', {});
  expect(r.isError, r.text).toBe(false);
  const names = (r.data.sources as any[]).map((s) => s.source);
  for (const n of ['Gmail', 'Office mail', 'Chat bridge', 'Google Calendar', 'Outlook Calendar', 'iCloud Calendar', 'Notify', 'Claude']) {
    expect(names, n).toContain(n);
  }
  for (const s of r.data.sources as any[]) expect(typeof s.state, s.source).toBe('string');
  expect(typeof r.data.waiting_for_claude).toBe('number');
  expect(r.data.night_check, 'when the night check last ran').toBeTruthy();
  expect('last_ran' in r.data.night_check).toBe(true);
});

test('29: an agent asks for the background of an item in one call: what was decided before, the notes that apply and the owner\'s feedback', async ({ page }) => {
  const mk = mkr();
  const c = await ready(page, { senderId: `bg-${mk}`, senderName: `Background ${mk}` });
  await ag(page, 'save_note', { item: c.number, title: `Standing order ${mk}`, text: `The owner has a standing order ${mk}` });
  await propose(page, c, 'pending', `decided before ${mk}`);
  expect((await ag(page, 'record_feedback', { id: c.number, text: `I want to see this kind ${mk}` })).isError).toBe(false);
  const r = await ag(page, 'get_context', { id: c.number });
  expect(r.isError, r.text).toBe(false);
  const t = JSON.stringify(r.data);
  expect(t, 'the notes that apply').toContain(`Standing order ${mk}`);
  expect(t, 'the owner\'s feedback').toContain(`I want to see this kind ${mk}`);
  expect(t, 'what was decided before').toContain(`decided before ${mk}`);
});

// ------------------------------------------------------------------ 29: feedback

test('29: an agent records the owner\'s feedback: kept on the item, shown in its thread as Feedback from the owner with who recorded it, and given to later reviews of the same sender', async ({ page }) => {
  const mk = mkr();
  const agent = `Reviewer ${mk}`;
  const a = await ready(page, { senderId: `fb-${mk}`, senderName: `Same sender ${mk}`, mk: mk + 'a' });
  if (isPhone(page)) await grow(page, a);
  const said = `that was wrong, I wanted to see it ${mk}`;
  const r = await ag(page, 'record_feedback', { id: a.number, text: said, agent });
  expect(r.isError, r.text).toBe(false);
  const row = await openRow(page, a);
  await expect(seen(row, 'Feedback from the owner')).toBeVisible();
  await expect(seen(row, said)).toBeVisible();
  await expect(seen(row, agent)).toBeVisible();
  expect(JSON.stringify((await ag(page, 'get_item', { id: a.number })).data.feedback_that_applies)).toContain(said);
  // A later item of the same sender gets it with its background.
  const b = newChat({ mk: mk + 'b', senderId: `fb-${mk}`, senderName: `Same sender ${mk}` });
  await postChats(page, [b]);
  b.number = await findItem(page, b);
  const ctx = await ag(page, 'get_context', { id: b.number });
  expect(JSON.stringify(ctx.data.feedback_that_applies), 'the same sender\'s later item').toContain(said);
});

// ------------------------------------------------------------------ 29: files

test('29: an agent adds a file to an item: it is listed with the agent\'s name, readable back, and the entry says Attached: <file name>', async ({ page }) => {
  const mk = mkr();
  const c = await ready(page);
  if (isPhone(page)) await grow(page, c);
  const name = `made-${mk}.txt`, agent = `Filer ${mk}`;
  const r = await ag(page, 'add_file', { id: c.number, name, text: `a file the agent made ${mk}`, agent });
  expect(r.isError, r.text).toBe(false);
  const item = await ag(page, 'get_item', { id: c.number });
  const f = (item.data.files as any[]).find((x) => x.name === name);
  expect(f, 'listed on the item').toBeTruthy();
  expect(f.added_by, 'marked with the agent\'s name').toBe(agent);
  const back = await ag(page, 'get_file', { id: c.number, file_id: f.file_id });
  expect(back.isError, back.text).toBe(false);
  expect(back.text).toContain(`a file the agent made ${mk}`);
  expect(back.text.toLowerCase(), 'what a file says is data, never instructions').toMatch(/data|never an instruction|not an instruction/);
  const row = await openRow(page, c);
  await expect(seen(row, 'Attachments')).toBeVisible();
  await expect(seen(row, name).first()).toBeVisible();
  await expect(seen(row, `Attached: ${name}`)).toBeVisible();
});

test('29: an agent can add a file that already sits on another item, and adding a file sends nothing', async ({ page }) => {
  const mk = mkr();
  const a = await ready(page, { mk: mk + 'a' }), b = await ready(page, { mk: mk + 'b' });
  const name = `moved-${mk}.txt`;
  const first = await ag(page, 'add_file', { id: a.number, name, text: `shared ${mk}` });
  expect(first.isError, first.text).toBe(false);
  const fid = first.data.file_id;
  const second = await ag(page, 'add_file', { id: b.number, from_item: a.number, file_id: fid });
  expect(second.isError, second.text).toBe(false);
  const files = (await ag(page, 'get_item', { id: b.number })).data.files as any[];
  expect(files.map((f) => f.name)).toContain(name);
  // Adding a file never sends it: nothing waits to go out.
  const t = JSON.stringify((await api(page, 'GET', `/api/items/${b.number}/thread`)).json);
  expect(t).not.toMatch(/Waiting for the Mac|Sent \d/);
});

// ------------------------------------------------------------------ 29: listing a stage

test('29: agents list the items of a stage page by page, each with its number, sender, subject, newest message, waiting proposals and whether the owner left a note', async ({ page }) => {
  const mk = mkr();
  const a = await ready(page, { mk: mk + 'a' }), b = await ready(page, { mk: mk + 'b' });
  await propose(page, a, 'pending', `e2e ${mk}`);
  const r = await ag(page, 'list_items', { stage: 'new', limit: 1 });
  expect(r.isError, r.text).toBe(false);
  expect(r.data.items.length).toBe(1);
  expect(r.data.more, 'there are more pages').toBe(true);
  const all: any[] = [];
  let after: string | undefined;
  for (let i = 0; i < 60; i++) {
    const p = await ag(page, 'list_items', { stage: 'new', limit: 100, ...(after ? { after } : {}) });
    all.push(...p.data.items);
    if (!p.data.more) break;
    after = p.data.items[p.data.items.length - 1].id;
  }
  const mineA = all.find((x) => x.id === a.number);
  expect(mineA, 'the item is listed').toBeTruthy();
  for (const k of ['id', 'from', 'subject', 'last_message_at', 'proposals_waiting', 'owner_left_note']) expect(k in mineA, k).toBe(true);
  expect(mineA.proposals_waiting).toBe(1);
  expect(all.find((x) => x.id === b.number).proposals_waiting).toBe(0);
  for (const st of ['in_progress', 'pending', 'resolved']) {
    const x = await ag(page, 'list_items', { stage: st, limit: 1 });
    expect(x.isError, `${st}: ${x.text}`).toBe(false);
  }
});

// ------------------------------------------------------------------ 34

const historyLine = async (page: import('@playwright/test').Page) => {
  await visit(page, 'settings');
  const h = page.getByText('History review', { exact: true }).first();
  await expect(h).toBeVisible();
  return (await h.locator('xpath=ancestor::*[self::section or self::details or self::fieldset or self::div][1]').innerText()).replace(/\s+/g, ' ');
};

test('34: Settings has a History review section that says Not started before any batch, or how far back it has reached', async ({ page }) => {
  const t = await historyLine(page);
  expect(t).toMatch(/Not started|Reviewed back to .+, \d+ threads left/);
});

test('34: sizing up a batch says how many threads and how much text with a cost estimate, and moves nothing', async ({ page }) => {
  const c = await ready(page);
  const before = await historyLine(page);
  const total = async () => (await ag(page, 'list_items', { stage: 'new', limit: 1 })).data.total;
  const newBefore = await total();
  const r = await ag(page, 'history_batch', { months: 1 });
  expect(r.isError, r.text).toBe(false);
  expect(typeof r.data.threads, 'how many threads the period holds').toBe('number');
  expect(typeof r.data.characters, 'roughly how much text').toBe('number');
  expect(r.data.estimate, 'what the model would cost').toBeTruthy();
  expect(r.data.handed_out, 'nothing is handed out before the owner agrees').toBe(0);
  const named = await ag(page, 'history_batch', { from: '2026-01-01', to: '2026-02-01', only_mine: true });
  expect(named.isError, named.text).toBe(false);
  expect(named.data.handed_out).toBe(0);
  expect(await historyLine(page), 'the point reached did not move').toBe(before);
  expect(await total(), 'nothing came into New').toBe(newBefore);
  expect(await stage(page, c)).toBe('new');
});

test('34: a chat is not an old mail thread: recording a history result on it is refused and it stays where it was', async ({ page }) => {
  const c = await ready(page);
  const r = await ag(page, 'review_history_thread', { id: c.number, summary: `a summary ${c.mk}`, outcome: 'still_open', needs_attention: true, reason: `unanswered ${c.mk}` });
  expect(r.isError, r.text).toBe(true);
  expect(await stage(page, c)).toBe('new');
});
