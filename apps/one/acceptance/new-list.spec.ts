// New acceptance tests for apps/one/SPEC.md points 3, 5 (phone), 7, 12, 17 and 23 that the older files do not cover.
// Written from SPEC.md alone. Each test names its own run (one-helpers.ts) and uses chats posted like the bridge posts them.
//
// Not tested here, and why:
//  - 3 the time-based half of "Pending has no time limit" (that nothing moves a Pending item however long it waits) needs
//    days of waiting: the test checks that Pending stays on reload and that the row's words carry no limit.
//  - 3 the wording `waiting <n> days` and `waiting <n> hours` for longer waits: the wait counts from the move into
//    Pending, which a test cannot backdate.
//  - 23 rows of a connection other than the chat bridge: a test cannot create mail or calendar entries, so `Only Gmail`
//    is checked for hiding the chat rows, not for showing Gmail rows.
import { expect } from '@playwright/test';
import {
  test, mkr, days, go, api, ready, readyBare, chat, newChat, postChats, putFile, findItem, isPhone, isLaptop, isOpen, act, btn, seen,
  openRow, showRow, rowBox, toggleOf, expandGroup, moreUntil, inList, visit, openConnectors, stagesPane, connectionsPane,
  type Chat,
} from './one-helpers';
import { ag, grow, propose, stage, findRowHere } from './new-helpers';

const HEAD = /(\d+) new, (\d+) in progress/;

// ------------------------------------------------------------------ 3

test('3: Pending has no time limit: the item stays Pending and its row reads waiting <n> hours or days, with no word of a limit', async ({ page }) => {
  const c = await ready(page);
  if (isPhone(page)) await grow(page, c);
  const row = await openRow(page, c);
  await act(row, 'Mark pending');
  expect(await stage(page, c)).toBe('pending');
  await page.waitForTimeout(1500);
  await go(page, '/');
  expect(await stage(page, c), 'nothing moves it by itself').toBe('pending');
  const t = await showRow(page, c);
  const waiting = rowBox(t).getByText(/\bwaiting\b/i).locator('visible=true').first();
  await expect(waiting).toBeVisible();
  const words = await waiting.innerText();
  expect(words).toMatch(/waiting \d+ hours?\b/i);
  expect(words, 'the words must not read as a limit').not.toMatch(/expir|limit|until|left|remain|deadline|auto|\bof \d+/i);
});

// ------------------------------------------------------------------ 5, phone

/** The words a user can see inside an element: text nodes whose box is real size and not clipped away (sr-only style). */
async function visibleWords(el: import('@playwright/test').Locator): Promise<string> {
  return el.evaluate((root) => {
    const out: string[] = [];
    const w = document.createTreeWalker(root, NodeFilter.SHOW_TEXT);
    for (let n = w.nextNode(); n; n = w.nextNode()) {
      const t = (n.textContent ?? '').trim();
      if (!t) continue;
      let hidden = false;
      for (let e = n.parentElement; e; e = e.parentElement) {
        const cs = getComputedStyle(e); const b = e.getBoundingClientRect();
        if (cs.display === 'none' || cs.visibility === 'hidden' || cs.opacity === '0' || b.width < 3 || b.height < 3 ||
            /rect\(0(px)?,? ?0(px)?,? ?0(px)?,? ?0(px)?\)/.test(cs.clip) || /inset\(50%\)/.test(cs.clipPath) || parseFloat(cs.fontSize) === 0 ||
            (cs.overflow === 'hidden' && (b.width < 3 || b.height < 3))) { hidden = true; break; }
        if (e === root) break;
      }
      if (!hidden) {
        const r = document.createRange(); r.selectNodeContents(n);
        const rr = r.getBoundingClientRect();
        if (rr.width >= 3 && rr.height >= 3) out.push(t);
      }
    }
    return out.join(' ');
  });
}

const BAR = ['Mark pending', 'Resolve', 'To do', 'Attach', 'Send'];

