// Acceptance tests for apps/one/SPEC.md points 15, 16, 18, 20, 21, 22, 23, 25, 26 and 28 on the pages:
// Refresh, tokens, agenda, iCloud, import rules, guards, Connectors, People, search and Notes, Settings.
//
// Nothing here changes a setting, connects, disconnects or tests a connection, or presses `New token`,
// `Sign in`, `Test now`, `Remove` or `Connect`. Where a criterion needs that, only what is shown is checked.
// Every test makes its data under its own run name (one-helpers.ts), and the run is removed when the test ends.
// Not tested here, and why:
//  - 18 `Show earlier`, the filters hiding everything (`No events match.`), a filter's remembered choice,
//    day headings Today and Tomorrow, `Me` for entries with no person: they need calendar entries, which a
//    browser test cannot put in (the calendars are read by the app itself).
//  - 20 a wrong password (`Apple refused the sign-in...`): a wrong sign-in would hit Apple on every run of
//    every browser project; per-calendar `Show in agenda` and `Whose`: need a connected account.
//  - 21 adding, switching and testing a rule by hand: changes a setting that is not marked as the test's (only a
//    proposed rule, which carries the run's mark, is approved).
//  - 23 `Sign in`, `Test now`, `Remove` and the failing-source strip and notices (24): need a failing source.
//  - 28 the 14-day close, the first import depth and the settings' effects: need waiting or a real import.
import { expect, type Locator, type Page } from '@playwright/test';
import {
  test, mkr, isPhone, go, api, token, ready, chat, newChat, postChats, findItem, visit, openRow, seen, sidebar,
  agendaButton, openAgenda, findPerson, isLaptop, connectionsPane, chooseFile, makePng, btn, groupHas, toggleOf, isOpen, putFile, days, connection, skip, expectRefusal, expandGroup, moreUntil, tool, press,
} from './one-helpers';

const text = (l: Locator) => l.first().innerText();

// ------------------------------------------------------------------ 15 and 16

test('15: Refresh fetches everything now and then says when', async ({ page }) => {
  test.setTimeout(120_000);
  await go(page, '/');
  const b = page.getByRole('button', { name: 'Refresh', exact: true }).first();
  await expect(b).toBeVisible();
  await b.click();
  await b.click({ timeout: 1500 }).catch(() => {}); // pressing again while it runs does nothing new
  await expect(page.getByText(/^Refreshed \S+/).first()).toBeVisible({ timeout: 90_000 });
  // it says what happened, not only that it ran: how many new messages, and each source that failed, by name
  await expect(page.getByText(/^Refreshed \S+\s*(?:[,·]\s*)?(?:\d+ new|nothing new)/).first()).toBeVisible({ timeout: 90_000 });
  const line = await page.getByText(/^Refreshed \S+/).last().innerText();
  for (const f of line.matchAll(/(\S[^:]*) failed: (.+)/g)) expect(f[2].trim().length, `${f[1]} failed with an error in words`).toBeGreaterThan(3);
});

test('16: Connectors has the Agent token and Bridge token sections, each with Copy and New token', async ({ page }) => {
  await visit(page, 'connectors');
  for (const name of ['Agent token', 'Bridge token']) {
    const section = await connection(page, name);
    await expect(section.getByRole('button', { name: 'Copy', exact: true })).toBeVisible();
    await expect(section.getByRole('button', { name: 'New token', exact: true })).toBeVisible();
  }
  expect(await token(page, 'Agent')).not.toBe(await token(page, 'Bridge'));
});

// ------------------------------------------------------------------ 18

test('18: the Agenda button shows and hides the sidebar titled Agenda, with the two filters', async ({ page }) => {
  await go(page, '/');
  const side = sidebar(page);
  const was = await side.isVisible().catch(() => false);
  // On a laptop the agenda is always there and has no button (SPEC 1); the button is for a phone.
  if (isLaptop(page)) expect(await agendaButton(page).isVisible().catch(() => false), 'no Agenda button on a laptop').toBe(false);
  else if (!was) await agendaButton(page).click();
  await expect(side).toBeVisible();
  await expect(side.getByRole('heading', { name: 'Agenda', exact: true }).or(side.getByText('Agenda', { exact: true })).first()).toBeVisible();
  for (const f of ['Person', 'Calendar']) {
    const first = await side.getByLabel(f).first().evaluate((el) =>
      el.tagName === 'SELECT' ? (el as HTMLSelectElement).options[0].text.trim() : (el.textContent ?? '').trim());
    expect(first, f).toBe('All');
  }
  if (isLaptop(page)) {
    await page.reload();
    await expect(side).toBeVisible(); // always there
  }
});

