// Acceptance tests for apps/one/SPEC.md point 31: skip rules, the app's own review, the night check.
//
// Skip rules are made under the test's own run (API.md, "Skip rules under a run"): the rule carries the run's
// mark, applies only to that run's items and goes with the run. Each test that makes one removes its run at the
// end and checks that no rule is left (an unmarked rule or proposal is never made).
//  - The Claude API key is the owner's to enter: no test enters, saves or removes a key, and none causes a call to a
//    real model. What needs a saved key is written as test.fixme.
//  - Mail in both mailboxes and calendar notices cannot be put in from a browser.
import { expect, type Page, type Locator } from '@playwright/test';
import {
  test, mkr, currentRun, api, readyBare as ready, skip, stageOf, waitsForClaude, queueEntries, addMsg, chat, newChat, postChats, findItem,
  visit, go, act, openRow, stripValue, seen, connection, isLaptop, connectionsPane, type Chat,
} from './one-helpers';

const sender = (mk: string, domain = 'shop.test') => `bulk-${mk}@${domain}`;

/** Five chats from one sender address, each looked at by Claude and then clear-skipped (the numbers a skip rule needs). */
async function skipped(page: Page, address: string, mk: string, o: { n?: number; before?: (cs: Chat[]) => Promise<void> } = {}): Promise<Chat[]> {
  const cs: Chat[] = [];
  for (let i = 0; i < (o.n ?? 5); i++) cs.push(await ready(page, { senderId: address, senderName: `Bulk ${mk}`, net: 'messenger', text: `Offer ${i} ${mk}` }));
  if (o.before) await o.before(cs);
  for (const c of cs) {
    const s = await skip(page, c, 'e2e noise', 'noise');
    expect(s.status, s.text).toBe(200);
  }
  return cs;
}

/** Propose a skip rule, under this test's own run. */
const propose = (page: Page, field: 'sender' | 'sender_domain', value: string, mk: string, name = `Rule ${mk}`) =>
  api(page, 'POST', '/api/skip-rule-proposals', { field, value, name, reason: `skipped 5 times, brought back 0 times ${mk}` });

/** A proposal that must be refused: if the server wrongly accepts it, it is dismissed again so nothing is left waiting. */
async function refused(page: Page, field: 'sender' | 'sender_domain', value: string, mk: string) {
  const r = await propose(page, field, value, mk);
  if (r.status === 200) await dismissProposal(page, `Rule ${mk}`);
  return r;
}

/** Remove the test's run now and check that no skip rule or proposal of it is left anywhere. */
async function runGoneWithoutRules(page: Page, names: string[], counted = false) {
  const run = currentRun()!;
  const r = await api(page, 'DELETE', `/api/test-runs/${run}`, undefined, 'agent', null);
  expect(r.status, r.text).toBe(200);
  expect(r.json.removed.skip_rules, 'the run\'s skip rule goes with it').toBeGreaterThanOrEqual(counted ? 1 : 0);
  const left = await api(page, 'GET', '/api/test-runs', undefined, 'agent', null);
  expect((left.json.runs ?? []).map((x: any) => x.run)).not.toContain(run);
  await visit(page, 'settings');
  for (const n of names) await expect(page.getByText(n, { exact: true }), 'no rule left: ' + n).toHaveCount(0);
}

const ruleCard = (page: Page, name: string): Locator =>
  page.getByText(name, { exact: true }).first().locator('xpath=ancestor::*[self::div or self::li or self::article][1]');

async function dismissProposal(page: Page, name: string) {
  await visit(page, 'settings');
  const card = ruleCard(page, name);
  if (await card.getByRole('button', { name: 'Dismiss', exact: true }).isVisible().catch(() => false)) {
    const answered = page.waitForResponse((r) => r.request().method() !== 'GET', { timeout: 10_000 }).catch(() => null);
    await card.getByRole('button', { name: 'Dismiss', exact: true }).click();
    await answered;
  }
}

