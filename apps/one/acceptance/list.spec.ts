// Acceptance tests for apps/one/SPEC.md points 1, 2, 3, 5 and 27: the list in stages, opening rows,
// Pending / Resolved / Closed, rows in three parts, the Closed page.
//
// Not tested here, and why:
//  - 1 `Nothing here yet.` and `Nothing needs you`: only seen on an app with no items, so that test skips itself
//    on a preview, which holds real data.
//  - 2 keys `j`, `k` and `Enter`: they act on whichever row is current, which on a preview can be a real item
//    (opening a row in New moves it to In progress). `/`, `a`, `c` and `Esc` are tested on the test's own row.
//  - 3 and 28 the 14-day auto-close: needs 14 days of waiting and a setting only the owner changes.
//  - 5 the sender's own picture and the Gmail / Office mail / Calendar source labels: no way to put
//    mail or calendar entries in from a browser test.
import { expect } from '@playwright/test';
import {
  readyBare, test, mkr, isPhone, go, api, ready, chat, newChat, addMsg, postChats, stageOf, skip, queueEntries, waitsForClaude,
  groupHeader, expandGroup, toggleOf, expectRow, moreUntil, rowBox, isOpen, showRow, groupHas, openRow, act, btn, say, noteBox, seen,
  absTop, visit, findItem, inList, isLaptop, stagesPane, tool, press, type Chat,
} from './one-helpers';

const HEADING = /(\d+) new, (\d+) in progress/;
async function counts(page: import('@playwright/test').Page): Promise<[number, number]> {
  await go(page, '/');
  if (await page.getByText('Nothing needs you').first().isVisible().catch(() => false)) return [0, 0];
  const t = await page.getByText(HEADING).first().innerText();
  const m = HEADING.exec(t)!;
  return [Number(m[1]), Number(m[2])];
}

// ------------------------------------------------------------------ 1

test('1: the page is titled ONE, with the four stage groups folded and counted', async ({ page }) => {
  const c = await ready(page);
  await go(page, '/');
  await expect(page).toHaveTitle('ONE');
  for (const g of ['New', 'In progress', 'Pending', 'Resolved']) {
    await expect(groupHeader(page, g)).toBeVisible();
    await expect(groupHeader(page, g)).toContainText(/\d/);
  }
  // Closed is not a group of the list.
  await expect(page.getByRole('button', { name: /^Closed\b/ })).toHaveCount(0);
  if (isLaptop(page)) {
    // On a laptop the stages stand in the left pane; the page opens on New, whose rows are shown (see the 1: laptop tests).
    await expect(groupHeader(page, 'New')).toHaveAttribute('aria-pressed', 'true');
    await expectRow(page, c);
    return;
  }
  // Folded: the test's own row is not shown until its group opens.
  await expect(toggleOf(page, c)).toBeHidden();
  await expandGroup(page, 'New');
  await expectRow(page, c);
});

test('1: an app with no items at all says Nothing needs you and Nothing here yet.', async ({ page }) => {
  await go(page, '/');
  await page.waitForLoadState('load');
  const anyGroup = await groupHeader(page, 'New').isVisible();
  test.skip(anyGroup, 'the app holds items: the empty wording cannot be seen');
  await expect(page.getByText('Nothing needs you').first()).toBeVisible();
  await expect(page.getByText('Nothing here yet.').first()).toBeVisible();
});

test('1: the heading gives the new and the in-progress items as two numbers', async ({ page }) => {
  const [n0, p0] = await counts(page);
  const c = await ready(page);
  const [n1, p1] = await counts(page);
  expect(n1).toBe(n0 + 1);
  expect(p1).toBe(p0);
  const t = await showRow(page, c);
  await t.click(); // opening a row in New makes it In progress
  await expect.poll(() => isOpen(t)).toBe(true);
  await t.click();
  const [n2, p2] = await counts(page);
  expect(n2).toBe(n0);
  expect(p2).toBe(p0 + 1);
});

test('1: the page says which environment it runs in', async ({ page }) => {
  await go(page, '/');
  await expect(page.getByText(/^Environment: (production|preview: .+|development)$/).first()).toBeVisible();
});

