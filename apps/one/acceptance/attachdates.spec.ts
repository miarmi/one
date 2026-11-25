// Acceptance tests for apps/one/SPEC.md point 7, the paragraph of 2026-11-23: the Attachments list goes by when a file was
// sent or attached (not by when ONE saved it), and each file shows that date, also a file from a message older than the
// thread's window.
//
// Made with the run's own data only: chats posted through the bridge path with their own message times (a time in the past
// is allowed), their files, an agent's `add_file`, and the owner's `Attach` on the page. The date a file shows is compared with
// the date the thread shows for the entry that carried or attached it ("shown as in the thread"), not with a fixed format,
// except for the one file from 2024, whose date the spec gives as an example (`26 Jan 2024`).
//
// Not tested, and why:
//  - the date of a file that the owner or an agent attached in a past year: an attachment's date is the moment it is attached,
//    which is now; a test cannot attach in the past.
import { expect, type Locator, type Page } from '@playwright/test';
import {
  test, mkr, go, api, tool, newChat, postChats, putFile, findItem, ensureListed, openRow, say, noteForm, chooseFile, type Chat, type Att, type Msg,
} from './one-helpers';

const OLD_AT = '2024-01-26T10:00:00Z'; // 10:00 UTC: the same calendar day in every time zone a test browser can have
const att = (id: string, mk: string, what: string): Att => ({ id, name: `${what}-${mk}.txt`, type: 'text/plain', size: 8 });

/** A chat with an old message (2024, outside the thread's window) and a recent one, both with a file. The recent message's file
 *  is posted and stored first, the old message's file later, so that the order in which ONE saved them is the wrong one. */
async function chatWithFiles(page: Page) {
  const mk = mkr();
  const recent = att('a-recent', mk, 'recent'), old = att('a-old', mk, 'old');
  const c = newChat({ mk, msgs: [{ text: `recent words ${mk}`, attachments: [recent], minsAgo: 180 }] });
  await postChats(page, [c]);
  expect((await putFile(page, c, c.msgs[0], recent, Buffer.from('22222222'))).status).toBe(200);
  c.number = await findItem(page, c);
  const oldMsg: Msg = { id: `${mk}-old`, text: `old words ${mk}`, at: OLD_AT, own: false, reactions: [], attachments: [old] };
  c.msgs.unshift(oldMsg);
  await postChats(page, [c]);
  expect((await putFile(page, c, oldMsg, old, Buffer.from('11111111'))).status).toBe(200);
  expect((await api(page, 'POST', `/api/items/${c.number}/seen`, {})).status).toBe(200);
  await ensureListed(page, c);
  return { c, mk, recent, old };
}

/** The Attachments list of an open row. */
const listOf = (row: Locator): Locator =>
  row.getByText('Attachments', { exact: true }).locator('visible=true').first().locator('xpath=ancestor::*[.//li][1]');
const chipOf = (list: Locator, name: string): Locator => list.getByRole('listitem').filter({ hasText: name }).first();
const names = async (list: Locator): Promise<string[]> =>
  (await list.getByRole('link').allInnerTexts()).map((t) => t.trim().split(/\s+/)[0]);

const DATE = /\b\d{1,2} [A-Z][a-z]{2}(?: \d{4})?\b|\b\d{1,2}:\d{2}\b/;
/** The date a chip shows: its time element, or else the first date or time in its text outside the file's name. */
async function chipDate(chip: Locator, name: string): Promise<string | null> {
  const t = chip.locator('time');
  if (await t.count()) return (await t.first().innerText()).trim();
  const text = (await chip.innerText()).split(name).join(' ');
  return DATE.exec(text)?.[0] ?? null;
}
/** The caption of a chip reads `added by <who> · sent <date>` for a file that came with a message and `added by <who> · attached <date>`
 *  for one attached in the thread (the word comes right before the date). */
async function expectCaption(chip: Locator, name: string, kind: 'sent' | 'attached') {
  const date = await chipDate(chip, name);
  expect(date, `${name} shows a date`).toBeTruthy();
  const text = (await chip.innerText()).replace(/\s+/g, ' ');
  const esc = date!.replace(/[.*+?^${}()|[\]\\]/g, '\\$&');
  expect(text, `${name}: the caption says it was ${kind}`).toMatch(new RegExp(`added by .+ · ${kind} ${esc}`));
}

/** The date the thread shows for the entry that carries these words: its time as written, and the date part of its full time. */
async function entryDate(row: Locator, words: string | RegExp): Promise<{ shown: string; day: string }> {
  const entry = row.getByRole('listitem').filter({ hasText: words }).filter({ has: row.page().locator('time') })
    .filter({ hasNot: row.getByText('Attachments', { exact: true }) }).first();
  const time = entry.locator('time').first();
  await expect(time, 'the thread shows a time for it').toBeVisible();
  const shown = (await time.innerText()).trim();
  const title = (await time.getAttribute('title')) ?? shown;
  return { shown, day: (/^\d{1,2} [A-Z][a-z]{2}(?: \d{4})?/.exec(title)?.[0] ?? shown) };
}
/** The chip's date is the thread's date for the same moment: written the same way, or (for a time of today the thread shows only as a clock time) the day of it. */
function sameAsThread(chip: string | null, e: { shown: string; day: string }, what: string) {
  expect(chip, `${what}: the file shows a date`).toBeTruthy();
  expect([e.shown, e.day], `${what}: the file's date (${chip}) is the thread's (${e.shown})`).toContain(chip!);
}