/** True when the owner has saved a Claude API key on this app: tests that rely on "no key" then step aside. */
async function keySaved(page: Page): Promise<boolean> {
  await visit(page, 'connectors');
  const region = page.getByRole('region', { name: 'Claude API', exact: true });
  await expect(region).toBeVisible();
  return (await region.getByText('Connected', { exact: true }).count()) > 0;
}

// ------------------------------------------------------------------ settings and connections

test('31: Settings has the section Skip rules, and the Claude settings with their defaults', async ({ page }) => {
  await visit(page, 'settings');
  await expect(page.getByRole('heading', { name: 'Skip rules', exact: true })).toBeVisible();
  for (const label of ['Review model', 'Daily limit', 'Night check at', 'Night check model', 'Night check limit']) {
    await expect(page.getByLabel(label, { exact: true }).first(), label).toBeVisible();
  }
  // To begin with: the middle model and 2 USD a day; the night check uses the best model.
  expect(await page.getByLabel('Review model', { exact: true }).first().inputValue()).toMatch(/sonnet/i);
  expect(Number(await page.getByLabel('Daily limit', { exact: true }).first().inputValue())).toBe(2);
  expect(await page.getByLabel('Night check model', { exact: true }).first().inputValue()).toMatch(/opus/i);
  expect(await page.getByLabel('Night check at', { exact: true }).first().inputValue()).toMatch(/^\d{1,2}:\d{2}$/);
  expect(Number(await page.getByLabel('Night check limit', { exact: true }).first().inputValue())).toBeGreaterThan(0);
});

test('31: Connections has the section Claude API with the field API key, Save and Remove, and a state', async ({ page }) => {
  await visit(page, 'connectors');
  const row = await connection(page, 'Claude API');
  // Looked at only: no key is entered, saved or removed by a test.
  await expect(row.getByLabel('API key', { exact: true })).toBeVisible();
  await expect(row.getByRole('button', { name: 'Save', exact: true })).toBeVisible();
  await expect(row.getByRole('button', { name: 'Remove', exact: true })).toBeVisible();
  const state = row.getByText(/^(Connected|Not configured)$/).first();
  await expect(state).toBeVisible();
  // The key is never shown again: the field holds no saved key.
  expect(await row.getByLabel('API key', { exact: true }).inputValue()).toBe('');
});

test('31: with no key saved a new item keeps waiting for an agent as before', async ({ page }) => {
  test.skip(await keySaved(page), 'a Claude API key is saved here: the app would read the item itself');
  const row = await connection(page, 'Claude API');
  await expect(row.getByText('Not configured', { exact: true }).first()).toBeVisible();
  const c = await chat(page);
  expect(await waitsForClaude(page, c)).toBe(true);
  await go(page, '/');
  // The strip of a phone; on a laptop the same is said in Connections (SPEC 1).
  if (isLaptop(page)) await expect(connectionsPane(page).getByRole('link', { name: /^Claude/ })).toContainText(/\d+ waiting/);
  else await expect.poll(async () => (await stripValue(page, /(\d+) waiting for Claude/)) !== null, { message: 'the strip (or the `+n more` that folds it) says items wait for Claude' }).toBe(true);
  // Nothing is skipped and nothing is written in the thread by the app on its own.
  await page.waitForTimeout(1500);
  expect(await waitsForClaude(page, c)).toBe(true);
  expect(await stageOf(page, c)).not.toBe('closed');
  const thread = await api(page, 'GET', `/api/items/${c.number}/thread`);
  expect(JSON.stringify(thread.json.thread)).not.toContain('model_failed');
  await api(page, 'POST', `/api/items/${c.number}/seen`, {});
});

// ------------------------------------------------------------------ a proposed skip rule

