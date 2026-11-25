// Acceptance tests for apps/one/SPEC.md point 17, second paragraph: The owner answers in a chat himself, directly,
// attachments included: the box, its placeholder, Enter and Shift+Enter, Send, Attach, going out, and what no agent
// can do about it. Also point 7's limits as they apply to this box.
//
// Only the test's own invented chats are written in, and the test never calls /api/bridge/outbox (it would hand out
// The owner's real messages to the test). So the states `Sent <time>.` and `Could not send: <reason>` cannot be reached
// here. Every message the owner writes is withdrawn where the page offers it, and the run is removed at the end.
// Not tested here, and why:
//  - 17 "this box is only on chats; mail rows have no such box": mail cannot be put in from a browser test.
//  - 17 `Sent <time>.`, `Could not send: <reason>`: need the bridge to take the message (see above).
//  - 17 `Waiting for the Mac to send this.` with `Withdraw` appears only while the Mac has not been heard from; a
//    test that posts its own chat through the bridge has just been heard, so it is checked only when it shows.
import { expect, type Locator, type Page } from '@playwright/test';
import {
  test, mkr, isPhone, readyBare, go, api, ready, openRow, btn, seen, noteBox, chatBox, chatForm, waitsForClaude, chooseFile, stageOf,
  token, itemOf, type Chat, noteForm, toggleOf, settle, GROUPS, expandGroup, moreUntil,
} from './one-helpers';

const OUT = /Sending…|Waiting for the Mac to send this\./;

const msgsOf = (row: Locator, c: Chat): Locator => row.getByRole('list', { name: `Messages of ${c.number}` });
// one message of the chat (a message that carries a file has a list of its own inside, which is not a message)
const entry = (msgs: Locator, text: string | RegExp): Locator => msgs.locator(':scope > li').filter({ hasText: text });

async function setup(page: Page, net = 'messenger', label = 'Messenger') {
  const c = await ready(page, { net });
  const row = await openRow(page, c);
  return { c, row, box: chatBox(row, label), form: chatForm(row, label), msgs: msgsOf(row, c), label };
}

/** If the message is waiting for the Mac, withdraw it, as the spec allows. */
async function withdrawIfWaiting(row: Locator) {
  const w = row.getByRole('button', { name: 'Withdraw', exact: true }).locator('visible=true');
  for (let i = 0; i < 6 && (await w.count()); i++) {
    // the page may redraw after a withdrawal: a button that has gone meanwhile is not an error
    const answered = row.page().waitForResponse((r) => r.request().method() !== 'GET', { timeout: 5000 }).catch(() => null);
    await w.first().click({ timeout: 3000 }).catch(() => {});
    await answered;
    await row.page().waitForLoadState('load').catch(() => {});
  }
}

test('17: a chat row has a box to write into that chat, named after the network, with Send and Attach', async ({ page }) => {
  test.skip(isPhone(page), 'on a phone there is no such box: see the next test');
  for (const [net, label] of [['messenger', 'Messenger'], ['whatsapp', 'WhatsApp'], ['imessage', 'iMessage'], ['sms', 'SMS']]) {
    const { row, box, form } = await setup(page, net, label);
    await expect(box, label).toBeVisible();
    await expect(box, label).toHaveAttribute('placeholder', `Write in ${label}…`);
    await expect(form.getByRole('button', { name: 'Send', exact: true }), label).toBeVisible();
    await expect(form.getByRole('button', { name: 'Attach', exact: true }), label).toBeVisible();
    // it is a different box from the note box, which belongs to the thread
    await expect(noteBox(row)).toBeVisible();
    expect(await noteBox(row).getAttribute('placeholder')).toBe('Add a note or ask Claude…');
  }
});

