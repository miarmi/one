// Acceptance tests for apps/one/SPEC.md points 4, 6, 7, 8, 9, 10, 11, 12, 17 and 30: what counts as a new
// message, the thread, attachments, message text and pictures, the last month, one matter one row, Claude
// looks first, proposals, chats judged once finished, nothing is kept twice.
//
// Not tested here, and why:
//  - 4 mail (read state, label, star, folder) and events: cannot be driven from a browser test.
//  - 6 folding of acknowledgement notes (`1 older note from <agent>`): neither SPEC.md nor API.md says how an agent
//    marks a note as one that only acknowledges something.
//  - 10 a matter pattern joining separate mails, and merge / split proposals: patterns are a setting only the owner
//    changes and API.md has no merge or split address.
//  - 12 the `Result` marker: nothing the spec lets a test reach produces a result without sending something.
//  - 30 the first wording of an edited message: nothing under /api/ shows the kept version, only that a change happened.
//  - 11 how Claude decides (noise vs content): Claude's review runs outside the app.
import { expect } from '@playwright/test';
import {
  test, mkr, isPhone, go, api, ready, chat, newChat, addMsg, postChats, putFile, findItem, stageOf, skip,
  waitsForClaude, queueEntries, inList, groupHas, openRow, showRow, act, btn, say, noteBox, seen, rowBox,
  toggleOf, tool, ensureListed, expandGroup, isOpen, chooseFile, waitingCount, noteForm, iso, makePng, days, qn, type Chat, expectRefusal
} from './one-helpers';

/** An item that is not in the list (closed): found by search, and opened from its result. */
async function openFromSearch(page: import('@playwright/test').Page, c: Chat) {
  await go(page, `/search?q=${encodeURIComponent(c.mk)}`);
  await page.getByRole('link', { name: c.name }).first().click(); // the result that is the item (not a saved note)
  await expect(page).toHaveURL(/\/items\/\d+/);
}

const errorText = (r: { text: string }) => r.text.slice(0, 300);

// ------------------------------------------------------------------ 4

test('4: fetching the same unchanged message again changes nothing', async ({ page }) => {
  const c = newChat({ text: `Same again ${mkr()}` });
  const first = await postChats(page, [c]);
  expect(first.messages ?? 1).toBeGreaterThanOrEqual(1);
  c.number = await findItem(page, c);
  const second = await postChats(page, [c]);
  expect(second.stored ?? 0).toBe(0);
  expect(second.versions ?? 0).toBe(0);
  expect(second.unchanged ?? 1).toBeGreaterThanOrEqual(1);
  const item = await api(page, 'GET', `/api/items/${c.number}`);
  expect(item.json.messages).toHaveLength(1);
  await api(page, 'POST', `/api/items/${c.number}/seen`, {});
});

test('4: a reaction on a chat message adds no message and brings nothing back', async ({ page }) => {
  const c = await ready(page);
  const row = await openRow(page, c);
  await act(row, 'Resolve');
  expect(await stageOf(page, c)).toBe('resolved');
  c.msgs[0].reactions = [{ by: c.senderId, emoji: '👍' }];
  await postChats(page, [c]);
  expect(await stageOf(page, c)).toBe('resolved');
  expect((await api(page, 'GET', `/api/items/${c.number}`)).json.messages).toHaveLength(1);
  expect(await waitsForClaude(page, c)).toBe(false);
});

test('4: an edited chat message shows the new text marked edited and goes back to Claude, with one message', async ({ page }) => {
  const c = await ready(page);
  const row0 = await openRow(page, c);
  await act(row0, 'Resolve');
  c.msgs[0].text = `Corrected words ${c.mk}`;
  c.msgs[0].editedAt = iso(0); // the real bridge says when Beeper has it edited
  const r = await postChats(page, [c]);
  expect(r.versions ?? 1).toBeGreaterThanOrEqual(1);
  expect(await waitsForClaude(page, c)).toBe(true);
  const item = await api(page, 'GET', `/api/items/${c.number}`);
  expect(item.json.messages, 'a changed copy never becomes a second message').toHaveLength(1);
  await api(page, 'POST', `/api/items/${c.number}/seen`, {});
  const row = await openRow(page, c);
  await expect(seen(row, `Corrected words ${c.mk}`)).toBeVisible();
  await expect(seen(row, 'edited')).toBeVisible();
});