test('1: the name ONE at the top of every page is a link to the list', async ({ page }) => {
  await ready(page);
  for (const key of ['settings', 'notes', 'connectors', 'closed']) {
    await visit(page, key);
    const name = page.getByRole('link', { name: 'ONE', exact: true }).first();
    await expect(name, key).toBeVisible();
    await name.click();
    await expect(page, key).toHaveTitle('ONE');
    await expect(page.getByRole('heading', { name: /^(\d+ new, \d+ in progress|Nothing needs you)$/ }).or(page.getByText(HEADING)).first(), key).toBeVisible();
    expect(new URL(page.url()).pathname, key).toBe('/');
  }
});

test('1: the top of the page respects vertical space', async ({ page }) => {
  test.skip(isLaptop(page), 'on a laptop the top is the ribbon of the three panes (the 1: laptop tests)');
  await ready(page); // an app with no items shows no groups
  await go(page, '/');
  const search = page.getByPlaceholder('Search mail, chats, events and notes').first();
  const head = page.getByText(HEADING).or(page.getByText('Nothing needs you')).first();
  const name = page.getByRole('link', { name: 'ONE', exact: true }).or(page.getByText('ONE', { exact: true })).first();
  const centres: number[] = [];
  for (const l of [name, search, head]) {
    await expect(l).toBeVisible();
    const b = (await l.boundingBox())!;
    centres.push(b.y + b.height / 2);
  }
  const spread = Math.max(...centres) - Math.min(...centres);
  if (isPhone(page)) expect(spread, 'at most two lines on a phone').toBeLessThan(110);
  else expect(spread, 'one line on a laptop').toBeLessThan(45);
  // The environment line sits in the page footer (the owner, 2026-11-20); the spec fixes only its wording.
  await expect(page.getByText(/^Environment: (production|preview: .+|development)$/).first()).toBeVisible();
  const first = (await groupHeader(page, 'New').boundingBox())!;
  expect(first.y, 'the list starts right under the top bar').toBeLessThan(isPhone(page) ? 280 : 170);
});

test('1: an opened group shows at least ten rows on a laptop', async ({ page }) => {
  test.skip(isPhone(page), 'the ten rows are promised for a laptop screen');
  test.setTimeout(120_000);
  const mk = mkr();
  const cs = Array.from({ length: 12 }, (_, i) => newChat({ mk: `${mk}x${i}`, text: `Row ${i} ${mk}` }));
  await postChats(page, cs);
  // The shared marker finds all twelve; Claude looks at each so that they stand in New.
  const found = new Set<string>();
  for (let tries = 0; tries < 20 && found.size < 12; tries++) {
    const r = await api(page, 'GET', `/api/search?q=${mk}&limit=50`);
    for (const x of r.json?.results ?? []) if (x.item) found.add(String(x.item));
    if (found.size < 12) await page.waitForTimeout(400);
  }
  expect(found.size).toBe(12);
  for (const id of found) expect((await api(page, 'POST', `/api/items/${id}/seen`, {})).status).toBe(200);
  await go(page, '/');
  await expandGroup(page, 'New');
  const rows = page.locator('[aria-expanded], summary').filter({ hasText: mk });
  await expect.poll(() => rows.locator('visible=true').count()).toBeGreaterThanOrEqual(10);
  for (const id of found) await api(page, 'POST', `/api/items/${id}/skip`, { reason: 'e2e cleanup ' + mk, kind: 'finished' });
});

test('1: the header says how many proposals wait in the group', async ({ page }) => {
  const c = await ready(page);
  const r = await api(page, 'POST', `/api/items/${c.number}/proposals`, { action: 'pending', reason: 'e2e ' + c.mk });
  expect(r.status, r.text).toBe(200);
  await go(page, '/');
  await expect(groupHeader(page, 'New')).toContainText(/\d+ to approve/);
});