test('17: on a phone a chat row shows its messages and the note box for Claude, and no box to write into the chat', async ({ page }) => {
  test.skip(!isPhone(page), 'this is the phone half of the rule: a laptop has the box');
  for (const [net, label] of [['messenger', 'Messenger'], ['whatsapp', 'WhatsApp']]) {
    const c = await ready(page, { net });
    const row = await openRow(page, c);
    await expect(noteBox(row)).toBeVisible();
    await expect(msgsOf(row, c)).toBeVisible();
    await expect(chatBox(row, label).locator('visible=true'), label).toHaveCount(0);
    await expect(row.getByPlaceholder(/^Write in /).locator('visible=true'), label).toHaveCount(0);
    // the one bar of the open row has the note box's Send and Attach, but no `Send` that writes into the chat
    await expect(noteForm(row).getByRole('button', { name: 'Send', exact: true })).toBeVisible();
  }
});

/** Is the chat's row in the list, looked at as it is (nothing is added to the chat to make it show)? */
async function listedNow(page: Page, c: Chat): Promise<boolean> {
  await go(page, '/');
  await settle(page);
  const t = toggleOf(page, c);
  for (const g of GROUPS) {
    await expandGroup(page, g);
    if (await moreUntil(page, t, 1200)) return true;
  }
  return false;
}

test('17: a phone lists a chat only when its thread holds something besides the chat\'s own messages; a laptop lists every chat', async ({ page }) => {
  const mine = await readyBare(page, { net: 'messenger' });
  if (isPhone(page)) {
    expect(await listedNow(page, mine), 'a chat nobody commented on is not listed on a phone').toBe(false);
    // a comment from Claude makes it listed
    expect((await api(page, 'POST', `/api/items/${mine.number}/comments`, { text: `phone word ${mine.mk}` })).status).toBe(200);
    expect(await listedNow(page, mine)).toBe(true);
    // so does a proposal
    const other = await readyBare(page, { net: 'whatsapp' });
    expect(await listedNow(page, other)).toBe(false);
    expect((await api(page, 'POST', `/api/items/${other.number}/proposals`, { action: 'pending', reason: `e2e ${other.mk}` })).status).toBe(200);
    expect(await listedNow(page, other)).toBe(true);
  } else {
    expect(await listedNow(page, mine), 'a laptop lists every chat').toBe(true);
  }
});

test('17: the box is under the chat\'s messages, not among the thread\'s entries', async ({ page }) => {
  test.skip(isPhone(page), 'the box to write into a chat is for a laptop only (point 17)');
  const { row, box, msgs } = await setup(page);
  const m = (await msgs.boundingBox())!, b = (await box.boundingBox())!;
  expect(b.y, 'the box is under the messages').toBeGreaterThan(m.y + m.height - 4);
});

test('17: Enter sends what is written, it shows in the chat at once as going out, and nothing needs Approve', async ({ page }) => {
  test.skip(isPhone(page), 'the box to write into a chat is for a laptop only (point 17)');
  const { c, row, box, msgs } = await setup(page);
  const text = `Hi from the owner ${c.mk}`;
  await box.fill(text);
  await box.press('Enter');
  await expect(box, 'the box is empty again').toHaveValue('');
  const mine = entry(msgs, text);
  await expect(mine).toHaveCount(1);
  await expect(mine).toContainText(OUT);
  await expect(btn(row, 'Approve')).toHaveCount(0);
  await page.reload();
  const again = await openRow(page, c);
  await expect(entry(msgsOf(again, c), text), 'it is kept').toHaveCount(1);
  await withdrawIfWaiting(again);
});

test('17: Shift and Enter make a new line and send nothing', async ({ page }) => {
  test.skip(isPhone(page), 'the box to write into a chat is for a laptop only (point 17)');
  const { c, row, box, msgs } = await setup(page);
  await box.fill(`top ${c.mk}`);
  await box.press('Shift+Enter');
  await box.pressSequentially(`bottom ${c.mk}`);
  expect(await box.inputValue()).toBe(`top ${c.mk}\nbottom ${c.mk}`);
  await expect(msgs.getByText(`top ${c.mk}`)).toHaveCount(0);
  await box.press('Enter');
  await expect(box).toHaveValue('');
  await expect(entry(msgs, `bottom ${c.mk}`)).toHaveCount(1);
  await expect(entry(msgs, `top ${c.mk}`)).toHaveCount(1);
  await withdrawIfWaiting(row);
});