test('18: the agenda shows where the owner is in time: Today, a Now line with the time, and a Now button', async ({ page }) => {
  const side = await openAgenda(page);
  await expect(side.getByRole('heading', { name: /^Today/ }).first()).toBeVisible();
  await expect(side.getByText(/^Now \d{1,2}:\d{2}$/).first()).toBeVisible();
  await expect(side.getByRole('button', { name: 'Now', exact: true })).toBeVisible();
  // the minutes are the present minutes (the page may keep Prague time, whole hours away from the test's)
  const line = (await side.getByText(/^Now \d{1,2}:\d{2}$/).first().innerText()).replace('Now ', '');
  const m = Number(line.split(':')[1]), now = new Date().getMinutes();
  expect(Math.min(Math.abs(m - now), 60 - Math.abs(m - now)), 'the time is the present time').toBeLessThanOrEqual(3);
});

test('18: with no events the agenda says No events., and the first choice of each filter is All', async ({ page }) => {
  const side = await openAgenda(page);
  const events = await api(page, 'GET', '/api/agenda');
  if ((events.json.events as any[]).length === 0) await expect(side.getByText('No events.').first()).toBeVisible();
  for (const f of ['Person', 'Calendar']) await expect(side.getByLabel(f).first()).toBeVisible();
});

test('18: the agenda is read as text through the API, with a bad date refused', async ({ page }) => {
  const r = await api(page, 'GET', '/api/agenda');
  expect(r.status).toBe(200);
  expect(Array.isArray(r.json.events)).toBe(true);
  expect(Array.isArray(r.json.todos)).toBe(true);
  expect((await api(page, 'GET', '/api/agenda?from=not-a-date')).status).toBe(400);
});

// ------------------------------------------------------------------ 20

test('20: Connectors has an iCloud Calendar section that offers Connect or shows who is connected', async ({ page }) => {
  await visit(page, 'connectors');
  const section = await connection(page, 'iCloud Calendar');
  const connected = await section.getByText(/^Connected as /).first().isVisible().catch(() => false);
  if (connected) {
    await expect(section.getByRole('button', { name: 'Disconnect', exact: true })).toBeVisible();
  } else {
    await expect(section.getByLabel('Apple ID')).toBeVisible();
    await expect(section.getByLabel('App-specific password')).toBeVisible();
    await expect(section.getByRole('button', { name: 'Connect', exact: true })).toBeVisible();
  }
});

// ------------------------------------------------------------------ 21

test('21: Settings has Import rules with the two starting rules, what each dropped, and the buttons', async ({ page }) => {
  await visit(page, 'settings');
  await expect(page.getByText('Import rules', { exact: true }).first()).toBeVisible();
  for (const name of ['GitHub pipeline mails', 'Muted group chats']) {
    const rule = page.getByText(name, { exact: true }).first().locator('xpath=ancestor::li[1]');
    await expect(rule.getByText(/Dropped \(\d+\)/).first(), name).toBeAttached();
    await expect(rule.getByRole('switch', { name: new RegExp('^Enabled') }), name).toBeVisible();
    await expect(rule.getByRole('button', { name: 'Test rule', exact: true }), name).toBeVisible();
  }
  await expect(page.getByRole('button', { name: 'Add rule', exact: true }).first()).toBeVisible();
});

test('21: a muted group chat that was dropped is listed with Import again, which brings it back', async ({ page }) => {
  const c = newChat({ group: true, muted: true, name: `Muted ${mkr()}` });
  await postChats(page, [c]);
  await visit(page, 'settings');
  const rule = page.getByText('Muted group chats', { exact: true }).first().locator('xpath=ancestor::li[1]');
  await rule.getByText(/^Dropped \(\d+\)$/).click(); // the list of what it dropped opens on a click
  const entry = rule.getByText(c.mk).first().locator('xpath=ancestor::li[1]');
  await expect(entry).toBeVisible();
  await entry.getByRole('button', { name: 'Import again', exact: true }).click();
  await postChats(page, [c], true); // the bridge answers with the message once more
  c.number = await findItem(page, c);
  expect(c.number).toBeTruthy();
});