test('7: every file in the Attachments list shows a date', async ({ page }) => {
  const { c, mk, recent, old } = await chatWithFiles(page);
  await tool(page, 'add_file', { id: c.number, name: `agent-${mk}.txt`, text: 'agent words' });
  const row = await openRow(page, c);
  const list = listOf(row);
  await expect(list).toBeVisible();
  for (const name of [recent.name, old.name, `agent-${mk}.txt`]) {
    const chip = chipOf(list, name);
    await expect(chip, name).toBeVisible();
    expect(await chipDate(chip, name), `${name} shows a date`).toBeTruthy();
    await expectCaption(chip, name, name === `agent-${mk}.txt` ? 'attached' : 'sent');
  }
});

test('7: the date of a file that came with a message is the date of that message, as the thread shows it, with the year for another year', async ({ page }) => {
  const { c, mk, recent, old } = await chatWithFiles(page);
  const row = await openRow(page, c);
  const list = listOf(row);
  // the file of the 2024 message: the spec's own example
  expect(await chipDate(chipOf(list, old.name), old.name), 'a file from another year shows the year').toBe('26 Jan 2024');
  // open the older messages and compare with what the thread says for each message
  await row.getByRole('button', { name: 'Show older', exact: true }).locator('visible=true').first().click().catch(() => {});
  const oldEntry = await entryDate(row, `old words ${mk}`);
  expect(oldEntry.shown, 'the thread shows the 2024 message with its year').toBe('26 Jan 2024');
  sameAsThread(await chipDate(chipOf(list, old.name), old.name), oldEntry, 'old file');
  await expectCaption(chipOf(list, old.name), old.name, 'sent');
  const recentEntry = await entryDate(row, `recent words ${mk}`);
  sameAsThread(await chipDate(chipOf(list, recent.name), recent.name), recentEntry, 'recent file');
  await expectCaption(chipOf(list, recent.name), recent.name, 'sent');
  // and what ONE says about the file agrees: the date is when the message was sent, not when ONE saved the file
  const files = (await api(page, 'GET', `/api/items/${c.number}`)).json.files as any[];
  const f = files.find((x) => x.name === old.name);
  expect(f.sent_at).toBe('2024-01-26T10:00:00Z');
  expect(new Date(f.added_at).getTime(), 'it was saved long after it was sent').toBeGreaterThan(new Date(f.sent_at).getTime() + 86_400_000);
});

test('7: the date of a file attached in the thread is when it was attached, as the thread shows that entry', async ({ page }) => {
  const mk = mkr();
  const c = newChat({ mk, minsAgo: 90 });
  await postChats(page, [c]);
  c.number = await findItem(page, c);
  expect((await api(page, 'POST', `/api/items/${c.number}/seen`, {})).status).toBe(200);
  await ensureListed(page, c);
  const agentName = `agent-${mk}.txt`, ownerName = `owner-${mk}.txt`;
  expect((await tool(page, 'add_file', { id: c.number, name: agentName, text: 'agent words' })).isError).toBe(false);
  const row = await openRow(page, c);
  await chooseFile(page, noteForm(row), 'Attach', { name: ownerName, mimeType: 'text/plain', buffer: Buffer.from(`owner words ${mk}`) });
  await say(row, `here is my file ${mk}`);
  await expect(row.getByText(`Attached: ${ownerName}`).locator('visible=true').first()).toBeVisible();
  const list = listOf(row);
  for (const name of [agentName, ownerName]) {
    const e = await entryDate(row, `Attached: ${name}`);
    sameAsThread(await chipDate(chipOf(list, name), name), e, name);
    await expectCaption(chipOf(list, name), name, 'attached');
  }
});

test('7: the Attachments list is ordered by that date, newest last, even when an older file was saved later', async ({ page }) => {
  const { c, mk, recent, old } = await chatWithFiles(page);
  const agentName = `agent-${mk}.txt`;
  expect((await tool(page, 'add_file', { id: c.number, name: agentName, text: 'agent words' })).isError).toBe(false);
  // ONE saved the recent message's file first, then the agent's, and the 2024 file last: the order of saving is not the order shown
  const files = (await api(page, 'GET', `/api/items/${c.number}`)).json.files as any[];
  const when = (f: any) => new Date(f.sent_at ?? f.added_at).getTime();
  const expected = [...files].sort((a, b) => when(a) - when(b)).map((f) => f.name);
  expect(expected, 'the file of the oldest message comes first, the one attached now last').toEqual([old.name, recent.name, agentName]);
  const saved = [...files].sort((a, b) => new Date(a.added_at).getTime() - new Date(b.added_at).getTime() || a.file_id - b.file_id).map((f) => f.name);
  expect(saved, 'the old file was saved after the recent one').not.toEqual(expected);
  const row = await openRow(page, c);
  const list = listOf(row);
  await expect(list).toBeVisible();
  const shown = (await names(list)).filter((n) => n.endsWith(`${mk}.txt`));
  expect(shown).toEqual(expected);
});

test('7: a file from a message older than the thread\'s window is still listed, with its date', async ({ page }) => {
  const { c, old } = await chatWithFiles(page);
  const files = (await api(page, 'GET', `/api/items/${c.number}`)).json.files as any[];
  expect(files.find((f) => f.name === old.name).from_older_message, 'its message is outside the window').toBe(true);
  const row = await openRow(page, c);
  // the older message is not in the thread until it is asked for ...
  await expect(row.getByText(`old words ${c.mk}`).locator('visible=true')).toHaveCount(0);
  await expect(row.getByRole('button', { name: 'Show older', exact: true }).locator('visible=true').first()).toBeVisible();
  // ... but its file is in the list, with its date
  const list = listOf(row);
  const chip = chipOf(list, old.name);
  await expect(chip).toBeVisible();
  expect(await chipDate(chip, old.name)).toBe('26 Jan 2024');
  await expectCaption(chip, old.name, 'sent');
});

void go;
