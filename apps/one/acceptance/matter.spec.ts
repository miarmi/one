// Acceptance tests for apps/one/SPEC.md point 10 (matter patterns, joining and splitting rows), the part that a
// browser test can reach: proposals made through the agents' address and shown on Settings. Joining and splitting
// act on rows of MAIL only (API.md: a chat is refused), and mail cannot be put in from a browser, so what a join,
// a split or a pattern does to mail is test.fixme at the end. Everything is made under the test's own run.
import { expect, type Page, type Locator } from '@playwright/test';
import { test, mkr, api, ready, visit, currentRun } from './one-helpers';

const patternCard = (page: Page, name: string): Locator =>
  page.getByText(name, { exact: true }).first().locator('xpath=ancestor::*[self::div or self::li or self::article][1]');

const pressAndWait = async (page: Page, b: Locator) => {
  const answered = page.waitForResponse((r) => r.request().method() !== 'GET', { timeout: 10_000 }).catch(() => null);
  await b.click();
  await answered;
  await page.waitForLoadState('load').catch(() => {});
};

async function gone(page: Page, name: string) {
  const run = currentRun()!;
  const r = await api(page, 'DELETE', `/api/test-runs/${run}`, undefined, 'agent', null);
  expect(r.status, r.text).toBe(200);
  await visit(page, 'settings');
  await expect(page.getByText(name, { exact: true }), 'no pattern left').toHaveCount(0);
}

test('10: a pattern proposed by an agent is a Proposal with Approve and Dismiss and joins nothing until approved', async ({ page }) => {
  const mk = mkr();
  const name = `Pattern ${mk}`;
  const pattern = `ORD-${mk}-[0-9]+`;
  const r = await api(page, 'POST', '/api/pattern-proposals', { pattern, name, reason: `three mails about one order ${mk}` });
  expect(r.status, r.text).toBe(200);
  expect(r.json.pattern_proposal_id).toBeTruthy();
  const again = await api(page, 'POST', '/api/pattern-proposals', { pattern, name, reason: `three mails about one order ${mk}` });
  expect(again.status, again.text).toBe(200);
  expect(again.json.unchanged).toBeTruthy();

  await visit(page, 'settings');
  const section = page.getByRole('heading', { name: 'Matter patterns', exact: true }).locator('xpath=ancestor::section[1]');
  const card = section.getByText(name, { exact: true }).first().locator('xpath=ancestor::*[.//button[normalize-space()="Dismiss"]][1]');
  await expect(card).toBeVisible();
  await expect(card.getByText('Proposal', { exact: true })).toBeVisible();
  await expect(card.getByRole('button', { name: 'Approve', exact: true })).toBeVisible();
  await expect(card.getByRole('button', { name: 'Dismiss', exact: true })).toBeVisible();
  await expect(section.getByRole('button', { name: 'Add pattern', exact: true })).toBeVisible();

  // Nothing under /api/ approves, adds or removes a pattern.
  const id = r.json.pattern_proposal_id;
  for (const [m, p] of [
    ['POST', `/api/pattern-proposals/${id}/approve`], ['POST', `/api/pattern-proposals/${id}/dismiss`],
    ['POST', '/api/patterns'], ['DELETE', `/api/patterns/${id}`], ['PUT', `/api/patterns/${id}`],
  ] as const) {
    const x = await api(page, m, p, { approved: true });
    expect(x.status, `${m} ${p}`).toBe(403);
    expect(x.json).toEqual({ ok: false, error: 'only the owner can do this' });
  }
  // The owner approves on the page: it stops being a proposal. Under a run it applies to the run's mail only and never
  // enters the owner's own list of patterns.
  await pressAndWait(page, card.getByRole('button', { name: 'Approve', exact: true }));
  await visit(page, 'settings');
  const now = page.getByRole('heading', { name: 'Matter patterns', exact: true }).locator('xpath=ancestor::section[1]');
  await expect(now.getByRole('button', { name: 'Approve', exact: true })).toHaveCount(0);
  await expect(now.getByText('Proposal', { exact: true })).toHaveCount(0);
  await expect(now.getByText(pattern)).toHaveCount(0);
  await gone(page, name);
});

test('10: Dismiss on a proposed pattern removes it', async ({ page }) => {
  const mk = mkr();
  const name = `Pattern ${mk}`;
  const r = await api(page, 'POST', '/api/pattern-proposals', { pattern: `FILE-${mk}-[0-9]+`, name, reason: `files ${mk}` });
  expect(r.status, r.text).toBe(200);
  await visit(page, 'settings');
  const card = page.getByText(name, { exact: true }).first().locator('xpath=ancestor::*[.//button[normalize-space()="Dismiss"]][1]');
  await pressAndWait(page, card.getByRole('button', { name: 'Dismiss', exact: true }));
  await visit(page, 'settings');
  await expect(page.getByText(name, { exact: true })).toHaveCount(0);
  await gone(page, name);
});