test('1: the Pending header says the longest wait', async ({ page }) => {
  // The laptop text of SPEC 1 replaces the group headers by the stages of the left pane and names no longest wait there.
  test.skip(isLaptop(page), 'phone: the group header; a laptop has no group headers');
  const c = await ready(page);
  const row = await openRow(page, c);
  await act(row, 'Mark pending');
  await go(page, '/');
  await expect(groupHeader(page, 'Pending')).toContainText(/longest wait \d+ (days?|hours?|minutes?)/);
});

// ------------------------------------------------------------------ 2

test('2: tapping a closed row opens it in place, tapping its top line closes it', async ({ page }) => {
  const c = await ready(page);
  const t = await showRow(page, c);
  await expect.poll(() => isOpen(t)).toBe(false);
  await t.click();
  await expect.poll(() => isOpen(t)).toBe(true);
  await expect(seen(rowBox(t), `Hello ${c.mk}`)).toBeVisible();
  await t.click();
  await expect.poll(() => isOpen(t)).toBe(false);
});

test('2: opening a row in New makes it In progress, the row stays and then sits at the top of In progress', async ({ page }) => {
  const c = await ready(page);
  const t = await showRow(page, c);
  const before = await absTop(t);
  await t.click();
  await expect.poll(() => isOpen(t)).toBe(true);
  expect(await stageOf(page, c)).toBe('in_progress');
  expect(Math.abs((await absTop(t)) - before), 'the row did not move').toBeLessThan(4);
  await t.click(); // closed again
  expect(await groupHas(page, c, 'New')).toBe(false);
  // Fresh load with only In progress opened: The owner's row is the first row shown.
  await go(page, '/');
  await expandGroup(page, 'In progress');
  const rows = page.locator('[aria-expanded], summary').filter({ hasText: /Q-\d+/ }).locator('visible=true');
  await expectRow(page, c);
  await expect(rows.first()).toContainText(c.number);
});

test('2: several rows can be open and the page remembers which', async ({ page }) => {
  const a = await ready(page), b = await ready(page);
  const ta = await showRow(page, a);
  const tb = toggleOf(page, b);
  await expandGroup(page, 'New');
  await moreUntil(page, tb);
  await ta.click();
  await tb.click();
  await expect.poll(() => isOpen(ta)).toBe(true);
  await expect.poll(() => isOpen(tb)).toBe(true);
  await page.reload();
  await expandGroup(page, 'In progress');
  await expectRow(page, a);
  await expect.poll(() => isOpen(toggleOf(page, a))).toBe(true);
  await expect.poll(() => isOpen(toggleOf(page, b))).toBe(true);
});

test('2: The owner writing on a row leaves it where it is', async ({ page }) => {
  const c = await ready(page);
  const row = await openRow(page, c);
  const t = toggleOf(page, c);
  const before = await absTop(t);
  await say(row, 'a note ' + c.mk);
  await expect(seen(row, 'a note ' + c.mk)).toBeVisible();
  expect(Math.abs((await absTop(t)) - before)).toBeLessThan(4);
});

test('2: items that arrive while the page is open wait behind a button', async ({ page }) => {
  test.setTimeout(150_000);
  await go(page, '/');
  await expandGroup(page, 'New');
  const c = await ready(page); // arrives while the page is open
  const button = page.getByRole('button', { name: /^\d+ new items?$/ });
  await expect(button).toBeVisible({ timeout: 100_000 });
  await expect(toggleOf(page, c)).toBeHidden(); // it did not take its place by itself
  await button.click();
  await expandGroup(page, 'New');
  await expectRow(page, c);
});