test('21: a proposed rule is marked Proposal with Approve and Dismiss, and Dismiss removes it', async ({ page }) => {
  const mk = mkr();
  const r = await api(page, 'POST', '/api/rule-proposals', {
    field: 'sender', value: `bulk-${mk}@example.invalid`, name: `Rule ${mk}`, reason: `214 mails in three years, none opened, none answered ${mk}`,
  });
  expect(r.status, r.text.slice(0, 300)).toBe(200);
  await visit(page, 'settings');
  const card = page.getByText(mk).first().locator('xpath=ancestor::*[.//button[normalize-space()="Dismiss"]][1]');
  await expect(card).toContainText('Proposal');
  await expect(btn(card, 'Approve')).toBeVisible();
  await btn(card, 'Dismiss').click();
  await expect(page.getByText(mk)).toHaveCount(0);
});

// ------------------------------------------------------------------ 22

test('22: Settings lists the guards with their state and how often they refused', async ({ page }) => {
  await visit(page, 'settings');
  await expect(page.getByText('Guards', { exact: true }).first()).toBeVisible();
  const switchable = ['Reason required', 'Protected people', 'Blocked targets'];
  const fixed = ['Approval only', 'Exact text', 'Same thread', 'Send once', 'Approval required', 'Actor by channel'];
  for (const name of [...switchable, ...fixed]) {
    const g = page.getByText(name, { exact: true }).first().locator('xpath=ancestor::*[contains(., "Refused recently:")][1]');
    await expect(g, name).toContainText(/Refused recently: \d+/);
    if (fixed.includes(name)) await expect(g, name).toContainText('Always on');
    else await expect(g, name).toContainText(/\b(On|Off)\b/);
  }
});

// ------------------------------------------------------------------ 23

test('23: the top bar names ONE, shows a status dot with its label, and the dot opens Connectors', async ({ page }) => {
  await go(page, '/');
  await expect(page.getByRole('link', { name: 'ONE', exact: true }).or(page.getByText('ONE', { exact: true })).first()).toBeVisible();
  // On a laptop the connections are the block at the bottom of the left pane (SPEC 1): the lines of Gmail, Office mail
  // and the chat bridge show only that connection's rows, the Claude line leads to Connectors; the dot with its label is the
  // phone's.
  const dot = isLaptop(page)
    ? connectionsPane(page).getByRole('link', { name: /^Claude/ })
    : page.getByRole('link', { name: /^Connectors: (all fine|needs a look|a source has stopped)$/ })
      .or(page.getByRole('button', { name: /^Connectors: (all fine|needs a look|a source has stopped)$/ })).first();
  await expect(dot).toBeVisible();
  await dot.click();
  await expect(page.getByRole('heading', { name: 'Connectors', exact: true }).first()).toBeVisible();
  await expect(page).toHaveTitle(/Connectors/);
});

test('1: on a phone, notices that do not fit the top strip are folded behind `+<n> more`, and a tap on it opens Connectors', async ({ page }) => {
  // A browser test cannot make the notices overflow (that needs a failing source), so this looks only when the link is
  // there: on a crowded top line it says `+<n> more` and leads to Connectors; when the notices fit there is nothing to check.
  test.skip(isLaptop(page), 'the folded strip is the phone\'s');
  await go(page, '/');
  await page.getByText(/^Environment: /).first().waitFor({ state: 'visible', timeout: 10_000 }).catch(() => {});
  const more = page.getByRole('link', { name: /^\+\d+ more/ }).or(page.getByRole('button', { name: /^\+\d+ more/ })).locator('visible=true').first();
  if (!(await more.isVisible().catch(() => false))) return;
  expect((await more.innerText()).trim(), 'the link says `+<n> more`').toMatch(/^\+\d+ more$/);
  await more.click();
  await expect(page.getByRole('heading', { name: 'Connectors', exact: true }).first()).toBeVisible();
  await expect(page).toHaveTitle(/Connectors/);
});

test('23: the Connectors page has a link Back to the queue', async ({ page }) => {
  await visit(page, 'connectors');
  const back = page.getByRole('link', { name: 'Back to the queue', exact: true }).first();
  await expect(back).toBeVisible();
  await back.click();
  await expect(page).toHaveTitle('ONE');
});