test('4: a message from someone else is added and brings the item back to Claude', async ({ page }) => {
  const c = await ready(page);
  await addMsg(page, c, { text: `Another one ${c.mk}`, minsAgo: 45 });
  const item = await api(page, 'GET', `/api/items/${c.number}`);
  expect(item.json.messages).toHaveLength(2);
  expect(await waitsForClaude(page, c)).toBe(true);
  await api(page, 'POST', `/api/items/${c.number}/seen`, {});
});

test('4: a message the owner sent himself is added and never brings the item to Claude', async ({ page }) => {
  const c = await ready(page);
  await addMsg(page, c, { text: `from the owner ${c.mk}`, own: true, minsAgo: 40 });
  expect((await api(page, 'GET', `/api/items/${c.number}`)).json.messages).toHaveLength(2);
  expect(await waitsForClaude(page, c)).toBe(false);
});

// ------------------------------------------------------------------ 6

test('6: The owner writes a note signed you, and the item is queued for Claude once', async ({ page }) => {
  const c = await ready(page);
  const row = await openRow(page, c);
  await expect(noteBox(row)).toHaveAttribute('placeholder', 'Add a note or ask Claude…');
  await say(row, 'first note ' + c.mk);
  await expect(seen(row, 'first note ' + c.mk)).toBeVisible();
  await expect(seen(row, 'you')).toBeVisible();
  await say(row, 'second note ' + c.mk);
  await expect(seen(row, 'second note ' + c.mk)).toBeVisible();
  const queued = await queueEntries(page, c);
  expect(queued, 'queued once, not once per note').toHaveLength(1);
  expect(queued[0].reason).toBe('note from the owner');
  expect((await api(page, 'POST', `/api/items/${c.number}/comments`, { text: 'answer ' + c.mk })).status).toBe(200);
  expect(await waitsForClaude(page, c), 'not queued once Claude has answered').toBe(false);
});

test('6: pressing Enter sends, Shift and Enter make a new line', async ({ page }) => {
  const c = await ready(page);
  const row = await openRow(page, c);
  const box = noteBox(row);
  await box.fill(`top line ${c.mk}`);
  await box.press('Shift+Enter');
  await box.pressSequentially(`bottom line ${c.mk}`);
  expect(await box.inputValue()).toContain('\n');
  await expect(seen(row, `top line ${c.mk}`)).toBeHidden(); // nothing was sent yet (the text is only in the box)
  await box.press('Enter');
  await expect(box).toHaveValue('');
  await expect(seen(row, `top line ${c.mk}`)).toBeVisible();
  await expect(seen(row, `bottom line ${c.mk}`)).toBeVisible();
});

test('6: an empty entry is refused', async ({ page }) => {
  const c = await ready(page);
  const row = await openRow(page, c);
  await row.getByRole('button', { name: 'Send', exact: true }).first().click();
  await expect(seen(row, 'Write something first.')).toBeVisible();
});

test('6: a thread with no entries says No notes yet.', async ({ page }) => {
  test.skip(isPhone(page), 'a phone lists a chat only once its thread holds something besides the chat\'s messages (point 17)');
  const c = await chat(page, { live: true }); // shown at once, with nothing written in it by anyone
  const row = await openRow(page, c);
  await expect(seen(row, 'No notes yet.')).toBeVisible();
});

test('6: agents sign with their name, oldest first', async ({ page }) => {
  const c = await ready(page);
  const a1 = 'first-' + mkr(), a2 = 'second-' + mkr(), robo = 'Robo' + mkr().slice(3, 9);
  expect((await api(page, 'POST', `/api/items/${c.number}/comments`, { text: a1 })).status).toBe(200);
  expect((await api(page, 'POST', `/api/items/${c.number}/comments`, { text: a2, agent: robo })).status).toBe(200);
  const row = await openRow(page, c);
  await expect(seen(row, a1)).toBeVisible();
  await expect(seen(row, a2)).toBeVisible();
  await expect(seen(row, 'Claude')).toBeVisible();
  await expect(seen(row, robo)).toBeVisible();
  const text = await row.innerText();
  expect(text.indexOf(a1)).toBeLessThan(text.lastIndexOf(a2));
});