test('17: Send sends too, an empty box sends nothing, and a double press sends once', async ({ page }) => {
  test.skip(isPhone(page), 'the box to write into a chat is for a laptop only (point 17)');
  const { c, row, form, box, msgs } = await setup(page);
  const before = await msgs.locator(':scope > li').count();
  await form.getByRole('button', { name: 'Send', exact: true }).click();
  await page.waitForTimeout(800);
  expect(await msgs.locator(':scope > li').count(), 'an empty box writes nothing').toBe(before);
  await box.fill(`once ${c.mk}`);
  await form.getByRole('button', { name: 'Send', exact: true }).dblclick();
  await expect(entry(msgs, `once ${c.mk}`)).toHaveCount(1);
  await page.waitForTimeout(800);
  await expect(entry(msgs, `once ${c.mk}`), 'it goes out once').toHaveCount(1);
  await withdrawIfWaiting(row);
});

test('17: a message the owner writes never makes the chat wait for Claude', async ({ page }) => {
  test.skip(isPhone(page), 'the box to write into a chat is for a laptop only (point 17)');
  const { c, row, box } = await setup(page);
  expect(await waitsForClaude(page, c)).toBe(false);
  await box.fill(`not for Claude ${c.mk}`);
  await box.press('Enter');
  await expect(entry(msgsOf(row, c), `not for Claude ${c.mk}`)).toHaveCount(1);
  expect(await waitsForClaude(page, c), 'not queued for Claude').toBe(false);
  expect(['new', 'in_progress', 'pending']).toContain(await stageOf(page, c));
  await withdrawIfWaiting(row);
});

test('17: while the message is waiting for the Mac it says so and can be withdrawn', async ({ page }) => {
  test.skip(isPhone(page), 'the box to write into a chat is for a laptop only (point 17)');
  const { c, row, box, msgs } = await setup(page);
  await box.fill(`maybe waiting ${c.mk}`);
  await box.press('Enter');
  const mine = entry(msgs, `maybe waiting ${c.mk}`);
  await expect(mine).toContainText(OUT);
  if (await mine.getByText('Waiting for the Mac to send this.').count()) {
    await expect(mine.getByRole('button', { name: 'Withdraw', exact: true })).toBeVisible();
    await mine.getByRole('button', { name: 'Withdraw', exact: true }).click();
    await expect(mine.getByText('Waiting for the Mac to send this.')).toHaveCount(0);
    await expect(mine.getByRole('button', { name: 'Withdraw', exact: true })).toHaveCount(0);
  } else {
    test.info().annotations.push({ type: 'note', description: 'the bridge had just been heard from: the message showed Sending…, not the waiting state' });
  }
  await withdrawIfWaiting(row);
});

test('17: The owner attaches files in the chat: each file goes as its own message after the text', async ({ page }) => {
  test.skip(isPhone(page), 'the box to write into a chat is for a laptop only (point 17)');
  const { c, row, box, form, msgs } = await setup(page);
  const a = `first-${c.mk}.txt`, b = `second-${c.mk}.txt`;
  await chooseFile(page, form, 'Attach', [
    { name: a, mimeType: 'text/plain', buffer: Buffer.from('aaa') },
    { name: b, mimeType: 'text/plain', buffer: Buffer.from('bbbb') },
  ]);
  await box.fill(`with two files ${c.mk}`);
  await form.getByRole('button', { name: 'Send', exact: true }).click();
  await expect(entry(msgs, `with two files ${c.mk}`)).toHaveCount(1);
  const fileA = entry(msgs, a), fileB = entry(msgs, b);
  await expect(fileA, 'the first file is its own message').toHaveCount(1);
  await expect(fileB, 'the second file is its own message').toHaveCount(1);
  await expect(fileA.first()).not.toContainText(`with two files ${c.mk}`);
  await expect(fileA.first()).not.toContainText(b);
  await expect(row.getByText('Attachments', { exact: true }).locator('visible=true').first()).toBeVisible();
  await withdrawIfWaiting(row);
});