test('2: keys on a laptop: / for search, a for the agenda, c for the note box, Esc to close', async ({ page }) => {
  test.skip(isPhone(page), 'keys are for a laptop');
  const c = await ready(page);
  const row = await openRow(page, c);
  const blur = () => page.evaluate(() => (document.activeElement as HTMLElement | null)?.blur());
  await blur();
  await page.keyboard.press('c');
  await expect(noteBox(row)).toBeFocused();
  await blur();
  await page.keyboard.press('/');
  await expect(page.getByPlaceholder('Search mail, chats, events and notes').first()).toBeFocused();
  await blur();
  const side = page.getByRole('complementary', { name: 'Agenda' }).first();
  const was = await side.isVisible().catch(() => false);
  await page.keyboard.press('a');
  await page.keyboard.press('a');
  // The agenda is always shown on a laptop (SPEC 1): pressing a twice leaves it as it was.
  await expect.poll(() => side.isVisible().catch(() => false)).toBe(was);
  await blur();
  const t = toggleOf(page, c);
  // Esc closes the row the keys are on: the one whose note box has the focus.
  await page.keyboard.press('c');
  await expect(noteBox(row)).toBeFocused();
  await page.keyboard.press('Escape');
  await page.keyboard.press('Escape');
  await expect.poll(() => isOpen(t)).toBe(false);
});

// ------------------------------------------------------------------ 3

test('3: Mark pending puts the item in Pending and shows how long it waited', async ({ page }) => {
  const c = await ready(page);
  const row = await openRow(page, c);
  await act(row, 'Mark pending');
  await expect(seen(row, 'You marked this pending.')).toBeVisible();
  expect(await stageOf(page, c)).toBe('pending');
  expect(await groupHas(page, c, 'Pending')).toBe(true);
  await expect(rowBox(toggleOf(page, c))).toContainText(/waiting \d+ (days?|hours?)/);
  await expect(rowBox(toggleOf(page, c))).not.toContainText(/waiting \d+ (days?|hours?) (left|of|until|more|remaining)|expires|limit/i);
});

test('3: Resolve and Reopen', async ({ page }) => {
  const c = await ready(page);
  const row = await openRow(page, c);
  await act(row, 'Resolve');
  await expect(seen(row, 'You resolved this.')).toBeVisible();
  expect(await stageOf(page, c)).toBe('resolved');
  expect(await groupHas(page, c, 'Resolved')).toBe(true);
  const again = await openRow(page, c);
  await act(again, 'Reopen');
  await expect(seen(again, 'You reopened this.')).toBeVisible();
  expect(await stageOf(page, c)).not.toBe('resolved');
});

test('3: the buttons that move a row on are reachable on the closed line', async ({ page }) => {
  const c = await ready(page);
  const t = await showRow(page, c);
  await expect.poll(() => isOpen(t)).toBe(false); // closed: the row was never opened
  const row = rowBox(t);
  // On a phone the closed line carries the resolve icon itself (point 5); on a laptop the ribbon acts on the chosen row.
  if (isPhone(page)) {
    await act(row, 'Resolve');
    expect(await stageOf(page, c)).toBe('resolved');
  } else {
    await act(row, 'Mark pending');
    expect(await stageOf(page, c)).toBe('pending');
  }
});

test('3: the owner\'s own message from anywhere moves an In progress item to Pending', async ({ page }) => {
  const c = await ready(page);
  const t = await showRow(page, c);
  await t.click(); // In progress
  await expect.poll(() => isOpen(t)).toBe(true);
  expect(await stageOf(page, c)).toBe('in_progress');
  await addMsg(page, c, { text: `my own answer ${c.mk}`, own: true, minsAgo: 30 });
  expect(await stageOf(page, c)).toBe('pending');
  expect(await waitsForClaude(page, c)).toBe(false);
  expect(await groupHas(page, c, 'Pending')).toBe(true);
});

for (const from of ['pending', 'resolved', 'closed'] as const) {
  test(`3: a new message from someone else brings a ${from} item back to New`, async ({ page }) => {
    const c = await ready(page);
    if (from === 'closed') {
      expect((await skip(page, c, 'e2e finished')).status).toBe(200);
    } else {
      const row = await openRow(page, c);
      await act(row, from === 'pending' ? 'Mark pending' : 'Resolve');
      expect(await stageOf(page, c)).toBe(from);
    }
    await addMsg(page, c, { text: `a new question ${c.mk}`, minsAgo: 60 });
    expect(await stageOf(page, c)).toBe('new');
    expect(await groupHas(page, c, 'New')).toBe(true);
    expect(await waitsForClaude(page, c), 'it goes to Claude again').toBe(true);
    await api(page, 'POST', `/api/items/${c.number}/seen`, {});
  });
}