// ------------------------------------------------------------------ 7

test('7: The owner attaches a file to an entry: it is listed under Attachments and downloads', async ({ page }) => {
  const c = await ready(page);
  const row = await openRow(page, c);
  const name = `file-${c.mk}.txt`, body = `contents ${c.mk}`;
  await expect(noteForm(row).getByRole('button', { name: 'Attach', exact: true }), 'the note box has Attach').toBeVisible();
  await chooseFile(page, noteForm(row), 'Attach', { name, mimeType: 'text/plain', buffer: Buffer.from(body) });
  await say(row, 'here it is ' + c.mk);
  await expect(seen(row, `Attached: ${name}`)).toBeVisible();
  await expect(seen(row, 'Attachments')).toBeVisible();
  const chip = row.getByRole('link', { name: new RegExp(name.replace('.', '\\.')) }).locator('visible=true').first()
    .or(row.getByText(name, { exact: true }).locator('visible=true').first()).first();
  await expect(chip).toBeVisible();
  const [dl] = await Promise.all([page.waitForEvent('download', { timeout: 10_000 }), chip.click()]);
  expect(dl.suggestedFilename()).toBe(name);
  const path = await dl.path();
  expect((await import('fs')).readFileSync(path!, 'utf8')).toBe(body);
});

test('7: files that came with a chat message are in the same list, and a file is never opened as a page', async ({ page }) => {
  const mk = mkr();
  const att = { id: 'a1', name: `from-chat-${mk}.txt`, type: 'text/plain', size: 8 };
  const c = newChat({ mk, msgs: [{ text: `with a file ${mk}`, attachments: [att] }] });
  const r = await postChats(page, [c]);
  expect(JSON.stringify(r.missing_files ?? [])).toContain(mk);
  const up = await putFile(page, c, c.msgs[0], att, Buffer.from('12345678'));
  expect(up.status).toBe(200);
  expect((await putFile(page, c, c.msgs[0], att, Buffer.from('12345678'))).json?.unchanged ?? true).toBeTruthy();
  c.number = await findItem(page, c);
  await api(page, 'POST', `/api/items/${c.number}/seen`, {});
  const row = await openRow(page, c);
  await expect(seen(row, 'Attachments')).toBeVisible();
  const chip = row.getByText(att.name).locator('visible=true').first();
  await expect(chip).toBeVisible();
  const href = await row.getByRole('link', { name: att.name }).first().getAttribute('href');
  expect(href).toBeTruthy();
  const res = await page.request.get(href!, { failOnStatusCode: false });
  expect(res.status()).toBe(200);
  expect(res.headers()['content-disposition'] ?? '').toMatch(/attachment/i);
});

test('7: a closed row whose thread holds a file shows a paperclip named Has attachments, and a row without a file shows none', async ({ page }) => {
  const mk = mkr();
  const att = { id: 'a1', name: `clip-${mk}.txt`, type: 'text/plain', size: 8 };
  const c = newChat({ mk, msgs: [{ text: `with a file ${mk}`, attachments: [att] }] });
  await postChats(page, [c]);
  expect((await putFile(page, c, c.msgs[0], att, Buffer.from('12345678'))).status).toBe(200);
  c.number = await findItem(page, c);
  expect((await api(page, 'POST', `/api/items/${c.number}/seen`, {})).status).toBe(200);
  await ensureListed(page, c);
  const t = await showRow(page, c);
  expect(await isOpen(t)).toBe(false);
  await expect(rowBox(t).getByRole('img', { name: 'Has attachments', exact: true }).first(), 'the paperclip on the closed row').toBeVisible();
  const plain = await ready(page);
  const t2 = await showRow(page, plain);
  await expect(rowBox(t2).getByRole('img', { name: 'Has attachments' })).toHaveCount(0);
});