test('10: a pattern that is not a regular expression, or matches an empty text, is refused', async ({ page }) => {
  const mk = mkr();
  const bad = await api(page, 'POST', '/api/pattern-proposals', { pattern: '(', name: `Bad ${mk}`, reason: 'e2e' });
  expect(bad.status, bad.text).toBeGreaterThanOrEqual(400);
  expect(bad.json.ok).toBe(false);
  const empty = await api(page, 'POST', '/api/pattern-proposals', { pattern: 'x*', name: `Empty ${mk}`, reason: 'e2e' });
  expect(empty.status, empty.text).toBeGreaterThanOrEqual(400);
  expect(empty.json.ok).toBe(false);
  const nobody = await api(page, 'POST', '/api/pattern-proposals', { pattern: `Z-${mk}`, name: `N ${mk}` }, 'none');
  expect(nobody.status).toBe(401);
  expect(nobody.json).toEqual({ ok: false, error: 'wrong token' });
  await visit(page, 'settings');
  await expect(page.getByText(`Bad ${mk}`)).toHaveCount(0);
  await expect(page.getByText(`Empty ${mk}`)).toHaveCount(0);
});

test('10: a row cannot be joined with itself, and a join or split of chats is only ever a proposal that moves nothing', async ({ page }) => {
  const a = await ready(page), b = await ready(page);
  const self = await api(page, 'POST', `/api/items/${a.number}/merge-proposals`, { join: a.number, reason: 'e2e' });
  expect(self.status, self.text).toBe(409);
  expect(self.json.ok).toBe(false);
  // Joining is proposed, never done by itself (point 10): after the proposal both rows still stand as they were.
  const chats = await api(page, 'POST', `/api/items/${a.number}/merge-proposals`, { join: b.number, reason: 'e2e' });
  expect([200, 409], chats.text).toContain(chats.status);
  expect((await api(page, 'GET', `/api/items/${a.number}`)).json.item.stage).toBe('new');
  expect((await api(page, 'GET', `/api/items/${b.number}`)).json.item.stage).toBe('new');
  expect((await api(page, 'GET', `/api/items/${a.number}/thread`)).text).not.toMatch(/joined .* into this row/);
  expect((await api(page, 'GET', `/api/items/${b.number}/thread`)).text).not.toMatch(/joined .* into this row/);
});

test('10: a join or split proposal needs a row that exists, and a token; nothing under /api/ approves one', async ({ page }) => {
  const a = await ready(page);
  const none = await api(page, 'POST', `/api/items/${a.number}/merge-proposals`, { join: 'Q-9999999', reason: 'e2e' });
  expect(none.status, none.text).toBeGreaterThanOrEqual(400);
  const nobody = await api(page, 'POST', `/api/items/${a.number}/merge-proposals`, { join: a.number, reason: 'e2e' }, 'none');
  expect(nobody.status).toBe(401);
  expect(nobody.json).toEqual({ ok: false, error: 'wrong token' });
  for (const p of ['merge-proposals/1/approve', 'split-proposals/1/approve', 'merge-proposals/1/dismiss', 'split-proposals/1/dismiss']) {
    const x = await api(page, 'POST', `/api/items/${a.number}/${p}`, { approved: true });
    expect(x.status, p).toBe(403);
    expect(x.json).toEqual({ ok: false, error: 'only the owner can do this' });
  }
});

// Mail cannot be put in from a browser test, and joins and splits act on mail only.
test.fixme('10: a join proposal shows the chip `Join <number> into this row` with Approve and Dismiss on the row that stays, and moves nothing until approved', async () => {
  // Needs two mail rows.
});
test.fixme('10: an approved join moves everything into the row that stays, the old number leads there and the row says `<agent> joined <number> into this row.`', async () => {
  // Needs two mail rows.
});
test.fixme('10: a split proposal takes one mail thread out of a joined row into a row of its own once approved, and nothing is lost', async () => {
  // Needs a row that joined several mail threads.
});
test.fixme('10: mail whose subject or text carries the same reference as an approved pattern joins one row', async () => {
  // Needs separate mails with the same reference.
});
test.fixme('10: mail with a similar sender or subject but no common reference never joins', async () => {
  // Needs mail; the chat form of this check lives in thread.spec.ts.
});