test('23: every connection shows a status, the sections and the known limitations are there', async ({ page }) => {
  await ready(page); // the bridge has been heard from
  await visit(page, 'connectors');
  const statuses = /(OK|Stale|Error|Needs login|Not configured)/;
  for (const name of ['Gmail', 'Office mail', 'Chat bridge', 'Google Calendar', 'Outlook Calendar', 'iCloud Calendar', 'Notify', 'Claude']) {
    const region = page.getByRole('region', { name, exact: true });
    await expect(region, `${name} has a row`).toBeVisible();
    await expect(region.locator('summary').first(), name).toContainText(statuses);
  }
  // Every connection that has been read says when (a bridge that was never heard from is `Not configured` and has no time).
  const bridge = await connection(page, 'Chat bridge');
  if (!/Not configured/.test(await bridge.locator('summary').first().innerText())) {
    await expect(bridge.getByText(/^Last read: /).first()).toBeVisible();
  }
  for (const s of ['Recent fetches', 'Last 14 days', 'Known limitations']) await expect(page.getByText(s, { exact: true }).first()).toBeVisible();
  for (const name of ['Gmail', 'Office mail']) await connection(page, name);
  for (const l of [
    'Chats arrive only while the Mac is on and Beeper is running.',
    'A network that has logged itself out delivers nothing until the owner logs in on the Mac.',
    'Phone calls are not covered.',
    'Mail deleted before the first import cannot be imported.',
    'Only the notification mails are seen. ONE never signs in to the data box, and a notification is not the delivery itself.',
  ]) await expect(page.getByText(l).first()).toBeVisible();
});

test('23: choosing a connection shows only the rows that came through it, with Only <connection> and Show all, and the choice is not kept', async ({ page }) => {
  const c = await ready(page, { net: 'messenger' }); // comes through the chat bridge
  await visit(page, 'connectors');
  const bridge = page.getByRole('region', { name: 'Chat bridge', exact: true });
  await bridge.getByRole('link', { name: 'Chat bridge', exact: true }).first().click();
  const only = page.getByText(/^Only .+/).first();
  await expect(only).toBeVisible();
  await expect(page.getByRole('button', { name: 'Show all', exact: true }).first()).toBeVisible();
  const row = toggleOf(page, c);
  if (!isLaptop(page)) await expandGroup(page, 'New');
  await expect(await moreUntil(page, row, 4000), 'a row that came through the chat bridge is listed').toBe(true);
  // another connection shows none of the bridge's rows
  await visit(page, 'connectors');
  await page.getByRole('region', { name: 'Office mail', exact: true }).getByRole('link', { name: 'Office mail', exact: true }).first().click();
  await expect(page.getByText(/^Only .+/).first()).toBeVisible();
  if (!isLaptop(page)) await expandGroup(page, 'New');
  await expect(row, 'the bridge row is not among Office mail\'s').toHaveCount(0);
  // Show all brings everything back
  await page.getByRole('button', { name: 'Show all', exact: true }).first().click();
  await expect(page.getByText(/^Only .+/)).toHaveCount(0);
  // and the choice is not kept after the page is left
  await visit(page, 'connectors');
  await bridge.getByRole('link', { name: 'Chat bridge', exact: true }).first().click();
  await expect(page.getByText(/^Only .+/).first()).toBeVisible();
  await visit(page, 'settings');
  await go(page, '/');
  await expect(page.getByText(/^Only .+/)).toHaveCount(0);
});

test('22: Claude can propose a change to a setting: it shows what changes and why, changes nothing before Approve, and a guard that is always on cannot be proposed off', async ({ page }) => {
  const mk = mkr();
  const why = `because e2e ${mk}`;
  const r = await tool(page, 'propose_setting', { kind: 'setting', name: 'Chat quiet time', value: '1 hour', reason: why });
  expect(r.isError, r.text).toBe(false);
  await visit(page, 'settings');
  const card = page.getByText(why).first().locator('xpath=ancestor::*[.//button[normalize-space()="Approve"]][1]');
  await expect(card).toBeVisible();
  await expect(card).toContainText('Chat quiet time');
  await expect(card).toContainText('1 hour');
  await expect(card.getByRole('button', { name: 'Dismiss', exact: true })).toBeVisible();
  // nothing changed before Approve
  await expect(page.getByLabel('Chat quiet time', { exact: true }).locator('option:checked')).toHaveText('30 minutes');
  await press(page, card.getByRole('button', { name: 'Dismiss', exact: true }));
  await visit(page, 'settings');
  await expect(page.getByText(why)).toHaveCount(0);
  // a guard that is always on cannot be proposed off
  const off = await tool(page, 'propose_setting', { kind: 'guard', name: 'Approval only', value: 'Off', reason: `no ${mk}` });
  expect(off.isError, off.text).toBe(true);
  // no agent can change a setting itself
  const direct = await api(page, 'POST', '/api/settings', { name: 'Chat quiet time', value: '45 minutes' });
  expect([401, 403, 404, 405]).toContain(direct.status);
});