test('31: a skip rule proposed through the agents\' address waits as a Proposal with Approve and Dismiss and does nothing yet', async ({ page }) => {
  const mk = mkr();
  const addr = sender(mk);
  const name = `Rule ${mk}`;
  await skipped(page, addr, mk);
  const r = await propose(page, 'sender', addr, mk, name);
  expect(r.status, r.text).toBe(200);
  expect(r.json.skip_rule_proposal_id).toBeTruthy();
  expect(r.json.numbers.skipped).toBe(5);
  expect(r.json.numbers.brought_back).toBe(0);
  try {
    // The same proposal twice answers the waiting one.
    const again = await propose(page, 'sender', addr, mk, name);
    expect(again.status, again.text).toBe(200);
    expect(again.json.unchanged).toBeTruthy();
    // It is shown to the owner with its numbers and the two buttons.
    await visit(page, 'settings');
    const card = ruleCard(page, name);
    await expect(card).toBeVisible();
    await expect(card.getByText('Proposal', { exact: true })).toBeVisible();
    await expect(card.getByRole('button', { name: 'Approve', exact: true })).toBeVisible();
    await expect(card.getByRole('button', { name: 'Dismiss', exact: true })).toBeVisible();
    await expect(card).toContainText('skipped 5 times, brought back 0 times');
    // Nothing under /api/ approves, switches on or changes a skip rule.
    const id = r.json.skip_rule_proposal_id;
    for (const [m, p] of [
      ['POST', `/api/skip-rule-proposals/${id}/approve`], ['POST', `/api/skip-rule-proposals/${id}/dismiss`],
      ['POST', `/api/skip-rules/${id}/approve`], ['POST', `/api/skip-rules/${id}/enable`], ['PUT', `/api/skip-rules/${id}`],
      ['DELETE', `/api/skip-rules/${id}`],
    ] as const) {
      const x = await api(page, m, p, { approved: true });
      expect(x.status, `${m} ${p}`).toBe(403);
      expect(x.json).toEqual({ ok: false, error: 'only the owner can do this' });
    }
    // Still waiting: a new item from that sender is not closed, it waits for Claude like any other.
    const fresh = await chat(page, { senderId: addr, senderName: `Bulk ${mk}`, net: 'messenger', text: `Another offer ${mk}` });
    expect(await stageOf(page, fresh)).not.toBe('closed');
    expect(await waitsForClaude(page, fresh)).toBe(true);
    await api(page, 'POST', `/api/items/${fresh.number}/seen`, {});
  } finally {
    await dismissProposal(page, name);
  }
  await runGoneWithoutRules(page, [name]);
});

test('31: Dismiss on a proposed skip rule removes the proposal and closes nothing', async ({ page }) => {
  const mk = mkr();
  const addr = sender(mk);
  const name = `Rule ${mk}`;
  await skipped(page, addr, mk);
  expect((await propose(page, 'sender', addr, mk, name)).status).toBe(200);
  await dismissProposal(page, name);
  await visit(page, 'settings');
  await expect(page.getByText(name, { exact: true })).toHaveCount(0);
  const fresh = await chat(page, { senderId: addr, senderName: `Bulk ${mk}`, net: 'messenger', text: `After dismiss ${mk}` });
  expect(await stageOf(page, fresh)).not.toBe('closed');
  await api(page, 'POST', `/api/items/${fresh.number}/seen`, {});
  await runGoneWithoutRules(page, [name]);
});