test('17: at most 5 files go with one message', async ({ page }) => {
  test.skip(isPhone(page), 'the box to write into a chat is for a laptop only (point 17)');
  const { c, row, box, form, msgs } = await setup(page);
  const files = Array.from({ length: 6 }, (_, i) => ({ name: `many${i}-${c.mk}.txt`, mimeType: 'text/plain', buffer: Buffer.from('x' + i) }));
  await chooseFile(page, form, 'Attach', files);
  await box.fill(`six files ${c.mk}`);
  await form.getByRole('button', { name: 'Send', exact: true }).click();
  await page.waitForTimeout(1500);
  let shown = 0;
  for (const f of files) shown += await entry(msgs, f.name).count();
  expect(shown, 'no more than five of the six go').toBeLessThanOrEqual(5);
  await withdrawIfWaiting(row);
});

test('17: a file over 20 MB is refused', async ({ page }) => {
  test.skip(isPhone(page), 'the box to write into a chat is for a laptop only (point 17)');
  test.setTimeout(120_000);
  const { c, row, box, form, msgs } = await setup(page);
  const name = `big-${c.mk}.bin`;
  await chooseFile(page, form, 'Attach', { name, mimeType: 'application/octet-stream', buffer: Buffer.alloc(21 * 1024 * 1024, 97) });
  await box.fill(`too big ${c.mk}`);
  await form.getByRole('button', { name: 'Send', exact: true }).click();
  await page.waitForTimeout(3000);
  await expect(entry(msgs, name), 'the big file did not go').toHaveCount(0);
  await withdrawIfWaiting(row);
});

test('17: nothing an agent does can put text into this box or send from it', async ({ page }) => {
  test.skip(isPhone(page), 'the box to write into a chat is for a laptop only (point 17)');
  const { c, row, box, msgs } = await setup(page);
  const word = `agent words ${c.mk}`;
  expect((await api(page, 'POST', `/api/items/${c.number}/comments`, { text: word })).status).toBe(200);
  expect((await api(page, 'POST', `/api/items/${c.number}/reply-proposals`, { text: word, reason: 'e2e' })).status).toBe(200);
  const before = (await itemOf(page, c)).messages.length;
  const paths = [
    `/api/items/${c.number}/write`, `/api/items/${c.number}/writes`, `/api/items/${c.number}/post`, `/api/items/${c.number}/send`,
    `/api/items/${c.number}/chat`, `/api/writes/1/withdraw`, `/api/posts/1/withdraw`, `/api/bridge/write`,
  ];
  for (const who of ['agent', 'bridge']) {
    for (const path of paths) {
      const r = await api(page, 'POST', path, { text: word, network: c.net, chat_id: c.chatId }, who);
      expect([401, 403, 404], `${who} ${path}`).toContain(r.status);
      expect(r.json?.ok, `${who} ${path}`).not.toBe(true);
    }
  }
  expect((await itemOf(page, c)).messages.length, 'no message was written').toBe(before);
  await page.reload();
  const again = await openRow(page, c);
  await expect(chatBox(again, 'Messenger')).toHaveValue('');
  await expect(entry(msgsOf(again, c), word), 'nothing of it in the chat').toHaveCount(0);
  void row; void box; void msgs;
});

test('17: the owner\'s writing is a signed-in page action: a cross-site post to it is refused and writes nothing', async ({ page }) => {
  test.skip(isPhone(page), 'the box to write into a chat is for a laptop only (point 17)');
  const { c, row } = await setup(page);
  const before = (await itemOf(page, c)).messages.length;
  const id = c.number.replace(/\D/g, '').replace(/^0+/, '');
  for (const path of [`/items/${id}/write`, `/writes/1/withdraw`]) {
    const r = await page.request.post(path, {
      headers: { Origin: 'https://evil.example', 'Sec-Fetch-Site': 'cross-site' },
      form: { text: `cross site ${c.mk}` }, failOnStatusCode: false,
    });
    expect(r.status(), path).toBe(403);
  }
  expect((await itemOf(page, c)).messages.length).toBe(before);
  await expect(entry(msgsOf(row, c), `cross site ${c.mk}`)).toHaveCount(0);
  void isPhone; void go; void seen; void token;
});
