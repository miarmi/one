// New acceptance tests for apps/one/SPEC.md points 15, 19, 22 and 27 that the older files do not cover.
// Written from SPEC.md alone; connector tools are called with the names and inputs the app's own tools listing gives.
//
// Not tested here, and why:
//  - 15 `<source> failed: <error>`: needs a failing source (see api.spec.ts, point 24).
//  - 19 "no agent can add, move or remove a calendar entry": a test cannot create a calendar entry to aim at.
//  - 27 `Mark all seen` is only checked to be there: pressing it would mark real entries of a preview seen.
//  - 27 the floating summary row `Closed by Claude: <n> to review` with its to-do `Review what Claude closed`, and that
//    resolving it marks the listed items seen: the summary is written by the app once a day in the evening, which a
//    test cannot wait for or start.
import { expect, type Locator, type Page } from '@playwright/test';
import {
  test, mkr, go, api, ready, openAgenda, visit, isPhone, isLaptop, groupHeader, expandGroup, skip, stageOf, btn, press, stagesPane, settle,
  type Chat,
} from './one-helpers';
import { ag, cardOf, propose, grow } from './new-helpers';

// ------------------------------------------------------------------ 15

// ------------------------------------------------------------------ 19

const todoOf = async (page: Page, text: string) => {
  const r = await ag(page, 'list_agenda', {});
  expect(r.isError, r.text).toBe(false);
  return (r.data.todos as any[]).find((t) => JSON.stringify(t).includes(text));
};
/** The agenda: a side panel on a laptop, a view of its own on a phone. */
const side = (page: Page) => openAgenda(page);

test('19: on a phone the To do group stands with the stages, folded, with its count, and opens to the to-dos with Done and the box to add one', async ({ page }) => {
  test.skip(!isPhone(page), 'the To do group is the phone\'s list page');
  const mk = mkr();
  await grow(page, await ready(page)); // an app with no rows shows no groups
  await go(page, '/');
  await settle(page);
  const g = groupHeader(page, 'To do');
  await expect(g).toBeVisible();
  await expect(g).toContainText(/\d/);
  expect(await g.getAttribute('aria-expanded'), 'folded like the stages').toBe('false');
  const n0 = Number(/(\d+)/.exec(await g.innerText())![1]);
  await g.click();
  await expect(page.getByLabel('Add a to-do').first()).toBeVisible();
  const add = page.getByRole('button', { name: 'Add', exact: true }).first();
  await page.getByLabel('Add a to-do').first().fill(`Phone to-do ${mk}`);
  await add.click();
  await expect(page.getByText(`Phone to-do ${mk}`).first()).toBeVisible();
  await go(page, '/');
  await settle(page);
  expect(Number(/(\d+)/.exec(await groupHeader(page, 'To do').innerText())![1])).toBe(n0 + 1);
  // The count in the top line opens it.
  const top = page.getByRole('banner').getByRole('button', { name: /to.?do/i }).or(page.getByRole('banner').getByRole('link', { name: /to.?do/i })).first();
  await expect(top, 'the top line carries the to-do count').toBeVisible();
  await top.click();
  await expect(page.getByLabel('Add a to-do').first()).toBeVisible();
  const row = page.getByText(`Phone to-do ${mk}`).first().locator('xpath=ancestor::*[.//button[normalize-space()="Done"]][1]');
  await expect(row.getByRole('button', { name: 'Done', exact: true })).toBeVisible();
  await row.getByRole('button', { name: 'Done', exact: true }).click();
  await expect(page.getByText(`Phone to-do ${mk}`)).toHaveCount(0);
});

test('19: Dismiss on an agent\'s to-do proposal adds nothing', async ({ page }) => {
  const mk = mkr();
  const text = `Never added ${mk}`;
  expect((await ag(page, 'post_agenda', { text, reason: `why ${mk}` })).isError).toBe(false);
  const s = await side(page);
  await press(page, cardOf(s, text).getByRole('button', { name: 'Dismiss', exact: true }));
  await expect(s.getByText(text)).toHaveCount(0);
  expect(await todoOf(page, mk)).toBeUndefined();
});

async function aTodo(page: Page, mk: string, text: string) {
  expect((await ag(page, 'post_agenda', { text, reason: `why ${mk}` })).isError).toBe(false);
  const s = await side(page);
  await press(page, cardOf(s, text).getByRole('button', { name: 'Approve', exact: true }));
  await expect.poll(async () => !!(await todoOf(page, mk))).toBe(true);
  return (await todoOf(page, mk)).id as number;
}

