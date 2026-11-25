// Acceptance tests for apps/one/SPEC.md points 13 (replies) and 19 (to-dos).
//
// Replies are tested on the test's own invented chat item only. A reply that is approved is withdrawn before
// the test ends, and the test never calls /api/bridge/outbox (it would hand out real replies).
// Not tested here, and why:
//  - 13 the results `Sent <time>.` and `Could not send: <reason>` and the `Could not send` notification:
//    they need the bridge to fetch the reply, which would hand out the owner's real replies too.
//  - 13 Gmail and Office mail replies and the `Blocked targets` refusal: mail cannot be put in from a
//    browser test, and API.md does not name a chat handle that is on the blocked list.
//  - 19 `Nothing to do.`: needs an empty to-do list; a preview holds the owner's real to-dos.
import { expect, type Locator, type Page } from '@playwright/test';
import {
  test, mkr, isPhone, go, api, ready, addMsg, act, btn, openRow, showRow, rowBox, noteBox, seen, openAgenda,
  toggleOf, isOpen, press, settle, tool, type Chat, expectRefusal
} from './one-helpers';

async function propose(page: Page, c: Chat, text: string) {
  const r = await api(page, 'POST', `/api/items/${c.number}/reply-proposals`, { text, reason: 'e2e ' + c.mk });
  expect(r.status, r.text.slice(0, 300)).toBe(200);
  return r;
}

/** The box that holds the reply text: the one editable field whose value is this text. */
async function replyBox(row: Locator, text: string): Promise<Locator> {
  const boxes = row.locator('textarea, input[type=text]');
  await expect.poll(async () => {
    for (const b of await boxes.all()) if ((await b.inputValue().catch(() => '')) === text) return true;
    return false;
  }).toBe(true);
  for (const b of await boxes.all()) if ((await b.inputValue().catch(() => '')) === text) return b;
  throw new Error('no reply box');
}

async function approveReply(page: Page, row: Locator) {
  if (!isPhone(page)) { await act(row, 'Approve'); return; }
  {
    await btn(row, 'Approve').click(); // on a phone this only asks once more: nothing is sent yet
    const ask = page.getByText('Send this reply?').first();
    await expect(ask).toBeVisible();
    await ask.locator('xpath=ancestor::*[.//button[normalize-space()="Cancel"]][1]').getByRole('button', { name: 'Send', exact: true }).click();
  }
}

// ------------------------------------------------------------------ 13

test('13: a reply proposal shows the exact text in an editable box, where it goes, Approve and Discuss', async ({ page }) => {
  const c = await ready(page);
  const text = `Thanks, see you then ${c.mk}`;
  await propose(page, c, text);
  const row = await openRow(page, c);
  await expect(seen(row, /Will be sent · reply in WhatsApp/)).toBeVisible();
  const box = await replyBox(row, text);
  await box.fill(text + ' edited');
  await expect(box).toHaveValue(text + ' edited');
  await expect(btn(row, 'Approve')).toBeVisible();
  await expect(btn(row, 'Discuss')).toBeVisible();
  await btn(row, 'Discuss').click();
  await expect(noteBox(row)).toBeFocused();
});

test('13: Approve sends what the box shows, keeps both texts, and a chat reply waits for the Mac until withdrawn', async ({ page }) => {
  const c = await ready(page);
  const text = `Original words ${c.mk}`, edited = `Edited words ${c.mk}`;
  await propose(page, c, text);
  const row = await openRow(page, c);
  const box = await replyBox(row, text);
  await box.fill(edited);
  await approveReply(page, row);
  await expect(seen(row, 'Waiting for the Mac to send this.')).toBeVisible();
  const ev = await api(page, 'GET', `/api/items/${c.number}/events`);
  expect(ev.text, 'the original text is kept').toContain(text);
  expect(ev.text, 'the edited text is kept').toContain(edited);
  await btn(row, 'Withdraw').click();
  await expect(row.getByText('Waiting for the Mac to send this.').locator('visible=true')).toHaveCount(0);
});