for (const from of ['new', 'in_progress', 'pending'] as const) {
  test(`5: on a phone one tap on the closed row's icon resolves it, says Resolved <number>. with Undo, and Undo returns it to ${from}`, async ({ page }) => {
    test.skip(!isPhone(page), 'one-tap resolve is the phone\'s');
    const c = await ready(page);
    await grow(page, c);
    if (from !== 'new') {
      const row = await openRow(page, c);
      if (from === 'pending') await act(row, 'Mark pending');
      await toggleOf(page, c).tap(); // close it again
      await expect.poll(() => isOpen(toggleOf(page, c))).toBe(false);
      await page.waitForTimeout(500);
      expect(await stage(page, c)).toBe(from);
    }
    const t = await showRow(page, c);
    await expect.poll(() => isOpen(t)).toBe(false);
    const quick = rowBox(t).getByRole('button', { name: 'Resolve', exact: true }).locator('visible=true');
    await expect(quick, 'the closed row carries the resolve icon itself').toHaveCount(1);
    await quick.first().tap();
    await expect(page.getByText(`Resolved ${c.number}.`).first()).toBeVisible();
    await expect.poll(() => stage(page, c)).toBe('resolved');
    const undo = page.getByRole('button', { name: 'Undo', exact: true }).first();
    await expect(undo).toBeVisible();
    await undo.tap();
    await expect.poll(() => stage(page, c)).toBe(from);
  });
}

test('5: on a phone one tap on the reopen icon of a closed Resolved row says Reopened <number>. with Undo, and Undo puts it back in Resolved', async ({ page }) => {
  test.skip(!isPhone(page), 'one-tap reopen is the phone\'s closed row');
  const c = await ready(page);
  await grow(page, c);
  const row = await openRow(page, c);
  await act(row, 'Resolve');
  expect(await stage(page, c)).toBe('resolved');
  await toggleOf(page, c).tap(); // close it, as a user does
  await expect.poll(() => isOpen(toggleOf(page, c))).toBe(false);
  await go(page, '/');
  await expandGroup(page, 'Resolved');
  const t = toggleOf(page, c);
  await moreUntil(page, t);
  await expect(t).toBeVisible();
  await page.waitForTimeout(1200); // the icon ignores a tap in its first moments (a guard against a double tap)
  const reopen = rowBox(t).getByRole('button', { name: 'Reopen', exact: true }).locator('visible=true');
  await expect(reopen, 'a closed row in Resolved carries the reopen icon').toHaveCount(1);
  await reopen.first().tap();
  await expect(page.getByText(`Reopened ${c.number}.`).first()).toBeVisible();
  await expect.poll(() => stage(page, c)).not.toBe('resolved');
  const undo = page.getByRole('button', { name: 'Undo', exact: true }).first();
  await expect(undo).toBeVisible();
  await page.waitForTimeout(700);
  await undo.tap();
  await expect.poll(() => stage(page, c)).toBe('resolved');
});

test('5: on a phone a tap opens a closed row and a tap on its top line closes it', async ({ page }) => {
  test.skip(!isPhone(page), 'touch taps are the phone\'s');
  const c = await ready(page);
  await grow(page, c);
  const t = await showRow(page, c);
  await expect.poll(() => isOpen(t)).toBe(false);
  await t.tap();
  await expect.poll(() => isOpen(t)).toBe(true);
  await expect(seen(rowBox(t), `Hello ${c.mk}`)).toBeVisible();
  await t.tap();
  await expect.poll(() => isOpen(t)).toBe(false);
});

// ------------------------------------------------------------------ 7

const paperclip = (row: import('@playwright/test').Locator) =>
  row.getByRole('img', { name: 'Has attachments', exact: true }).or(row.getByLabel('Has attachments', { exact: true }))
    .or(row.getByTitle('Has attachments', { exact: true })).locator('visible=true');

test('7: a file an agent added to the thread also shows the paperclip on the closed row', async ({ page }) => {
  const c = await ready(page);
  if (isPhone(page)) await grow(page, c);
  const before = await showRow(page, c);
  await expect(paperclip(rowBox(before))).toHaveCount(0);
  const r = await ag(page, 'add_file', { id: c.number, name: `agent-${c.mk}.txt`, text: `made by an agent ${c.mk}` });
  expect(r.isError, r.text).toBe(false);
  const t = await showRow(page, c);
  await expect(paperclip(rowBox(t)).first()).toBeVisible();
});

// ------------------------------------------------------------------ 12