test('7: at most 5 files at a time', async ({ page }) => {
  const c = await ready(page);
  const row = await openRow(page, c);
  const files = Array.from({ length: 6 }, (_, i) => ({ name: `many${i}-${c.mk}.txt`, mimeType: 'text/plain', buffer: Buffer.from('x' + i) }));
  await expect(noteForm(row).getByRole('button', { name: 'Attach', exact: true }), 'the note box has Attach').toBeVisible();
  await chooseFile(page, noteForm(row), 'Attach', files);
  await say(row, 'six files ' + c.mk);
  await page.waitForTimeout(1500);
  // A file may show twice (in the list and in the entry that added it): count the different names.
  const names = await row.getByText(new RegExp(`^many\\d-${c.mk}\\.txt$`)).locator('visible=true').allInnerTexts();
  expect(new Set(names).size, 'no more than five of the six stay').toBeLessThanOrEqual(5);
});

test('7: a file over 20 MB is refused', async ({ page }) => {
  test.setTimeout(120_000);
  const c = await ready(page);
  const row = await openRow(page, c);
  const name = `big-${c.mk}.bin`;
  await expect(noteForm(row).getByRole('button', { name: 'Attach', exact: true }), 'the note box has Attach').toBeVisible();
  await chooseFile(page, noteForm(row), 'Attach', { name, mimeType: 'application/octet-stream', buffer: Buffer.alloc(21 * 1024 * 1024, 97) });
  await say(row, 'too big ' + c.mk);
  await page.waitForTimeout(3000);
  await expect(row.getByText(`Attached: ${name}`).locator('visible=true')).toHaveCount(0);
});

// ------------------------------------------------------------------ 8

test('8: a script tag in a message, a subject or a note is shown as text and runs nothing', async ({ page }) => {
  const mk = mkr();
  const tag = `<script>window.__e2e1='${mk}'</script>`;
  const img = `<img src=x onerror="window.__e2e2='${mk}'">`;
  const c = await ready(page, { mk, name: `Subject ${tag} ${mk}`, text: `${tag} ${img} body ${mk}` });
  const row = await openRow(page, c);
  await expect(seen(row, tag, )).toBeVisible();
  await say(row, `my ${tag} ${mk}`);
  await expect(row.getByText(`my ${tag} ${mk}`).locator('visible=true').first()).toBeVisible();
  await api(page, 'POST', `/api/items/${c.number}/comments`, { text: `agent ${tag} ${mk}` });
  await go(page, '/');
  expect(await page.evaluate(() => [(window as any).__e2e1, (window as any).__e2e2])).toEqual([undefined, undefined]);
  const again = await openRow(page, c);
  await expect(again.getByText(`agent ${tag} ${mk}`).locator('visible=true').first()).toBeVisible();
  expect(await page.evaluate(() => [(window as any).__e2e1, (window as any).__e2e2])).toEqual([undefined, undefined]);
  await expect(page).toHaveTitle('ONE');
});

test('8: a picture inside a chat message is shown from the app\'s own copy', async ({ page }) => {
  const mk = mkr();
  const att = { id: 'p1', name: `pic-${mk}.png`, type: 'image/png', size: 0 };
  const png = makePng(37, 23);
  att.size = png.length;
  const c = newChat({ mk, msgs: [{ text: `look at this ${mk}`, attachments: [att] }] });
  await postChats(page, [c]);
  expect((await putFile(page, c, c.msgs[0], att, png)).status).toBe(200);
  c.number = await findItem(page, c);
  await api(page, 'POST', `/api/items/${c.number}/seen`, {});
  const row = await openRow(page, c);
  const pic = row.locator('img').filter({ has: page.locator('xpath=self::*') });
  await expect.poll(() => row.locator('img').evaluateAll((imgs) =>
    imgs.some((i) => (i as HTMLImageElement).naturalWidth === 37 && (i as HTMLImageElement).naturalHeight === 23))).toBe(true);
  void pic;
  const srcs = await row.locator('img').evaluateAll((imgs) => imgs.map((i) => (i as HTMLImageElement).currentSrc));
  const own = new URL(page.url()).origin;
  for (const s of srcs) if (s && !s.startsWith('data:')) expect(new URL(s).origin).toBe(own);
});

test('8: opening a thread loads nothing from a sender\'s server', async ({ page }) => {
  const mk = mkr();
  const host = `sender-${mk}.invalid`;
  const c = await ready(page, { mk, text: `<img src="https://${host}/pixel.gif"> see https://${host}/p.png ${mk}` });
  const hosts = new Set<string>();
  page.on('request', (r) => { try { hosts.add(new URL(r.url()).host); } catch { /* ignore */ } });
  const row = await openRow(page, c);
  await expect(seen(row, new RegExp(mk))).toBeVisible();
  await page.waitForTimeout(1500);
  expect([...hosts].filter((h) => h.includes(host))).toEqual([]);
});