test('13: on a phone Approve asks once more, and Cancel sends nothing', async ({ page }) => {
  test.skip(!isPhone(page), 'the second question is for a phone');
  const c = await ready(page);
  const text = `Phone reply ${c.mk}`;
  await propose(page, c, text);
  const row = await openRow(page, c);
  await btn(row, 'Approve').click(); // only asks once more
  const ask = page.getByText('Send this reply?').first();
  await expect(ask).toBeVisible();
  const dialog = ask.locator('xpath=ancestor::*[.//button[normalize-space()="Cancel"]][1]');
  await dialog.getByRole('button', { name: 'Cancel', exact: true }).click();
  await expect(row.getByText('Waiting for the Mac to send this.').locator('visible=true')).toHaveCount(0);
  await approveReply(page, row);
  await expect(seen(row, 'Waiting for the Mac to send this.')).toBeVisible();
  await btn(row, 'Withdraw').click();
});

test('13: on a laptop Approve sends without asking, and pressing it twice sends once', async ({ page }) => {
  test.skip(isPhone(page), 'a laptop only');
  const c = await ready(page);
  const text = `Laptop reply ${c.mk}`;
  await propose(page, c, text);
  const row = await openRow(page, c);
  await btn(row, 'Approve').dblclick();
  await expect(page.getByText('Send this reply?')).toHaveCount(0);
  await expect(seen(row, 'Waiting for the Mac to send this.')).toBeVisible();
  await expect(row.getByText('Waiting for the Mac to send this.').locator('visible=true')).toHaveCount(1);
  await expect(row.getByRole('button', { name: 'Withdraw', exact: true }).locator('visible=true')).toHaveCount(1);
  await btn(row, 'Withdraw').click();
});

test('13: a new reply takes the place of the one waiting, and the same text twice answers the waiting one', async ({ page }) => {
  const c = await ready(page);
  const a = `First draft ${c.mk}`, b = `Second draft ${c.mk}`;
  await propose(page, c, a);
  const again = await propose(page, c, a);
  expect(again.json.unchanged).toBe(true);
  await propose(page, c, b);
  const row = await openRow(page, c);
  await replyBox(row, b);
  await expect(row.locator('textarea, input[type=text]').evaluateAll((els, t) =>
    els.filter((e) => (e as HTMLInputElement).value === t).length, a)).resolves.toBe(0);
});

test('13: a new message withdraws the waiting reply', async ({ page }) => {
  const c = await ready(page);
  await propose(page, c, `A reply ${c.mk}`);
  await addMsg(page, c, { text: `wait, one more thing ${c.mk}`, minsAgo: 45 });
  await api(page, 'POST', `/api/items/${c.number}/seen`, {});
  const row = await openRow(page, c);
  await expect(seen(row, 'Withdrawn')).toBeVisible();
});

test('13: no reply is proposed to a message that is the owner\'s own, to a closed item, or with recipients', async ({ page }) => {
  const own = await ready(page);
  await addMsg(page, own, { text: `my own words ${own.mk}`, own: true, minsAgo: 30 });
  const same = await api(page, 'POST', `/api/items/${own.number}/reply-proposals`, { text: 'x', reason: 'e2e' });
  expect(same.status).toBe(403);
  expectRefusal(same.json.error, 'Same thread');
  const c = await ready(page);
  const withTo = await api(page, 'POST', `/api/items/${c.number}/reply-proposals`, { text: 'x', reason: 'e2e', to: 'someone@example.com' });
  expect(withTo.status).toBe(400);
  expect((await api(page, 'POST', `/api/items/${c.number}/skip`, { reason: 'e2e ' + c.mk, kind: 'finished' })).status).toBe(200);
  const closed = await api(page, 'POST', `/api/items/${c.number}/reply-proposals`, { text: 'x', reason: 'e2e' });
  expect(closed.status).toBe(409);
});

// ------------------------------------------------------------------ 19

const todoRow = (side: Locator, text: string): Locator =>
  side.getByText(text).first().locator('xpath=ancestor::*[.//button[normalize-space()="Done"]][1]');

async function todos(page: Page): Promise<any[]> {
  const r = await api(page, 'GET', '/api/agenda');
  expect(r.status).toBe(200);
  return r.json.todos ?? [];
}