test('23: a picture that is not an image, or is over 300 KB, is refused', async ({ page }) => {
  await visit(page, 'connectors');
  const row = await connection(page, 'Gmail');
  await expect(row.getByRole('button', { name: 'Set picture', exact: true })).toBeVisible();
  await chooseFile(page, row, 'Set picture', { name: 'notes.txt', mimeType: 'text/plain', buffer: Buffer.from('not a picture') });
  await expect(page.getByText('Use a PNG, JPEG or WebP picture under 300 KB.').first()).toBeVisible();
  await visit(page, 'connectors');
  const again = await connection(page, 'Gmail');
  const big = Buffer.concat([makePng(8, 8), Buffer.alloc(320 * 1024, 0)]);
  await chooseFile(page, again, 'Set picture', { name: 'big.png', mimeType: 'image/png', buffer: big });
  await expect(page.getByText('Use a PNG, JPEG or WebP picture under 300 KB.').first()).toBeVisible();
});

test('23: The owner can set his own picture for a source and remove it again', async ({ page }) => {
  await visit(page, 'connectors');
  const row = await connection(page, 'Chat bridge');
  await expect(row.getByRole('button', { name: 'Set picture', exact: true }).or(row.getByRole('button', { name: 'Remove picture', exact: true })).first()).toBeVisible();
  if (await row.getByRole('button', { name: 'Remove picture', exact: true }).isVisible().catch(() => false)) {
    test.info().annotations.push({ type: 'note', description: 'a picture is already set on this source: left alone' });
    return;
  }
  try {
    await chooseFile(page, row, 'Set picture', { name: 'pic.png', mimeType: 'image/png', buffer: makePng(16, 16) });
    await expect((await connection(page, 'Chat bridge')).getByRole('button', { name: 'Remove picture', exact: true })).toBeVisible();
  } finally {
    const remove = (await connection(page, 'Chat bridge')).getByRole('button', { name: 'Remove picture', exact: true });
    if (await remove.isVisible().catch(() => false)) await remove.click();
  }
  await expect((await connection(page, 'Chat bridge')).getByRole('button', { name: 'Remove picture', exact: true })).toBeHidden();
});

// ------------------------------------------------------------------ 25

test('25: People is reached from Settings, lists names, and a person opens with their fields', async ({ page }) => {
  const mk = mkr();
  const c = await ready(page, { mk, senderName: `Person ${mk}`, senderId: `p-${mk}` });
  await visit(page, 'settings');
  await expect(page.getByRole('link', { name: 'People', exact: true }).first()).toBeVisible();
  await findPerson(page, `Person ${mk}`);
  await expect(page.getByRole('heading', { name: 'People', exact: true }).first()).toBeVisible();
  const name = page.getByText(`Person ${mk}`).first();
  await expect(name).toBeVisible();
  await expect(page.getByLabel('I call them'), 'a person is a name in the list until it is opened').toHaveCount(0);
  await name.click();
  for (const label of ['Name', 'I call them', 'Who they are to me', 'Addresses and handles']) {
    await expect(page.getByLabel(label).first(), label).toBeVisible();
  }
  await expect(page.getByLabel('Always show me').first()).toBeVisible();
  for (const b of ['Save', 'Set picture']) await expect(page.getByRole('button', { name: b, exact: true }).first(), b).toBeVisible();
});

test('25: the People page has a link Back to the queue', async ({ page }) => {
  await visit(page, 'people');
  await expect(page.getByRole('link', { name: 'Back to the queue', exact: true }).first()).toBeVisible();
});

test('25: The owner can set a person\'s picture and remove it again', async ({ page }) => {
  const mk = mkr();
  await ready(page, { mk, senderName: `Pictured ${mk}`, senderId: `pic-${mk}` });
  await findPerson(page, `Pictured ${mk}`);
  await page.getByText(`Pictured ${mk}`).first().click();
  await expect(page.getByRole('button', { name: 'Remove picture', exact: true })).toHaveCount(0);
  await page.locator('input[type=file]').first().setInputFiles({ name: 'pic.png', mimeType: 'image/png', buffer: makePng(16, 16) });
  await page.getByRole('button', { name: 'Set picture', exact: true }).click();
  await expect(page.getByRole('button', { name: 'Remove picture', exact: true })).toBeVisible();
  await page.getByRole('button', { name: 'Remove picture', exact: true }).click();
  await expect(page.getByRole('button', { name: 'Remove picture', exact: true })).toHaveCount(0);
});