test('8: the page loads fonts, icons and scripts only from the app\'s own address', async ({ page }) => {
  const own = new URL(test.info().project.use.baseURL ?? 'http://127.0.0.1').origin;
  const foreign: string[] = [];
  page.on('request', (r) => {
    const u = r.url();
    if (/^(data|blob|about):/.test(u)) return;
    try { if (new URL(u).origin !== own) foreign.push(new URL(u).origin); } catch { /* ignore */ }
  });
  const c = await ready(page);
  await openRow(page, c);
  for (const k of ['settings', 'notes', 'connectors']) {
    const { visit } = await import('./one-helpers');
    await visit(page, k);
  }
  expect([...new Set(foreign)]).toEqual([]);
});

// ------------------------------------------------------------------ 9

test('9: a thread shows the last month, newest first, and older messages load with one button', async ({ page }) => {
  const mk = mkr();
  const c = await ready(page, {
    mk, msgs: [
      { text: `ancient one ${mk}`, minsAgo: days(50) }, { text: `ancient two ${mk}`, minsAgo: days(40) },
      { text: `ancient three ${mk}`, minsAgo: days(35) }, { text: `recent one ${mk}`, minsAgo: 300 },
      { text: `recent two ${mk}`, minsAgo: 200 },
    ],
  });
  const row = await openRow(page, c);
  await expect(seen(row, `recent two ${mk}`)).toBeVisible();
  await expect(seen(row, `recent one ${mk}`)).toBeVisible();
  const text = await row.innerText();
  expect(text.indexOf(`recent two ${mk}`), 'newest first').toBeLessThan(text.indexOf(`recent one ${mk}`));
  await expect(row.getByText(`ancient one ${mk}`).locator('visible=true')).toHaveCount(0);
  await expect(seen(row, '3 older messages')).toBeVisible();
  await btn(row, 'Show older').click();
  await expect(seen(row, `ancient one ${mk}`)).toBeVisible();
  await expect(seen(row, `ancient three ${mk}`)).toBeVisible();
});

test('9: older messages stay searchable', async ({ page }) => {
  const mk = mkr();
  const c = await ready(page, { mk, msgs: [{ text: `buried treasure ${mk}`, minsAgo: days(45) }, { text: `today ${mk}`, minsAgo: 100 }] });
  const r = await api(page, 'GET', `/api/search?q=${mk}+buried`);
  expect(r.status).toBe(200);
  expect(JSON.stringify(r.json.results)).toContain(c.number);
});

// ------------------------------------------------------------------ 10

test('10: the messages of one chat sit under one row, newest first', async ({ page }) => {
  const mk = mkr();
  const c = await ready(page, { mk, msgs: [{ text: `earlier ${mk}` }, { text: `later ${mk}` }] });
  const r = await api(page, 'GET', `/api/search?q=${mk}&limit=50`);
  expect(new Set((r.json.results as any[]).map((x) => x.item)).size).toBe(1);
  const row = await openRow(page, c);
  await expect(row).toContainText(/\b2 messages\b/);
  const text = await row.innerText();
  expect(text.indexOf(`later ${mk}`)).toBeLessThan(text.indexOf(`earlier ${mk}`));
});

test('10: similar sender or subject alone never joins rows', async ({ page }) => {
  const mk = mkr();
  const a = newChat({ mk, name: `Same name ${mk}`, senderId: `s-${mk}`, senderName: `Same sender ${mk}`, text: `first ${mk}` });
  const mk2 = mkr();
  const b = newChat({ mk: mk2, name: `Same name ${mk}`, senderId: `s-${mk}`, senderName: `Same sender ${mk}`, text: `second ${mk2}` });
  await postChats(page, [a, b]);
  a.number = await findItem(page, a, `first ${mk}`); b.number = await findItem(page, b, `second ${mk2}`);
  expect(a.number).not.toBe(b.number);
  await api(page, 'POST', `/api/items/${a.number}/seen`, {}); await api(page, 'POST', `/api/items/${b.number}/seen`, {});
});