test('19: The owner adds a to-do by hand, an empty box adds nothing, and Done ticks it off', async ({ page }) => {
  const mk = mkr();
  const side = await openAgenda(page);
  await expect(side.getByText('To do', { exact: true }).first()).toBeVisible();
  const before = (await todos(page)).length;
  await side.getByRole('button', { name: 'Add', exact: true }).click();
  expect((await todos(page)).length, 'an empty box adds nothing').toBe(before);
  const box = side.getByLabel('Add a to-do');
  await box.fill(`Buy a thing ${mk}`);
  await side.getByRole('button', { name: 'Add', exact: true }).click();
  await expect(side.getByText(`Buy a thing ${mk}`)).toBeVisible();
  expect(JSON.stringify(await todos(page))).toContain(mk);
  await todoRow(side, `Buy a thing ${mk}`).getByRole('button', { name: 'Done', exact: true }).click();
  await expect(side.getByText(`Buy a thing ${mk}`)).toHaveCount(0);
  expect(JSON.stringify(await todos(page))).not.toContain(mk);
});

test('19: a to-do made from a row shows the row\'s number, and the two are separate', async ({ page }) => {
  const c = await ready(page);
  const row = await openRow(page, c);
  await act(row, 'To do');
  await expect(seen(row, 'You made this a to-do.')).toBeVisible();
  const side = await openAgenda(page);
  const item = side.getByText(c.number).first();
  await expect(item).toBeVisible();
  // closing the item does not tick the to-do
  const r = await openRow(page, c);
  await act(r, 'Resolve');
  const side2 = await openAgenda(page);
  await expect(side2.getByText(c.number).first()).toBeVisible();
  // ticking the to-do does not reopen the item
  await todoRow(side2, c.number).getByRole('button', { name: 'Done', exact: true }).click();
  await expect(side2.getByText(c.number)).toHaveCount(0);
  expect((await api(page, 'GET', `/api/items/${c.number}`)).json.item.stage).toBe('resolved');
});

test('19: a to-do made from a row opens that row when tapped', async ({ page }) => {
  const c = await ready(page);
  const row = await openRow(page, c);
  await act(row, 'To do');
  const side = await openAgenda(page);
  await side.getByText(c.number).first().click();
  await expect.poll(async () => {
    if (isPhone(page)) return true; // the agenda is a view of its own; closing it is tested elsewhere
    return isOpen(toggleOf(page, c));
  }).toBe(true);
  await go(page, '/');
  const side2 = await openAgenda(page);
  await todoRow(side2, c.number).getByRole('button', { name: 'Done', exact: true }).click();
});

test('19: a to-do proposed by an agent is added only after Approve', async ({ page }) => {
  const c = await ready(page);
  const text = `Call back ${c.mk}`;
  const r = await api(page, 'POST', '/api/agenda/todos', { item: c.number, text, reason: 'e2e ' + c.mk });
  expect(r.status, r.text.slice(0, 300)).toBe(200);
  expect((await api(page, 'POST', '/api/agenda/todos', { item: c.number, text, reason: 'e2e' })).json.unchanged).toBe(true);
  expect(JSON.stringify(await todos(page))).not.toContain(c.mk);
  const t = await showRow(page, c);
  await act(rowBox(t), 'Approve');
  await expect.poll(async () => JSON.stringify(await todos(page))).toContain(c.mk);
  const side = await openAgenda(page);
  await todoRow(side, text).getByRole('button', { name: 'Done', exact: true }).click();
  await expect.poll(async () => JSON.stringify(await todos(page))).not.toContain(c.mk);
});