test('3: nothing else moves an item by itself', async ({ page }) => {
  const c = await ready(page);
  const row = await openRow(page, c);
  await act(row, 'Resolve');
  expect((await api(page, 'POST', `/api/items/${c.number}/comments`, { text: 'a comment ' + c.mk })).status).toBe(200);
  expect((await api(page, 'POST', `/api/items/${c.number}/seen`, {})).status).toBe(200);
  expect(await stageOf(page, c)).toBe('resolved');
});

// ------------------------------------------------------------------ 5

test('5: an open row has who, what and the thread', async ({ page }) => {
  const mk = mkr();
  const c = await ready(page, { mk, msgs: [{ text: `First text ${mk}` }, { text: `Second text ${mk}` }] });
  const row = await openRow(page, c);
  await expect(seen(row, c.number)).toBeVisible();
  await expect(seen(row, c.senderName)).toBeVisible();
  await expect(seen(row, 'WhatsApp')).toBeVisible();
  await expect(row).toContainText(/\b2 messages\b/);
  await expect(seen(row, `Second text ${mk}`)).toBeVisible();
  await expect(seen(row, `First text ${mk}`)).toBeVisible();
  await expect(noteBox(row)).toBeVisible();
});

test('5: the three parts sit side by side on a laptop and stack on a phone', async ({ page }) => {
  const c = await ready(page);
  const row = await openRow(page, c);
  const text = (await seen(row, `Hello ${c.mk}`).boundingBox())!;
  const box = (await noteBox(row).boundingBox())!;
  if (isPhone(page)) expect(box.y, 'the thread is under the text').toBeGreaterThan(text.y + text.height - 2);
  else expect(box.x, 'the thread is beside the text').toBeGreaterThan(text.x + 20);
});

test('5: a phone thread has the note box fixed at the bottom of the screen', async ({ page }) => {
  test.skip(!isPhone(page), 'a phone only');
  const c = await ready(page);
  const row = await openRow(page, c);
  await expect(noteBox(row)).toBeInViewport();
  await page.evaluate(() => window.scrollTo(0, 0));
  await expect(noteBox(row)).toBeInViewport();
  await page.evaluate(() => window.scrollTo(0, document.body.scrollHeight));
  await expect(noteBox(row)).toBeInViewport();
});

test('5: a text of thirty lines shows in full', async ({ page }) => {
  const mk = mkr();
  const text = Array.from({ length: 30 }, (_, i) => `line${i} ${mk}`).join('\n');
  const c = await ready(page, { mk, text });
  const row = await openRow(page, c);
  await expect(seen(row, `line29 ${mk}`)).toBeVisible();
  await expect(btn(row, 'Show all')).toBeHidden();
  // nothing is cut off: the block is as tall as its text
  const block = row.getByText(`line0 ${mk}`).locator('visible=true').first();
  expect(await block.evaluate((el) => el.scrollHeight - el.clientHeight)).toBeLessThanOrEqual(2);
});

test('5: a very long text is folded, Show all opens it and Show less closes it', async ({ page }) => {
  const mk = mkr();
  const text = Array.from({ length: 120 }, (_, i) => `line${i} ${mk}`).join('\n');
  const c = await ready(page, { mk, text });
  const row = await openRow(page, c);
  await expect(seen(row, `line0 ${mk}`)).toBeVisible();
  // The whole text is one block: folded, it is cut short, and the last line cannot be read.
  const block = row.getByText(`line0 ${mk}`).locator('visible=true').first();
  const folded = (await block.boundingBox())!.height;
  const all = row.getByRole('button', { name: 'Show all', exact: true }).locator('visible=true').first();
  await all.click();
  await expect.poll(async () => (await block.boundingBox())!.height, 'Show all opens the whole text').toBeGreaterThan(folded * 1.5);
  const opened = (await block.boundingBox())!.height;
  const less = row.getByRole('button', { name: 'Show less', exact: true }).locator('visible=true').first();
  await expect(less).toBeVisible();
  await less.click();
  await expect.poll(async () => (await block.boundingBox())!.height, 'Show less folds it again').toBeLessThan(opened * 0.75);
});