test('25: what the owner calls a person shows on their messages, and the owner can correct it on the People page', async ({ page }) => {
  const mk = mkr();
  const c = await ready(page, { mk, senderName: `Ondra ${mk}`, senderId: `t-${mk}` });
  const set = await api(page, 'POST', '/api/people/details', { person: `Ondra ${mk}`, call_name: `Tarik${mk}`, relation: `joiner ${mk}` });
  expect(set.status).toBe(200);
  const row = await openRow(page, c);
  await expect(seen(row, `Tarik${mk}`)).toBeVisible();
  await findPerson(page, mk);
  await page.getByRole('link', { name: new RegExp(`Tarik${mk}|Ondra ${mk}`) }).first().click(); // listed by the name the owner calls them, or by name

  await expect(page.getByLabel('I call them').first()).toHaveValue(`Tarik${mk}`);
  await expect(page.getByLabel('Who they are to me').first()).toHaveValue(`joiner ${mk}`);
  await page.getByLabel('I call them').first().fill(`Taro${mk}`);
  await page.getByRole('button', { name: 'Save', exact: true }).first().click();
  await expect.poll(async () => {
    const p = await api(page, 'GET', `/api/people?q=${mk}`);
    return (p.json.people as any[]).find((x) => x.name === `Ondra ${mk}`)?.call_name;
  }).toBe(`Taro${mk}`);
});

test('25: a join proposal shows as a card on People and Settings, and Approve joins the two', async ({ page }) => {
  const mk = mkr();
  await ready(page, { mk, senderName: `Lenka ${mk}`, senderId: `w-${mk}`, net: 'whatsapp' });
  await ready(page, { mk: mk + 'b', senderName: `Lenka mail ${mk}`, senderId: `m-${mk}`, net: 'sms' });
  const r = await api(page, 'POST', '/api/people/proposals', { action: 'join', person: `Lenka ${mk}`, with: `Lenka mail ${mk}`, reason: 'same person ' + mk });
  expect(r.status, r.text.slice(0, 300)).toBe(200);
  for (const where of ['settings', 'people']) {
    if (where === 'people') await findPerson(page, mk); else await visit(page, where);
    const card = page.getByText(/^Join /).filter({ hasText: `Lenka ${mk}` }).filter({ hasText: `Lenka mail ${mk}` }).first();
    await expect(card, where).toBeVisible();
  }
  const card = page.getByText(/^Join /).filter({ hasText: `Lenka ${mk}` }).first().locator('xpath=ancestor::*[.//button[normalize-space()="Approve"]][1]');
  await expect(btn(card, 'Dismiss')).toBeVisible();
  await btn(card, 'Approve').click();
  await expect.poll(async () => {
    const p = await api(page, 'GET', `/api/people?q=m-${mk}`);
    return (p.json.people as any[]).map((x) => x.name);
  }).toEqual([`Lenka ${mk}`]);
});

test('25: a Stop always showing card can be dismissed, and Approve switches the protection off', async ({ page }) => {
  const mk = mkr();
  await ready(page, { mk, senderName: `Guarded ${mk}`, senderId: `g-${mk}` });
  await api(page, 'POST', '/api/people/details', { person: `Guarded ${mk}`, always_show: true });
  const shown = async () => ((await api(page, 'GET', `/api/people?q=${mk}`)).json.people as any[]).find((x) => x.name === `Guarded ${mk}`).always_show;
  for (const [button, after] of [['Dismiss', true], ['Approve', false]] as const) {
    const r = await api(page, 'POST', '/api/people/proposals', { action: 'stop_always_show', person: `Guarded ${mk}`, reason: 'e2e ' + mk });
    expect(r.status, r.text.slice(0, 300)).toBe(200);
    await visit(page, 'settings');
    const card = page.getByText(`Stop always showing Guarded ${mk}`).first().locator('xpath=ancestor::*[.//button[normalize-space()="Approve"]][1]');
    await expect(card).toBeVisible();
    await btn(card, button).click();
    await expect.poll(shown).toBe(after);
  }
});

// ------------------------------------------------------------------ 26