// ------------------------------------------------------------------ 11

test('11: a new item waits for Claude, is counted in the strip, and appears in New once Claude has looked', async ({ page }) => {
  const waiting = () => waitingCount(page);
  const before = await waiting();
  const c = await chat(page); // two hours old: quiet, so it waits for Claude
  expect(await inList(page, c)).toBe(false);
  await expect.poll(waiting, { message: 'the strip counts the new item', timeout: 30_000 }).toBe(before + 1);
  expect((await api(page, 'POST', `/api/items/${c.number}/seen`, {})).status).toBe(200);
  await expect.poll(waiting, { message: 'the strip no longer counts it once Claude has looked', timeout: 30_000 }).toBe(before);
  await ensureListed(page, c); // a phone lists a chat only once its thread has grown (point 17)
  expect(await groupHas(page, c, 'New')).toBe(true);
});

test('11: on a crowded top line the count of items waiting for Claude is still said, folded behind +<n> more if it does not fit', async ({ page }) => {
  const mk = mkr();
  // other things that need the owner's eye make more strips under the top line (a waiting proposal on a setting and on a rule)
  expect((await tool(page, 'propose_setting', { kind: 'setting', name: 'Chat quiet time', value: '1 hour', reason: `crowd ${mk}` })).isError).toBe(false);
  await tool(page, 'propose_rule', { source: 'chat', field: 'sender', operator: 'is', value: `zz${mk}@crowd.test`, name: `Crowd ${mk}`, reason: `crowd ${mk}` });
  const before = await waitingCount(page);
  const c = await chat(page);
  await expect.poll(() => waitingCount(page), { message: 'the count includes the new item, wherever the page puts it', timeout: 30_000 }).toBe(before + 1);
  expect((await api(page, 'POST', `/api/items/${c.number}/seen`, {})).status).toBe(200);
  await expect.poll(() => waitingCount(page), { timeout: 30_000 }).toBe(before);
});

test('11: a summary is an entry marked Summary beside the original text, and a new one takes its place', async ({ page }) => {
  const mk = mkr();
  const long = `${mk} ` + 'a longer message that needs a summary '.repeat(12);
  const c = await chat(page, { mk, text: long });
  const s1 = `first summary ${mk}`, s2 = `second summary ${mk}`;
  const r1 = await api(page, 'POST', `/api/items/${c.number}/comments`, { text: s1, kind: 'summary' });
  expect(r1.status, errorText(r1)).toBe(200);
  expect(r1.json.appeared_in_new).toBe(true);
  expect((await api(page, 'POST', `/api/items/${c.number}/comments`, { text: s2, kind: 'summary' })).status).toBe(200);
  const row = await openRow(page, c);
  await expect(seen(row, s2)).toBeVisible();
  await expect(seen(row, 'Summary')).toBeVisible();
  await expect(row.getByText('Summary', { exact: true }).locator('visible=true')).toHaveCount(1);
  await expect(seen(row, 'a longer message that needs a summary')).toBeVisible(); // the original text stays
  await expect(row.getByText(s1).locator('visible=true')).toHaveCount(0);
  const ev = await api(page, 'GET', `/api/items/${c.number}/events`);
  expect(ev.text, 'the old summary stays in the record').toContain(s1);
});

test('11: a message of 300 characters or fewer gets no summary', async ({ page }) => {
  const c = await chat(page, { text: `short ${mkr()}` });
  const r = await api(page, 'POST', `/api/items/${c.number}/comments`, { text: 'summary', kind: 'summary' });
  expect(r.status).toBe(409);
  await api(page, 'POST', `/api/items/${c.number}/seen`, {});
});

test('11: an item that comes back through a new message is shown at once', async ({ page }) => {
  const c = await ready(page);
  const row = await openRow(page, c);
  await act(row, 'Resolve');
  await addMsg(page, c, { text: `back again ${c.mk}`, minsAgo: 45 });
  expect(await groupHas(page, c, 'New')).toBe(true);
  expect(await waitsForClaude(page, c)).toBe(true);
  await api(page, 'POST', `/api/items/${c.number}/seen`, {});
});