test('31: an approved skip rule closes a matching new item with Closed by a rule, it can be brought back, and the rule is then Paused', async ({ page }) => {
  test.slow(); // the one story of an approved rule: approve, close, bring back, paused: three times the usual time
  const mk = mkr();
  const addr = sender(mk);
  const name = `Rule ${mk}`;
  await skipped(page, addr, mk);
  expect((await propose(page, 'sender', addr, mk, name)).status).toBe(200);
  // The owner approves on the page.
  await visit(page, 'settings');
  const card = ruleCard(page, name);
  const answered = page.waitForResponse((r) => r.request().method() !== 'GET', { timeout: 10_000 }).catch(() => null);
  await card.getByRole('button', { name: 'Approve', exact: true }).click();
  await answered;
  await visit(page, 'settings');
  const rule = ruleCard(page, name);
  await expect(rule).toBeVisible();
  await expect(rule.getByRole('button', { name: 'Approve', exact: true })).toHaveCount(0);
  await expect(rule.getByText('Paused', { exact: true })).toHaveCount(0);

  // A new item from that sender is closed without asking Claude.
  const c = await chat(page, { senderId: addr, senderName: `Bulk ${mk}`, net: 'messenger', text: `Fresh offer ${mk}` });
  await expect.poll(() => stageOf(page, c), { timeout: 15_000 }).toBe('closed');
  expect(await waitsForClaude(page, c)).toBe(false);
  const thread = await api(page, 'GET', `/api/items/${c.number}/thread`);
  expect(JSON.stringify(thread.json.thread)).toContain(`Closed by a rule: ${name}`);

  // It is on the Closed page with the rule as the reason.
  await visit(page, 'closed');
  const entry = page.getByText(c.number).first().locator('xpath=ancestor::*[.//button[normalize-space()="Bring back"]][1]');
  await expect(entry).toContainText(`Closed by a rule: ${name}`);

  // An agent cannot pull it back: only the owner can.
  const un = await api(page, 'POST', `/api/items/${c.number}/unskip`, {});
  expect(un.status, un.text).toBeGreaterThanOrEqual(400);
  expect(await stageOf(page, c)).toBe('closed');

  // The owner brings it back: the item is New again and the rule is paused at once and shown to him.
  await act(entry, 'Bring back');
  expect(await stageOf(page, c)).toBe('new');
  await visit(page, 'settings');
  await expect(ruleCard(page, name).getByText('Paused', { exact: true })).toBeVisible();

  // A paused rule closes nothing more.
  const next = await chat(page, { senderId: addr, senderName: `Bulk ${mk}`, net: 'messenger', text: `Second fresh offer ${mk}` });
  await page.waitForTimeout(1000);
  expect(await stageOf(page, next)).not.toBe('closed');
  await api(page, 'POST', `/api/items/${next.number}/seen`, {});
  await api(page, 'POST', `/api/items/${c.number}/seen`, {});
  // The rule belongs to this run and goes with it.
  await runGoneWithoutRules(page, [name], true);
});

// ------------------------------------------------------------------ what a skip rule is never made for

test('31: refused, with the reason: fewer skips than the set number', async ({ page }) => {
  const mk = mkr();
  const addr = sender(mk);
  await skipped(page, addr, mk, { n: 4 });
  const r = await refused(page, 'sender', addr, mk);
  expect(r.status, r.text).toBe(409);
  expect(r.json.ok).toBe(false);
  expect(r.json.error).toMatch(/4/);
  expect(r.json.error).toMatch(/5/);
  expect(r.json.error).toMatch(/skip/i);
});

test('31: refused, with the reason: The owner has answered that sender', async ({ page }) => {
  const mk = mkr();
  const addr = sender(mk);
  await skipped(page, addr, mk, { before: async (cs) => { await addMsg(page, cs[0], { own: true, text: `my answer ${mk}`, minsAgo: 30 }); } });
  const r = await refused(page, 'sender', addr, mk);
  expect(r.status, r.text).toBe(409);
  expect(r.json.error).toMatch(/answer/i);
});

test('31: refused, with the reason: an item of that sender was brought back', async ({ page }) => {
  const mk = mkr();
  const addr = sender(mk);
  const cs = await skipped(page, addr, mk);
  await visit(page, 'closed');
  const entry = page.getByText(cs[0].number).first().locator('xpath=ancestor::*[.//button[normalize-space()="Bring back"]][1]');
  await act(entry, 'Bring back');
  expect(await stageOf(page, cs[0])).toBe('new');
  // The owner brought one back; five were skipped in all. (Skip the brought-back one again so the count is not the reason.)
  const again = await skip(page, cs[0], 'e2e noise again', 'noise');
  expect(again.status, again.text).toBe(200);
  const r = await refused(page, 'sender', addr, mk);
  expect(r.status, r.text).toBe(409);
  expect(r.json.error).toMatch(/brought back/i);
});