test('19: on a phone the list has a To do group with its count, folded like the stages, and it opens to the box to add one and Done', async ({ page }) => {
  test.skip(!isPhone(page), 'the To do group stands with the stages on a phone; a laptop has the to-dos in the agenda');
  const mk = mkr();
  await ready(page); // an app with nothing in it shows no groups at all
  await go(page, '/');
  await settle(page);
  const head = page.getByRole('button', { name: /^[^A-Za-z]*To do\b/ }).first();
  await expect(head).toBeVisible();
  await expect(head).toContainText(/\d/);
  await expect(page.getByLabel('Add a to-do').locator('visible=true'), 'folded: the box is not shown').toHaveCount(0);
  await head.click();
  const box = page.getByLabel('Add a to-do').locator('visible=true').first();
  await expect(box).toBeVisible();
  await box.fill(`Phone to-do ${mk}`);
  await press(page, page.getByRole('button', { name: 'Add', exact: true }).locator('visible=true').first());
  await go(page, '/');
  await settle(page);
  await page.getByRole('button', { name: /^[^A-Za-z]*To do\b/ }).first().click();
  const t = page.getByText(`Phone to-do ${mk}`).locator('visible=true').first();
  await expect(t).toBeVisible();
  await press(page, t.locator('xpath=ancestor::*[.//button[normalize-space()="Done"]][1]').getByRole('button', { name: 'Done', exact: true }).first());
  expect(JSON.stringify(await todos(page))).not.toContain(mk);
});

test('19: in the phone\'s agenda the to-dos come first, above the events', async ({ page }) => {
  test.skip(!isPhone(page), 'the phone\'s agenda is a view of its own');
  const side = await openAgenda(page);
  const todo = side.getByText('To do', { exact: true }).first();
  const events = side.getByText(/^(Today|Tomorrow|No events\.)/).first();
  await expect(todo).toBeVisible();
  await expect(events).toBeVisible();
  expect((await todo.boundingBox())!.y, 'to-dos above the events').toBeLessThan((await events.boundingBox())!.y);
});

test('19: an agent can propose a to-do that belongs to no item, and it shows in the agenda with its reason, Approve and Dismiss', async ({ page }) => {
  const mk = mkr();
  const text = `Loose to-do ${mk}`;
  const r = await api(page, 'POST', '/api/agenda/todos', { text, reason: `because ${mk}` });
  expect(r.status, r.text.slice(0, 300)).toBe(200);
  expect(JSON.stringify(await todos(page)), 'nothing is added before Approve').not.toContain(mk);
  const side = await openAgenda(page);
  const card = side.getByText(text).first().locator('xpath=ancestor::*[.//button[normalize-space()="Approve"]][1]');
  await expect(card).toContainText(`because ${mk}`);
  await expect(card.getByRole('button', { name: 'Dismiss', exact: true })).toBeVisible();
  await press(page, card.getByRole('button', { name: 'Dismiss', exact: true }));
  expect(JSON.stringify(await todos(page)), 'Dismiss adds nothing').not.toContain(mk);
});

test('19: an agent can propose to tick off a to-do, and it shows in the agenda with its reason, Approve and Dismiss, and changes nothing before Approve', async ({ page }) => {
  const mk = mkr();
  const text = `Tidy me ${mk}`;
  expect((await api(page, 'POST', '/api/agenda/todos', { text, reason: `new ${mk}` })).status).toBe(200);
  let side = await openAgenda(page);
  await press(page, side.getByText(text).first().locator('xpath=ancestor::*[.//button[normalize-space()="Approve"]][1]').getByRole('button', { name: 'Approve', exact: true }));
  const mine = (await todos(page)).find((t: any) => JSON.stringify(t).includes(mk));
  expect(mine, 'the approved to-do is in the list').toBeTruthy();
  const why = `done already ${mk}`;
  const r = await tool(page, 'update_agenda', { id: mine.id, change: 'done', reason: why });
  expect(r.isError, r.text).toBe(false);
  expect(JSON.stringify(await todos(page)), 'not ticked before Approve').toContain(mk);
  side = await openAgenda(page);
  const card = side.getByText(why).first().locator('xpath=ancestor::*[.//button[normalize-space()="Approve"]][1]');
  await expect(card.getByRole('button', { name: 'Dismiss', exact: true })).toBeVisible();
  await press(page, card.getByRole('button', { name: 'Approve', exact: true }));
  await expect.poll(async () => JSON.stringify(await todos(page))).not.toContain(mk);
});

function sidebarOf(page: Page): Locator {
  return page.getByRole('complementary', { name: 'Agenda' })
    .or(page.getByRole('dialog', { name: 'Agenda' }))
    .or(page.getByRole('region', { name: 'Agenda' })).first();
}
void press;