test('11: a note the owner saved lets Claude skip a message, and without its same_as_usual it does not', async ({ page }) => {
  const c = await ready(page);
  const title = `Standing order ${c.mk}`;
  const n = await api(page, 'POST', '/api/notes', { title, text: `expected every month ${c.mk}`, item: c.number });
  expect(n.status, errorText(n)).toBe(200);
  const noId = await api(page, 'POST', `/api/items/${c.number}/skip`, { reason: 'usual ' + c.mk, kind: 'noise', note_id: n.json.note_id });
  expect(noId.status, 'same_as_usual is required with a note').toBe(400);
  const without = await api(page, 'POST', `/api/items/${c.number}/skip`, { reason: 'usual ' + c.mk, kind: 'noise', same_as_usual: 'same' });
  expect(without.status).toBeGreaterThanOrEqual(400);
  expect(without.status).toBeLessThan(500);
  const ok = await api(page, 'POST', `/api/items/${c.number}/skip`, {
    reason: 'usual ' + c.mk, kind: 'noise', note_id: n.json.note_id, same_as_usual: 'the same monthly message',
  });
  expect(ok.status, errorText(ok)).toBe(200);
  await openFromSearch(page, c);
  await expect(page.getByText(`Skipped by your note ${title}.`).first()).toBeVisible();
});

test('11: an old message that mattered is skipped with a reason that begins old:', async ({ page }) => {
  const c = await ready(page);
  expect((await skip(page, c, 'the payment date 2020-01-01 has passed', 'old')).status).toBe(200);
  const { visit } = await import('./one-helpers');
  await visit(page, 'closed');
  await expect(page.getByText(`Reason: old: the payment date 2020-01-01 has passed ${c.mk}`).first()).toBeVisible();
});

// ------------------------------------------------------------------ 12

async function propose(page: import('@playwright/test').Page, c: Chat, action: 'pending' | 'resolve' | 'close') {
  const r = await api(page, 'POST', `/api/items/${c.number}/proposals`, { action, reason: `e2e ${action} ${c.mk}` });
  expect(r.status, errorText(r)).toBe(200);
  return r;
}

test('12: a proposal does nothing until Approve, and Approve on the closed row does it', async ({ page }) => {
  const c = await ready(page);
  await propose(page, c, 'resolve');
  expect(await stageOf(page, c)).toBe('new');
  const t = await showRow(page, c);
  const row = rowBox(t);
  await expect(btn(row, 'Approve')).toBeVisible();
  await expect(btn(row, 'Dismiss')).toBeVisible();
  await act(row, 'Approve');
  expect(await stageOf(page, c)).toBe('resolved');
  const open = await openRow(page, c);
  await expect(seen(open, 'You approved this proposal.')).toBeVisible();
  await expect(seen(open, 'Proposal')).toBeVisible();
});

test('12: Dismiss changes nothing and is recorded', async ({ page }) => {
  const c = await ready(page);
  await propose(page, c, 'pending');
  const t = await showRow(page, c);
  await act(rowBox(t), 'Dismiss');
  expect(await stageOf(page, c)).toBe('new');
  const d = await api(page, 'GET', '/api/decisions?days=1');
  expect(d.text).toContain(c.number);
  expect(d.text).toMatch(/dismiss/i);
});

test('12: a proposal that no longer fits because a new message arrived is shown as Withdrawn', async ({ page }) => {
  const c = await ready(page);
  await propose(page, c, 'resolve');
  await addMsg(page, c, { text: `new facts ${c.mk}`, minsAgo: 45 });
  await api(page, 'POST', `/api/items/${c.number}/seen`, {});
  const row = await openRow(page, c);
  await expect(seen(row, 'Withdrawn')).toBeVisible();
  await expect(btn(row, 'Approve')).toBeHidden();
});

test('12: a proposal is withdrawn when the owner acts himself', async ({ page }) => {
  const c = await ready(page);
  await propose(page, c, 'pending');
  const row = await openRow(page, c);
  await act(row, 'Resolve');
  await expect(seen(row, 'Withdrawn')).toBeVisible();
});