test('12: the reason of a proposal shows on the closed row with Approve and Dismiss together on one line', async ({ page }) => {
  const c = await ready(page);
  if (isPhone(page)) await grow(page, c);
  const token = `Reason-${c.mk}`;
  const reason = `We went through this together and it is settled, nothing is owed. ${token} ` +
    'The mail only repeats what was agreed last week and asks for nothing new, so resolving it loses nothing at all.';
  await propose(page, c, 'resolve', reason);
  const t = await showRow(page, c);
  await expect.poll(() => isOpen(t)).toBe(false);
  const box = rowBox(t);
  const why = box.getByText(token).locator('visible=true').first();
  await expect(why, 'the reason is read without opening the row').toBeVisible();
  await expect(box.getByText(/\bProposal\b|\bresolve\b/i).locator('visible=true').first()).toBeVisible();
  const a = btn(box, 'Approve'), d = btn(box, 'Dismiss');
  await expect(a).toBeVisible();
  await expect(d).toBeVisible();
  const [ab, db] = [(await a.boundingBox())!, (await d.boundingBox())!];
  expect(Math.abs(ab.y - db.y), 'Approve and Dismiss on one line').toBeLessThan(8);
  expect(await stage(page, c), 'nothing was approved').toBe('new');
  if (isPhone(page)) {
    const clamped = await why.evaluate((el) => {
      const cs = getComputedStyle(el); const lh = parseFloat(cs.lineHeight) || parseFloat(cs.fontSize) * 1.3;
      return el.getBoundingClientRect().height / lh;
    });
    expect(clamped, 'on a phone at most two lines when closed').toBeLessThan(2.6);
    await t.tap();
    await expect.poll(() => isOpen(t)).toBe(true);
    const full = rowBox(t).getByText(token).locator('visible=true').first();
    await expect(full).toBeVisible();
    const cut = await full.evaluate((el) => {
      const cs = getComputedStyle(el); const lh = parseFloat(cs.lineHeight) || parseFloat(cs.fontSize) * 1.3;
      return { lines: el.getBoundingClientRect().height / lh, over: el.scrollHeight > el.clientHeight + 2 };
    });
    expect(cut.over, 'in full when the row is opened').toBe(false);
    expect(cut.lines, 'the whole reason, more than two lines on a phone').toBeGreaterThan(2.4);
    await expect(rowBox(t).getByText('Because', { exact: false })).toHaveCount(0);
  }
});

// ------------------------------------------------------------------ 17

// By difference, on this test's own chats only: other rows (other tests' leftovers, rows that arrive at the same moment)
// may change the counts, so each before/after pair is measured again when something else moved in between.
test('17: a phone counts only what it lists: a bare chat adds nothing, and one whose thread grows adds one', async ({ page }) => {
  test.skip(!isPhone(page), 'the laptop lists every chat');
  const counts = async () => {
    await go(page, '/');
    if (await page.getByText('Nothing needs you').first().isVisible().catch(() => false)) return 0;
    const m = HEAD.exec(await page.getByText(HEAD).first().innerText())!;
    return Number(m[1]) + Number(m[2]);
  };
  const tries = 6;
  let bare: Chat | null = null;
  let ok = false;
  for (let i = 0; i < tries && !ok; i++) {
    const a = await counts();
    bare = await readyBare(page);
    ok = await expect.poll(counts, { timeout: 6000 }).toBe(a).then(() => true, () => false); // it may take a moment to settle
  }
  expect(ok, 'a chat with no thread of its own changes no count').toBe(true);
  expect(await inList(page, bare!), 'and is not listed').toBe(false);
  ok = false;
  let own: Chat | null = null;
  for (let i = 0; i < tries && !ok; i++) {
    own = await readyBare(page);
    const a = await counts();
    await grow(page, own, `a comment from Claude ${own.mk}`);
    ok = await expect.poll(counts, { timeout: 6000 }).toBe(a + 1).then(() => true, () => false);
  }
  expect(ok, 'a chat whose thread grew adds one').toBe(true);
  expect(await inList(page, own!), 'and is listed').toBe(true);
  expect(await inList(page, bare!), 'the bare one is still not').toBe(false);
});

// ------------------------------------------------------------------ 23

/** Choose a connection: a line of the Connections pane on a laptop where there is one, else its link on the Connectors page. */
async function choose(page: import('@playwright/test').Page, name: string) {
  await go(page, '/');
  if (isLaptop(page)) {
    const line = connectionsPane(page).getByRole('link', { name: new RegExp('^' + name) });
    if (await line.count()) {
      // The line leads to Connectors (point 1); its heading there is the choosing link.
      await line.first().click();
    } else await openConnectors(page);
  } else await openConnectors(page);
  const link = page.getByRole('heading', { name, exact: true }).getByRole('link', { name, exact: true }).first();
  await expect(link, `the ${name} link on Connectors`).toBeVisible();
  await link.click();
}