test('5: the same person always has the same colour', async ({ page }) => {
  const mk = mkr();
  const a = await ready(page, { mk, senderId: `same-${mk}`, senderName: `Same ${mk}`, text: `one ${mk}` });
  const b = newChat({ mk: mk + 'b', senderId: `same-${mk}`, senderName: `Same ${mk}`, text: `two ${mk}b` });
  await postChats(page, [b]);
  const bn = await findItem(page, b);
  b.number = bn; b.id = bn;
  await api(page, 'POST', `/api/items/${bn}/seen`, {});
  const colour = async (c: Chat) => {
    const t = await showRow(page, c);
    return page.locator('[aria-expanded], summary').filter({ hasText: c.number }).first()
      .getByText(c.number, { exact: true }).first().evaluate((el) => {
        let n: HTMLElement | null = el as HTMLElement;
        for (let i = 0; i < 4 && n; i++, n = n.parentElement) {
          const bg = getComputedStyle(n).backgroundColor;
          if (bg && bg !== 'rgba(0, 0, 0, 0)' && bg !== 'transparent') return bg;
        }
        return getComputedStyle(el as HTMLElement).color;
      }).then((v) => { void t; return v; });
  };
  const ca = await colour(a);
  const cb = await colour(b);
  expect(ca).toBe(cb);
});

test('5: a chat still being written in is marked Live, a quiet one is not', async ({ page }) => {
  const live = await chat(page, { live: true });      // shown at once, nobody has looked at it
  const quiet = await ready(page);
  const rl = await openRow(page, live);
  await expect(seen(rl, 'Live')).toBeVisible();
  const rq = await openRow(page, quiet);
  await expect(rq.getByText('Live', { exact: true }).locator('visible=true')).toHaveCount(0);
});

test('5: a closed row is one compact line with number, sender, source, subject and the newest text', async ({ page }) => {
  const mk = mkr();
  const c = await ready(page, { mk, msgs: [{ text: `Old words ${mk}` }, { text: `Newest words ${mk}` }] });
  const t = await showRow(page, c);
  await expect.poll(() => isOpen(t)).toBe(false);
  const row = rowBox(t);
  for (const part of [c.number, c.senderName, 'WhatsApp', c.name, `Newest words ${mk}`]) await expect(row).toContainText(part);
  const b = (await t.boundingBox())!;
  expect(b.height, 'one compact line').toBeLessThan(isPhone(page) ? 140 : 70);
});

test('5: each network has its own source name', async ({ page }) => {
  for (const [net, label] of [['whatsapp', 'WhatsApp'], ['messenger', 'Messenger'], ['imessage', 'iMessage'], ['sms', 'SMS']]) {
    const c = await ready(page, { net });
    const t = await showRow(page, c);
    await expect(rowBox(t)).toContainText(label);
  }
});

// ------------------------------------------------------------------ 27

test('27: a clear skip lands on the Closed page under Closed by Claude with its reason, and Bring back returns it', async ({ page }) => {
  const c = await ready(page);
  expect((await skip(page, c, 'noise because e2e', 'noise')).status).toBe(200);
  expect(await inList(page, c)).toBe(false);
  await go(page, '/');
  if (isLaptop(page)) await expect(stagesPane(page).getByRole('link', { name: /^Closed\s*\d+$/ })).toBeVisible();
  else await expect(page.getByRole('link', { name: /^Closed \(\d+(, \d+ not seen)?\)$/ }).first()).toBeVisible();
  await visit(page, 'closed');
  await expect(page.getByRole('heading', { name: 'Closed', exact: true }).first()).toBeVisible();
  await expect(page.getByText('Closed by Claude', { exact: true }).first()).toBeVisible();
  const entry = page.getByText(c.number, { exact: false }).first().locator('xpath=ancestor::*[.//button[normalize-space()="Bring back"]][1]');
  await expect(entry).toContainText('Closed by Claude');
  await expect(entry).toContainText(`Reason: noise because e2e ${c.mk}`);
  await act(entry, 'Bring back');
  expect(await stageOf(page, c)).toBe('new');
  const row = await openRow(page, c);
  await expect(seen(row, 'You brought this back.')).toBeVisible();
});