test('26: the search box finds a chat, says what it is and opens its row; unknown words give No results.', async ({ page }) => {
  const mk = mkr();
  const c = await ready(page, { mk, text: `find me ${mk}` });
  await go(page, '/');
  const box = page.getByPlaceholder('Search mail, chats, events and notes').first();
  await box.fill(mk);
  await box.press('Enter');
  const hit = page.getByText('Chat', { exact: true }).locator('visible=true').first();
  await expect(hit).toBeVisible();
  await page.getByRole('link', { name: new RegExp(mk) }).first().click(); // the result's title opens its row
  await expect(page.getByText(c.number, { exact: true }).first()).toBeVisible();
  await expect(page.getByPlaceholder('Add a note or ask Claude…').first()).toBeVisible();
  await go(page, '/');
  await box.fill('zzzz' + mkr());
  await box.press('Enter');
  await expect(page.getByText('No results.').first()).toBeVisible();
});

test('26: a comment and a saved note are found and marked Comment and Note', async ({ page }) => {
  const mk = mkr();
  const c = await ready(page, { mk });
  const w1 = `commentword${mk}`, w2 = `noteword${mk}`;
  await api(page, 'POST', `/api/items/${c.number}/comments`, { text: `remark ${w1}` });
  await api(page, 'POST', '/api/notes', { title: `Standing ${w2}`, text: `remember ${w2}` });
  await go(page, '/');
  const box = page.getByPlaceholder('Search mail, chats, events and notes').first();
  await box.fill(w1);
  await box.press('Enter');
  await expect(page.getByText('Comment', { exact: true }).locator('visible=true').first()).toBeVisible();
  await go(page, '/');
  await box.fill(w2);
  await box.press('Enter');
  await expect(page.getByText('Note', { exact: true }).locator('visible=true').first()).toBeVisible();
});

test('26: The owner saves a note on the Notes page', async ({ page }) => {
  const mk = mkr();
  const c = await ready(page, { mk }); // the note is about this item, so it is removed with the test's run
  await go(page, '/');
  await page.getByRole('link', { name: 'Notes', exact: true }).first().click();
  await expect(page.getByRole('heading', { name: 'Notes', exact: true }).first()).toBeVisible();
  await expect(page).toHaveTitle(/Notes/);
  await page.getByRole('button', { name: 'Add note', exact: true }).or(page.getByRole('link', { name: 'Add note', exact: true })).first().click();
  await page.getByLabel('Title').first().fill(`Title ${mk}`);
  await page.getByLabel('Note', { exact: true }).first().fill(`Body of the note ${mk}`);
  await page.getByLabel(/^About an item/).first().fill(c.number);
  await page.getByRole('button', { name: 'Save', exact: true }).first().click();
  await expect(page.getByText(`Title ${mk}`).first()).toBeVisible();
  await visit(page, 'notes');
  await expect(page.getByText(`Title ${mk}`).first()).toBeVisible();
  const found = await api(page, 'GET', `/api/search?q=${encodeURIComponent(`Body of the note ${mk}`)}`);
  expect(JSON.stringify(found.json.results)).toContain(`Title ${mk}`);
});

// ------------------------------------------------------------------ 28

test('28: Settings has the settings with their defaults, and the Proposals from Claude section', async ({ page }) => {
  await visit(page, 'settings');
  await expect(page).toHaveTitle(/Settings/);
  const defaults: [string, string, RegExp][] = [
    ['Sync interval', '5', /minute/i], ['First import depth', '1', /year/i], ['Review window', '1', /month/i],
    ['Chat quiet time', '30', /minute/i], ['Bridge silence before a notice', '15', /minute/i],
    ['Agenda window back', '1', /month/i], ['Agenda window ahead', '1', /month/i], ['Quiet time before closing', '14', /day/i],
  ];
  for (const [label, num, unit] of defaults) {
    const got = await page.evaluate((l) => {
      const els = [...document.querySelectorAll('*')].filter((e) =>
        [...e.childNodes].some((n) => n.nodeType === 3 && n.textContent?.trim() === l));
      for (const el of els) {
        let n: HTMLElement | null = el as HTMLElement;
        for (let i = 0; i < 5 && n; i++, n = n.parentElement) {
          const ctl = n.querySelectorAll('input:not([type=checkbox]):not([type=hidden]), select, textarea');
          if (ctl.length) {
            const clone = n.cloneNode(true) as HTMLElement;
            clone.querySelectorAll('select, option').forEach((s) => s.remove());
            const vals = [...ctl].map((c) => c.tagName === 'SELECT'
              ? (c as HTMLSelectElement).selectedOptions[0]?.text ?? '' : (c as HTMLInputElement).value);
            return `${clone.innerText} ${vals.join(' ')}`;
          }
        }
      }
      return '';
    }, label);
    expect(got, label).not.toBe('');
    expect(got, label).toMatch(new RegExp(`\\b${num}\\b`));
    expect(got, label).toMatch(unit);
  }
  await expect(page.getByText('Proposals from Claude', { exact: true }).first()).toBeVisible();
  await expect(page.getByRole('button', { name: 'Save', exact: true }).first()).toBeVisible();
});