test('31: refused, with the reason: an item of that sender got a comment, so it has mattered', async ({ page }) => {
  const mk = mkr();
  const addr = sender(mk);
  await skipped(page, addr, mk, {
    before: async (cs) => { expect((await api(page, 'POST', `/api/items/${cs[1].number}/comments`, { text: `worth a word ${mk}` })).status).toBe(200); },
  });
  const r = await refused(page, 'sender', addr, mk);
  expect(r.status, r.text).toBe(409);
  expect(r.json.error).toMatch(/comment|mattered/i);
});

test('31: refused: a sender that the owner marked Always show me', async ({ page }) => {
  const mk = mkr();
  const addr = sender(mk);
  await skipped(page, addr, mk);
  const p = await api(page, 'POST', '/api/people/details', { person: `Bulk ${mk}`, always_show: true });
  expect(p.status, p.text).toBe(200);
  const r = await refused(page, 'sender', addr, mk);
  expect(r.status, r.text).toBe(403);
  expect(r.json.ok).toBe(false);
  expect(r.json.error).toMatch(/Protected people|Always show/i);
});

test('31: refused: a sender on the Blocked targets list (a bank, a payment service, the data box)', async ({ page }) => {
  const mk = mkr();
  // Read the list as the owner has it on Settings; take its first entry.
  await visit(page, 'settings');
  const list = (await page.getByLabel(/^Senders and domains/).first().inputValue()).split('\n').map((s) => s.trim()).filter(Boolean);
  expect(list.length).toBeGreaterThan(0);
  const target = list[0].replace(/^\*?@?/, '');
  const addr = target.includes('@') ? target : `e2e-${mk}@${target}`;
  await skipped(page, addr, mk);
  const r = await refused(page, 'sender', addr, mk);
  expect(r.status, r.text).toBe(403);
  expect(r.json.ok).toBe(false);
  expect(r.json.error).toMatch(/Blocked targets|bank|blocked/i);
});

test('31: a skip rule is made only for a sender address or a mailing domain', async ({ page }) => {
  const mk = mkr();
  const r = await api(page, 'POST', '/api/skip-rule-proposals', { field: 'subject', value: `x ${mk}`, reason: 'skipped 5 times, brought back 0 times', name: `Rule ${mk}` });
  expect(r.status, r.text).toBe(400);
  const none = await api(page, 'POST', '/api/skip-rule-proposals', { field: 'sender', value: sender(mk), name: `Rule ${mk}` });
  expect(none.status, none.text).toBeGreaterThanOrEqual(400);
  const nobody = await api(page, 'POST', '/api/skip-rule-proposals', { field: 'sender', value: sender(mk), name: `Rule ${mk}`, reason: 'skipped 5 times, brought back 0 times' }, 'none', null);
  expect(nobody.status).toBe(401);
  expect(nobody.json).toEqual({ ok: false, error: 'wrong token' });
});

// ------------------------------------------------------------------ needs a saved key, or mail a browser cannot send

test.fixme('31: the model reads a new item by itself and acts only through the agents\' actions', async () => {
  // Needs a saved Claude API key and a real model call: a test never enters a key.
});

test.fixme('31: while today\'s limit is used up the strip says `Review paused: today\'s limit is reached` and the items wait', async () => {
  // Needs a saved key and a spent daily limit, which only a real model run produces.
});

test.fixme('31: while the model cannot be reached the strip says `Review paused: Claude cannot be reached`, one notice `Review paused` is sent, nothing is skipped', async () => {
  // Needs a saved (wrong or unreachable) key; a test never enters one.
});

test.fixme('31: the night check brings an item back to New with `Night check: <reason>` and the morning line says `Night check: <n> messages, <m> brought back`', async () => {
  // Runs once a night with a saved key and the best model: cannot be started or seen without one.
});

test.fixme('31: the same mail in both mailboxes: the second copy is closed and says `Same mail as <number>`', async () => {
  // Mail cannot be put into the two mailboxes from a browser test (covered by the app\'s own tests).
});

test.fixme('31: a notice that someone added an entry to a shared calendar is closed by a skip rule', async () => {
  // Calendar notices cannot be put in from a browser test.
});

test.fixme('31: the import rules drop first, the skip rules next, the model last', async () => {
  // The order can only be seen with a saved key (what reaches the model).
});