test('27: the Closed page lists the newest first and has a link back', async ({ page }) => {
  const a = await ready(page), b = await ready(page);
  expect((await skip(page, a)).status).toBe(200);
  await page.waitForTimeout(1100);
  expect((await skip(page, b)).status).toBe(200);
  await visit(page, 'closed');
  const text = await page.getByRole('main').innerText();
  expect(text.indexOf(b.number)).toBeGreaterThan(-1);
  expect(text.indexOf(b.number)).toBeLessThan(text.indexOf(a.number));
  await page.getByRole('link', { name: 'Back to the queue', exact: true }).first().click();
  await expect(page).toHaveTitle('ONE');
});

test('27: what the owner approved to close is listed under Everything else with who and why', async ({ page }) => {
  const c = await ready(page);
  const r = await api(page, 'POST', `/api/items/${c.number}/proposals`, { action: 'close', reason: `owner closes ${c.mk}` });
  expect(r.status, r.text).toBe(200);
  const row = rowBox(await showRow(page, c));
  await act(row, 'Approve');
  expect(await stageOf(page, c)).toBe('closed');
  await visit(page, 'closed');
  await expect(page.getByText('Everything else', { exact: true }).first()).toBeVisible();
  const entry = page.getByText(c.number).first().locator('xpath=ancestor::*[.//button[normalize-space()="Bring back"]][1]');
  await expect(entry).toContainText(/Closed by /);
  await expect(entry).toContainText('Reason:');
  await act(entry, 'Bring back');
});


// ------------------------------------------------------------------ 3, 5, 12 and 27: what the spec added on 2026-11-21

test('3: Claude can bring a pending item back to New with one sentence, and it needs no Approve', async ({ page }) => {
  const c = await ready(page);
  const row = await openRow(page, c);
  await act(row, 'Mark pending');
  expect(await stageOf(page, c)).toBe('pending');
  const why = `found a new fact ${c.mk}`;
  const r = await tool(page, 'bring_back', { id: c.number, reason: why });
  expect(r.isError, r.text).toBe(false);
  expect(await stageOf(page, c)).toBe('new');
  const again = await openRow(page, c);
  await expect(seen(again, `Claude: ${why}`)).toBeVisible();
});

test('5: a phone has no three-dots menu: the open row\'s actions, the note box, Attach and Send are icons in one bar', async ({ page }) => {
  test.skip(!isPhone(page), 'the one bar is the phone\'s; a laptop has the ribbon');
  const c = await ready(page);
  const row = await openRow(page, c);
  for (const n of ['Mark pending', 'Resolve', 'To do', 'Attach', 'Send']) {
    await expect(btn(row, n), n + ' is a button named by its word').toBeVisible();
    // an icon, not a word: the name is read by a screen reader, the button shows a picture
    expect(await btn(row, n).locator('svg, img, [aria-hidden=true]').count(), n + ' has a picture').toBeGreaterThan(0);
  }
  await expect(noteBox(row)).toBeVisible();
  await expect(row.locator('button[aria-haspopup], [role=menu], [aria-haspopup=true]').locator('visible=true'), 'no menu behind three dots').toHaveCount(0);
  await expect(row.locator('summary').filter({ hasText: /^\s*(⋯|…|\.\.\.|more|actions)?\s*$/i }).locator('visible=true'), 'no three-dots summary').toHaveCount(0);
  await expect(row.getByRole('button', { name: /^(more|actions|menu|⋯|…|\.\.\.)/i }), 'no three-dots button').toHaveCount(0);
  // the buttons keep their places in the bar: it is fixed at the bottom of the screen
  const vh = page.viewportSize()!.height;
  const b = (await btn(row, 'Send').boundingBox())!;
  expect(b.y + b.height, 'the bar is at the bottom of the screen').toBeGreaterThan(vh - 140);
});