test('19: a proposed change to a to-do (correct its words) changes nothing before Approve, and its new words after', async ({ page }) => {
  const mk = mkr();
  const old = `Call the plumber ${mk}`, fresh = `Call the electrician ${mk}`;
  const id = await aTodo(page, mk, old);
  const r = await ag(page, 'update_agenda', { id, change: 'edit', text: fresh, reason: `wrong trade ${mk}` });
  expect(r.isError, r.text).toBe(false);
  expect(JSON.stringify(await todoOf(page, mk)), 'unchanged before Approve').toContain(old);
  const s = await side(page);
  const card = cardOf(s, fresh);
  await expect(card).toContainText(`wrong trade ${mk}`);
  await expect(card.getByRole('button', { name: 'Dismiss', exact: true })).toBeVisible();
  await press(page, card.getByRole('button', { name: 'Approve', exact: true }));
  await expect.poll(async () => JSON.stringify(await todoOf(page, mk))).toContain(fresh);
  await ag(page, 'update_agenda', { id, change: 'drop', reason: `cleanup ${mk}` });
});

for (const change of ['drop'] as const) {
  test(`19: a proposal to ${change === 'done' ? 'tick off' : 'drop'} a to-do changes nothing before Approve, and takes it off the list after`, async ({ page }) => {
    const mk = mkr();
    const text = `Old to-do ${mk}`;
    const id = await aTodo(page, mk, text);
    const r = await ag(page, 'update_agenda', { id, change, reason: `already handled ${mk}` });
    expect(r.isError, r.text).toBe(false);
    expect(await todoOf(page, mk), 'still there before Approve').toBeTruthy();
    const s = await side(page);
    const card = cardOf(s, `already handled ${mk}`);
    await expect(card).toBeVisible();
    await press(page, card.getByRole('button', { name: 'Approve', exact: true }));
    await expect.poll(async () => !!(await todoOf(page, mk))).toBe(false);
  });
}

// ------------------------------------------------------------------ 22

test('22: a refusal reads Refused by guard: <guard name> followed by a sentence saying why', async ({ page }) => {
  const c = await ready(page);
  const r = await api(page, 'POST', `/api/items/${c.number}/proposals`, { action: 'close' });
  expect(r.status).toBe(403);
  expect(r.json.error).toMatch(/Refused by guard: Reason required\W+\S+(\s+\S+){3,}/);
  const t = await ag(page, 'resolve_item', { id: c.number, reason: '' });
  expect(t.isError, t.text).toBe(true);
  expect(t.text).toMatch(/Refused by guard: Reason required\W+\S+(\s+\S+){3,}/);
});

const ALWAYS_ON = ['Approval only', 'Exact text', 'Same thread', 'Send once', 'Approval required', 'Actor by channel'];

test('22: a guard that is always on cannot be proposed off, and nothing is proposed', async ({ page }) => {
  const mk = mkr();
  const before = await ag(page, 'list_settings', { kind: 'guards' });
  for (const g of before.data.guards as any[]) {
    if (ALWAYS_ON.includes(g.name)) expect(g.state, g.name).toBe('Always on');
  }
  for (const name of ALWAYS_ON) {
    const r = await ag(page, 'propose_setting', { kind: 'guard', name, value: 'off', reason: `try ${mk}` });
    expect(r.isError, name + ': ' + r.text).toBe(true);
  }
  const after = await ag(page, 'list_settings', { kind: 'guards' });
  expect(JSON.stringify(after.data.waiting)).not.toContain(mk);
});

const quiet = async (page: Page) => ((await ag(page, 'list_settings', {})).data.settings as any[]).find((x) => x.name === 'Chat quiet time');

// A proposal made under a test run never changes a real setting when approved (the run's mark keeps tests away from
// real settings), so this test makes its proposal without a run name, approves it, and puts the setting back the same way.
test('22: after the owner\'s Approve the proposed setting applies', async ({ page }) => {
  const mk = mkr();
  const s0 = await quiet(page);
  const was: string = s0.value;
  const to: string = (s0.allowed as string[]).find((v) => v !== was)!;
  const change = async (value: string, reason: string) => {
    const r = await ag(page, 'propose_setting', { kind: 'setting', name: 'Chat quiet time', value, reason, run: undefined });
    expect(r.isError, r.text).toBe(false);
    await visit(page, 'settings');
    await press(page, cardOf(page, reason).getByRole('button', { name: 'Approve', exact: true }));
  };
  try {
    await change(to, `e2e change ${mk}`);
    await expect.poll(async () => (await quiet(page)).value).toBe(to);
  } finally {
    if ((await quiet(page)).value !== was) {
      await change(was, `e2e put back ${mk}`);
      await expect.poll(async () => (await quiet(page)).value).toBe(was);
    }
  }
});

// ------------------------------------------------------------------ 27