test('10: Settings has the section Matter patterns with the button Add pattern', async ({ page }) => {
  await visit(page, 'settings');
  await expect(page.getByText('Matter patterns', { exact: true }).first()).toBeVisible();
  await expect(page.getByRole('button', { name: 'Add pattern', exact: true }).first()).toBeVisible();
});
// ------------------------------------------------------------------ added: 21, 22, 25, 26 on the pages

test('21: Approve on a proposed rule makes it a rule in the list, switched on, and it keeps what it dropped', async ({ page }) => {
  const mk = mkr();
  const name = `Rule ${mk}`;
  const r = await api(page, 'POST', '/api/rule-proposals', {
    field: 'sender', value: `bulk-${mk}@example.invalid`, name, reason: `214 mails in three years, none opened, none answered ${mk}`,
  });
  expect(r.status, r.text.slice(0, 300)).toBe(200);
  await visit(page, 'settings');
  const card = page.getByText(mk).first().locator('xpath=ancestor::*[.//button[normalize-space()="Dismiss"]][1]');
  await btn(card, 'Approve').click();
  const rule = page.getByText(name, { exact: true }).first().locator('xpath=ancestor::li[1]');
  await expect(rule.getByRole('switch', { name: /^Enabled/ })).toBeVisible();
  await expect(rule.getByText(/Dropped \(0\)/).first()).toBeAttached();
  await expect(rule.getByRole('button', { name: 'Test rule', exact: true })).toBeVisible();
});

test('22: a refusal by a guard is counted on Settings under Refused recently', async ({ page }) => {
  const c = await ready(page);
  const count = async () => {
    await visit(page, 'settings');
    const g = page.getByText('Reason required', { exact: true }).first().locator('xpath=ancestor::li[1]');
    return Number(/Refused recently: (\d+)/.exec(await g.innerText())![1]);
  };
  const before = await count();
  const no = await api(page, 'POST', `/api/items/${c.number}/skip`, { kind: 'noise' });
  expect(no.status).toBe(403);
  expectRefusal(no.json.error, 'Reason required');
  expect(await count()).toBe(before + 1);
});

test('25: The owner can switch Always show me on from the People page, and it protects the person', async ({ page }) => {
  const mk = mkr();
  const c = await ready(page, { mk, net: 'messenger', senderName: `Friend ${mk}`, senderId: `fr-${mk}` });
  await findPerson(page, `Friend ${mk}`);
  await page.getByText(`Friend ${mk}`).first().click();
  await page.getByLabel('Always show me').check();
  await page.getByRole('button', { name: 'Save', exact: true }).first().click();
  await expect.poll(async () => {
    const p = await api(page, 'GET', `/api/people?q=${mk}`);
    return (p.json.people as any[]).find((x) => x.name === `Friend ${mk}`)?.always_show;
  }).toBe(true);
  const r = await skip(page, c);
  expect(r.status, r.text).toBe(403);
  expectRefusal(r.json.error, 'Protected people');
});

test('26: the search page filters by person and says what each result is', async ({ page }) => {
  const mk = mkr();
  await ready(page, { mk, senderName: `Seeker ${mk}`, senderId: `sk-${mk}`, text: `findable words ${mk}` });
  await go(page, `/search?q=${mk}`);
  await expect(page.getByText('Chat', { exact: true }).locator('visible=true').first()).toBeVisible();
  await page.getByLabel(/^Person/).fill(`Nobody ${mkr()}`);
  await page.getByRole('button', { name: 'Search', exact: true }).click();
  await expect(page.getByText('No results.').first()).toBeVisible();
  await page.getByLabel(/^Person/).fill(`Seeker ${mk}`);
  await page.getByRole('button', { name: 'Search', exact: true }).click();
  await expect(page.getByText(`findable words ${mk}`).first()).toBeVisible();
});

void text; void chat; void newChat; void toggleOf; void isOpen; void putFile; void days; void groupHas;

// ------------------------------------------------------------------ 24 and 32

test.fixme('24: a failed source turns red with its error, one notice is sent, and a strip under the header names it', async () => {
  // Needs a failing source or a bridge that reports a logged-out network: that would change a real connection's state.
  // The app's own tests cover it; a browser test could only look at the strip.
});

// 32: the Work tool token and its private list are tested in worktool.spec.ts.