test('5: on a phone a closed row is resolved with one tap, a line says so with Undo, and in Resolved one tap reopens it', async ({ page }) => {
  test.skip(!isPhone(page), 'one-tap resolve is on the phone\'s closed row');
  const c = await ready(page);
  const t = await showRow(page, c);
  const closed = rowBox(t);
  await expect(btn(closed, 'Resolve'), 'the closed row carries the resolve icon').toBeVisible();
  await press(page, btn(closed, 'Resolve'));
  await expect(page.getByText(`Resolved ${c.number}.`).first()).toBeVisible();
  await expect(page.getByRole('button', { name: 'Undo', exact: true }).first()).toBeVisible();
  expect(await stageOf(page, c)).toBe('resolved');
  await press(page, page.getByRole('button', { name: 'Undo', exact: true }).first());
  expect(await stageOf(page, c), 'Undo took the resolve back').not.toBe('resolved');
  // resolve again, then reopen from the closed row in Resolved
  const t2 = await showRow(page, c);
  await press(page, btn(rowBox(t2), 'Resolve'));
  expect(await stageOf(page, c)).toBe('resolved');
  await go(page, '/');
  await expandGroup(page, 'Resolved');
  const t3 = toggleOf(page, c);
  await moreUntil(page, t3);
  await expect(btn(rowBox(t3), 'Reopen'), 'in Resolved the closed row carries the reopen icon').toBeVisible();
  await press(page, btn(rowBox(t3), 'Reopen'));
  await expect(page.getByText(`Reopened ${c.number}.`).first()).toBeVisible();
  expect(await stageOf(page, c)).not.toBe('resolved');
});

test('12: the chip of a proposal says why on the closed row, without opening it', async ({ page }) => {
  const c = await ready(page);
  const why = `we went through this together ${c.mk}`;
  const r = await api(page, 'POST', `/api/items/${c.number}/proposals`, { action: 'resolve', reason: why });
  expect(r.status, r.text).toBe(200);
  const t = await showRow(page, c);
  expect(await isOpen(t)).toBe(false);
  const closed = rowBox(t);
  await expect(seen(closed, why), 'the reason is read on the closed row').toBeVisible();
  await expect(btn(closed, 'Approve')).toBeVisible();
  await expect(btn(closed, 'Dismiss')).toBeVisible();
  // Approve and Dismiss stay together on one line
  const [a, d] = await Promise.all([btn(closed, 'Approve').boundingBox(), btn(closed, 'Dismiss').boundingBox()]);
  expect(Math.abs(a!.y - d!.y), 'Approve and Dismiss on one line').toBeLessThan(8);
});

test('27: an entry Claude closed is marked Not seen, Seen clears the mark, and the link says how many are not seen', async ({ page }) => {
  const c = await ready(page);
  expect((await skip(page, c, 'seen test', 'noise')).status).toBe(200);
  await visit(page, 'closed');
  const entry = page.getByText(c.number).first().locator('xpath=ancestor::*[.//button[normalize-space()="Bring back"]][1]');
  await expect(entry).toContainText('Not seen');
  await expect(page.getByRole('button', { name: 'Mark all seen', exact: true }).first()).toBeVisible();
  await go(page, '/');
  const link = isLaptop(page) ? stagesPane(page).getByRole('link', { name: /^Closed/ }) : page.getByRole('link', { name: /^Closed \(/ }).first();
  if (!isLaptop(page)) await expect(link).toHaveText(/^Closed \(\d+, \d+ not seen\)$/);
  await visit(page, 'closed');
  const e2 = page.getByText(c.number).first().locator('xpath=ancestor::*[.//button[normalize-space()="Bring back"]][1]');
  await press(page, e2.getByRole('button', { name: 'Seen', exact: true }));
  const e3 = page.getByText(c.number).first().locator('xpath=ancestor::*[.//button[normalize-space()="Bring back"]][1]');
  await expect(e3).not.toContainText('Not seen');
  await act(e3, 'Bring back');
});