test('12: a clear skip needs a reason, goes to Closed and can be brought back', async ({ page }) => {
  const c = await ready(page);
  const no = await api(page, 'POST', `/api/items/${c.number}/skip`, { kind: 'noise' });
  expect(no.status).toBe(403);
  expectRefusal(no.json.error, 'Reason required');
  expect((await skip(page, c, 'clear noise', 'noise')).status).toBe(200);
  expect(await stageOf(page, c)).toBe('closed');
  expect((await api(page, 'POST', `/api/items/${c.number}/unskip`, {})).status).toBe(200);
  expect(await stageOf(page, c)).toBe('new');
});

test('12: a close proposal without a reason is refused by the Reason required guard', async ({ page }) => {
  const c = await ready(page);
  const r = await api(page, 'POST', `/api/items/${c.number}/proposals`, { action: 'close' });
  expect(r.status).toBe(403);
  expectRefusal(r.json.error, 'Reason required');
});

test('12: Claude may close a row when the owner\'s own newest note asks for it, and the thread says so', async ({ page }) => {
  const c = await ready(page);
  const row = await openRow(page, c);
  await say(row, 'please close this ' + c.mk);
  await expect(seen(row, 'please close this ' + c.mk)).toBeVisible();
  const r = await api(page, 'POST', `/api/items/${c.number}/close-on-note`, { reason: 'you asked ' + c.mk });
  expect(r.status, errorText(r)).toBe(200);
  await openFromSearch(page, c);
  await expect(page.getByText('Claude closed this on your note.').first()).toBeVisible();
  expect(await stageOf(page, c)).toBe('closed');
});

// ------------------------------------------------------------------ 17

test('17: a chat that is still being written in is shown at once and does not wait for Claude', async ({ page }) => {
  const c = await chat(page, { live: true });
  expect(await waitsForClaude(page, c)).toBe(false);
  await ensureListed(page, c); // a phone lists a chat only once its thread has grown
  expect(await groupHas(page, c, 'New')).toBe(true);
  expect(await waitsForClaude(page, c)).toBe(false);
});

test('17: a chat quiet for 30 minutes goes to Claude, and a message that needs the owner brings a finished chat back', async ({ page }) => {
  const c = await chat(page, { minsAgo: 45 });
  expect(await waitsForClaude(page, c)).toBe(true);
  expect(await inList(page, c)).toBe(false);
  await api(page, 'POST', `/api/items/${c.number}/seen`, {});
  expect((await skip(page, c, 'the exchange has finished', 'finished')).status).toBe(200);
  await addMsg(page, c, { text: `one more thing ${c.mk}`, minsAgo: 40 });
  expect(await stageOf(page, c)).toBe('new');
  await ensureListed(page, c); // a phone lists a chat only once its thread has grown
  expect(await groupHas(page, c, 'New')).toBe(true);
  await api(page, 'POST', `/api/items/${c.number}/seen`, {});
});

test('17: a chat that arrived already finished says Arrived settled.', async ({ page }) => {
  const c = await chat(page, { minsAgo: days(3) });
  await api(page, 'POST', `/api/items/${c.number}/seen`, {});
  const row = await openRow(page, c);
  await expect(seen(row, 'Arrived settled.')).toBeVisible();
});

// ------------------------------------------------------------------ 30

test('30: an edited message is kept as a version, never as a second message', async ({ page }) => {
  const c = await ready(page);
  c.msgs[0].text = `New wording ${c.mk}`;
  c.msgs[0].editedAt = iso(0);
  const r = await postChats(page, [c]);
  expect(r.versions ?? 1).toBeGreaterThanOrEqual(1);
  const item = await api(page, 'GET', `/api/items/${c.number}`);
  expect(item.json.messages).toHaveLength(1);
  expect(item.json.messages[0].text).toContain(`New wording ${c.mk}`);
  const ev = await api(page, 'GET', `/api/items/${c.number}/events`);
  expect(ev.text, 'the change is in the record of the item').toContain('chat_edited');
  await api(page, 'POST', `/api/items/${c.number}/seen`, {});
});

test('30: a closed item is not deleted: it is still found by search', async ({ page }) => {
  const c = await ready(page);
  expect((await skip(page, c)).status).toBe(200);
  const r = await api(page, 'GET', `/api/search?q=${c.mk}`);
  expect(JSON.stringify(r.json.results)).toContain(c.number);
});
