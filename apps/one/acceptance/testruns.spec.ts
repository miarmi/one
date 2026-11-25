// Acceptance tests for apps/one/SPEC.md point 16 (how tests put items in, and how a test run removes everything of
// its own and nothing else) and point 30 (what the tests' marker may remove), with API.md, "Test runs".
//
// Every test here names its own run through the fixture in one-helpers.ts, like every other test, and the fixture
// removes it when the test ends. A second run name is used only to prove that one run cannot touch another's data.
//
// Not tested here, and why:
//  - 16 "never removes real mail, chats, people, notes, rules or settings": a test cannot make unmarked data (it would
//    stay behind), so the claim is checked with run names that match nothing real, with the other run's data, and,
//    where the app holds data that is not a test's (a preview does), with a call under a run name aimed at it.
//  - 16 calendar events and sync state: tests cannot create them (API.md).
import { expect } from '@playwright/test';
import {
  test, mkr, isPhone, go, api, token, ready, chat, newChat, postChats, findItem, itemOf, newRun, chatBody, putFile,
  openRow, say, visit, seen, noteBox, chatBox, btn,
} from './one-helpers';

const KINDS = [
  'items', 'chats', 'links', 'messages', 'raw', 'events', 'files', 'outbox', 'proposals', 'agenda', 'agenda_versions',
  'notes', 'people', 'handles', 'pictures', 'person_proposals', 'rules', 'skipped', 'history', 'search_hits', 'pool_files',
];

const sum = (o: Record<string, number>) => Object.values(o).reduce((a, b) => a + b, 0);

/** The text of what exists for a marker: search over everything, closed and history included. */
async function hits(page: import('@playwright/test').Page, word: string) {
  const r = await api(page, 'GET', `/api/search?q=${encodeURIComponent(word)}&limit=50`);
  expect(r.status, r.text.slice(0, 200)).toBe(200);
  return (r.json.results as any[]) ?? [];
}

test('16: a run removes everything of its own and says how many records of each kind', async ({ page, run }) => {
  const mk = mkr();
  const att = { id: 'r1', name: `run-${mk}.txt`, type: 'text/plain', size: 5 };
  const c = newChat({ mk, senderName: `Runner ${mk}`, senderId: `runner-${mk}`, msgs: [{ text: `with a file ${mk}`, attachments: [att] }, { text: `second ${mk}` }] });
  await postChats(page, [c]);
  expect((await putFile(page, c, c.msgs[0], att, Buffer.from('hello'))).status).toBe(200);
  c.number = await findItem(page, c);
  expect((await api(page, 'POST', `/api/items/${c.number}/seen`, {})).status).toBe(200);
  expect((await api(page, 'POST', `/api/items/${c.number}/comments`, { text: `comment ${mk}` })).status).toBe(200);
  expect((await api(page, 'POST', `/api/items/${c.number}/proposals`, { action: 'resolve', reason: `e2e ${mk}` })).status).toBe(200);
  expect((await api(page, 'POST', `/api/notes`, { title: `Note ${mk}`, text: `text ${mk}`, item: c.number })).status).toBe(200);
  expect((await api(page, 'POST', `/api/agenda/todos`, { item: c.number, text: `todo ${mk}`, reason: `e2e ${mk}` })).status).toBe(200);
  expect((await api(page, 'POST', `/api/people/details`, { person: `Runner ${mk}`, call_name: `Run ${mk}` })).status).toBe(200);
  const rule = await api(page, 'POST', '/api/rule-proposals', {
    field: 'sender', value: `bulk-${mk}@example.invalid`, name: `Rule ${mk}`, reason: `214 mails in three years, none opened, none answered ${mk}`,
  });
  expect(rule.status, rule.text.slice(0, 200)).toBe(200);

  const listed = await api(page, 'GET', '/api/test-runs');
  expect(listed.status).toBe(200);
  const mine = (listed.json.runs as any[]).find((r) => r.run === run);
  expect(mine, 'the run is listed').toBeTruthy();
  expect(mine.records).toBeGreaterThan(5);
  expect(mine.counts.items).toBe(1);
  expect(typeof mine.first_at).toBe('string');

  const gone = await api(page, 'DELETE', `/api/test-runs/${run}`, undefined, 'agent', null);
  expect(gone.status, gone.text.slice(0, 300)).toBe(200);
  expect(gone.json.ok).toBe(true);
  expect(gone.json.run).toBe(run);
  for (const k of KINDS) expect(typeof gone.json.removed[k], `kind ${k} is present`).toBe('number');
  expect(gone.json.removed.items).toBe(1);
  expect(gone.json.removed.messages).toBeGreaterThanOrEqual(2);
  expect(gone.json.removed.notes).toBeGreaterThanOrEqual(1);
  expect(gone.json.removed.people, 'the people seen only in marked chats go with it').toBeGreaterThanOrEqual(1);
  expect(gone.json.removed.files).toBeGreaterThanOrEqual(1);
  expect(gone.json.total).toBe(sum(gone.json.removed));

  expect((await api(page, 'GET', `/api/items/${c.number}`)).status).toBe(404);
  expect(await hits(page, mk), 'nothing of it is left in search').toEqual([]);
  const people = await api(page, 'GET', `/api/people?q=${mk}`);
  expect(people.json.people).toHaveLength(0);
  const after = await api(page, 'GET', '/api/test-runs');
  expect((after.json.runs as any[]).map((r) => r.run)).not.toContain(run);

  const again = await api(page, 'DELETE', `/api/test-runs/${run}`, undefined, 'agent', null);
  expect(again.status).toBe(200);
  expect(again.json.total, 'a run removed already answers zeros').toBe(0);
  for (const k of KINDS) expect(again.json.removed[k]).toBe(0);
});

test('16: a run that never existed answers zeros', async ({ page }) => {
  const r = await api(page, 'DELETE', `/api/test-runs/${newRun()}`, undefined, 'agent', null);
  expect(r.status).toBe(200);
  expect(r.json.total).toBe(0);
  for (const k of KINDS) expect(r.json.removed[k]).toBe(0);
});

test('16: removing a run needs a token, and either the agents\' or the bridge token will do', async ({ page, run }) => {
  const c = await ready(page);
  for (const who of ['none', 'not-the-token']) {
    const d = await api(page, 'DELETE', `/api/test-runs/${run}`, undefined, who, null);
    expect(d.status, who).toBe(401);
    expect(d.json).toEqual({ ok: false, error: 'wrong token' });
    const l = await api(page, 'GET', '/api/test-runs', undefined, who, null);
    expect(l.status, who).toBe(401);
  }
  expect((await itemOf(page, c)).item, 'nothing was removed by the refused calls').toBeTruthy();
  const listBridge = await api(page, 'GET', '/api/test-runs', undefined, 'bridge', null);
  expect(listBridge.status).toBe(200);
  const viaBridge = await api(page, 'DELETE', `/api/test-runs/${run}`, undefined, 'bridge', null);
  expect(viaBridge.status, viaBridge.text.slice(0, 200)).toBe(200);
  expect(viaBridge.json.removed.items).toBe(1);
  expect((await api(page, 'GET', `/api/items/${c.number}`)).status).toBe(404);
});

test('16: a run name has the shape e2e- and 8 to 40 lower-case letters and digits, otherwise nothing is written', async ({ page }) => {
  const mk = mkr();
  const bad = ['E2E-ABCDEFGH', 'e2e-short', 'x2e-abcdefgh1', 'e2e-' + 'a'.repeat(41), 'e2e-abc def gh', 'e2e-abcdefgh!', 'e2e_abcdefgh', ''];
  for (const name of bad) {
    const c = newChat({ mk: mk + 'x' });
    const headerOnly = await api(page, 'POST', '/api/bridge/batch', { ...chatBody([c]) }, 'bridge', name || 'bad name');
    if (name !== '') expect(headerOnly.status, `header ${JSON.stringify(name)}`).toBe(400);
    const fieldOnly = await api(page, 'POST', '/api/bridge/batch', { ...chatBody([c]), run: name || 'bad name' }, 'bridge', null);
    expect(fieldOnly.status, `field ${JSON.stringify(name)}`).toBe(400);
  }
  expect(await hits(page, mk + 'x'), 'nothing was written').toEqual([]);
  // an agent call too
  const c = await ready(page);
  const r = await api(page, 'POST', `/api/items/${c.number}/comments`, { text: `bad run ${mk}` }, 'agent', 'E2E-ABCDEFGH');
  expect(r.status).toBe(400);
  expect(await hits(page, `bad run ${mk}`)).toEqual([]);
});

test('16: the header and the field of a batch must agree', async ({ page, run }) => {
  const mk = mkr();
  const c = newChat({ mk });
  const other = newRun();
  const r = await api(page, 'POST', '/api/bridge/batch', { ...chatBody([c]), run: other }, 'bridge', run);
  expect(r.status, r.text.slice(0, 200)).toBe(400);
  expect(await hits(page, mk)).toEqual([]);
  const ok = await api(page, 'POST', '/api/bridge/batch', { ...chatBody([c]), run }, 'bridge', run);
  expect(ok.status, ok.text.slice(0, 200)).toBe(200);
  expect(await hits(page, mk)).not.toEqual([]);
});

test('16: a connector tool takes the run name as an argument, and the item is then removed with the run', async ({ page, run }) => {
  const c = await ready(page);
  const { tool } = await import('./one-helpers');
  const r = await tool(page, 'add_comment', { id: c.number, text: `by tool ${c.mk}`, run });
  expect(r.isError, r.text).toBe(false);
  const bad = await tool(page, 'add_comment', { id: c.number, text: `bad run ${c.mk}`, run: 'NOT A RUN' });
  expect(bad.isError).toBe(true);
  expect(await hits(page, `bad run ${c.mk}`)).toEqual([]);
  const gone = await api(page, 'DELETE', `/api/test-runs/${run}`, undefined, 'agent', null);
  expect(gone.json.removed.items).toBe(1);
});

test('16: a call under one run cannot touch the data of another run', async ({ page }) => {
  const mk = mkr();
  const c = await ready(page, { mk, senderName: `Owner ${mk}`, senderId: `owner-${mk}` });
  const note = await api(page, 'POST', '/api/notes', { title: `Note ${mk}`, text: `first ${mk}`, item: c.number });
  expect(note.status, note.text.slice(0, 200)).toBe(200);
  const rule = await api(page, 'POST', '/api/rule-proposals', {
    field: 'sender', value: `bulk-${mk}@example.invalid`, name: `Rule ${mk}`, reason: `214 mails in three years, none opened, none answered ${mk}`,
  });
  expect(rule.status).toBe(200);
  const before = JSON.stringify((await itemOf(page, c)).messages);
  const intruder = newRun();
  try {
    const t = async (path: string, body: unknown, label: string) => {
      const r = await api(page, 'POST', path, body, 'agent', intruder);
      expect(r.status, label + ' ' + r.text.slice(0, 160)).toBe(403);
      expect(typeof r.json.error, label).toBe('string');
      expect(r.json.ok, label).toBe(false);
    };
    await t(`/api/items/${c.number}/comments`, { text: `intruder ${mk}` }, 'comment');
    await t(`/api/items/${c.number}/proposals`, { action: 'resolve', reason: `intruder ${mk}` }, 'proposal');
    await t(`/api/items/${c.number}/reply-proposals`, { text: `intruder ${mk}`, reason: 'e2e' }, 'reply proposal');
    await t(`/api/notes`, { title: `intruder ${mk}`, text: `intruder ${mk}`, item: c.number }, 'note on the item');
    await t(`/api/notes`, { note_id: note.json.note_id, title: `Note ${mk}`, text: `changed ${mk}` }, 'change of the note');
    await t(`/api/agenda/todos`, { item: c.number, text: `intruder ${mk}`, reason: 'e2e' }, 'to-do');
    await t(`/api/people/details`, { person: `Owner ${mk}`, call_name: `Intruder ${mk}` }, 'person');
    await t(`/api/items/${c.number}/skip`, { reason: `intruder ${mk}`, kind: 'noise' }, 'skip');
    const batch = await api(page, 'POST', '/api/bridge/batch', { ...chatBody([{ ...c, msgs: [...c.msgs, { id: `${mk}-x`, text: `intruder ${mk}`, at: new Date().toISOString(), own: false, reactions: [], attachments: [] }] }]), run: intruder }, 'bridge', intruder);
    expect(batch.status, 'a batch naming a chat of another run: ' + batch.text.slice(0, 160)).toBe(409);
    expect(await hits(page, `intruder ${mk}`), 'nothing the intruder wrote exists').toEqual([]);
    expect(JSON.stringify((await itemOf(page, c)).messages), 'the item is as it was').toBe(before);
    expect((await itemOf(page, c)).item.stage).toBe('new');
    const people = await api(page, 'GET', `/api/people?q=${mk}`);
    expect((people.json.people as any[]).find((p) => p.name === `Owner ${mk}`).call_name ?? '').not.toBe(`Intruder ${mk}`);
    // and the intruder's own run cannot remove what is not its own
    const gone = await api(page, 'DELETE', `/api/test-runs/${intruder}`, undefined, 'agent', null);
    expect(gone.json.total).toBe(0);
    expect((await api(page, 'GET', `/api/items/${c.number}`)).status).toBe(200);
  } finally {
    await api(page, 'DELETE', `/api/test-runs/${intruder}`, undefined, 'agent', null);
  }
});

test('16: a call under a run name cannot touch data that carries no run mark', async ({ page }) => {
  // Anything the app holds that is not this test's own: on a preview that is the owner's real data; a bare local app has none.
  const mine = await ready(page);
  const found: string[] = [];
  for (const stage of ['new', 'in_progress', 'pending', 'resolved', 'closed', 'history']) {
    const r = await api(page, 'GET', `/api/items?stage=${stage}&limit=20`);
    if (r.status !== 200) continue;
    for (const it of (r.json.items ?? r.json.rows ?? []) as any[]) if (it.id && String(it.id) !== mine.number) found.push(String(it.id));
    if (found.length) break;
  }
  test.skip(found.length === 0, 'the app holds nothing but this test\'s own data: there is nothing unmarked to aim at');
  const target = found[0];
  const before = await api(page, 'GET', `/api/items/${target}/thread`);
  const word = `intruder-${mkr()}`;
  const r1 = await api(page, 'POST', `/api/items/${target}/comments`, { text: word });
  expect(r1.status, r1.text.slice(0, 200)).toBe(403);
  const r2 = await api(page, 'POST', `/api/items/${target}/proposals`, { action: 'resolve', reason: word });
  expect(r2.status, r2.text.slice(0, 200)).toBe(403);
  const r3 = await api(page, 'POST', `/api/notes`, { title: word, text: word, item: target });
  expect(r3.status, r3.text.slice(0, 200)).toBe(403);
  const after = await api(page, 'GET', `/api/items/${target}/thread`);
  expect(after.text, 'the unmarked item was not touched').toBe(before.text);
  expect(await hits(page, word)).toEqual([]);
});

test('16: removal never takes anything that is not the named run\'s, whatever it is asked', async ({ page }) => {
  const c = await ready(page);
  const note = await api(page, 'POST', '/api/notes', { title: `Note ${c.mk}`, text: `kept ${c.mk}`, item: c.number });
  expect(note.status).toBe(200);
  const odd = ['Q-0001', 'all', '*', '..', '%2e%2e', 'e2e-', 'e2e-' + 'z'.repeat(41), 'e2e-nosuchrun0001', 'settings', 'guards'];
  for (const name of odd) {
    const r = await api(page, 'DELETE', `/api/test-runs/${name}`, undefined, 'agent', null);
    if (r.status === 200) expect(r.json.total, name).toBe(0);
    else expect([400, 404, 405], name).toContain(r.status);
  }
  const noName = await api(page, 'DELETE', '/api/test-runs', undefined, 'agent', null);
  expect([400, 404, 405]).toContain(noName.status);
  expect((await api(page, 'GET', `/api/items/${c.number}`)).status, 'this run\'s item is still there').toBe(200);
  const s = await api(page, 'GET', '/api/search?q=' + encodeURIComponent(`kept ${c.mk}`));
  expect(JSON.stringify(s.json.results)).toContain(`Note ${c.mk}`);
  // settings and guards are still there
  await visit(page, 'settings');
  await expect(page.getByText('Guards', { exact: true }).first()).toBeVisible();
  await expect(page.getByText('Import rules', { exact: true }).first()).toBeVisible();
  for (const name of ['GitHub pipeline mails', 'Muted group chats']) await expect(page.getByText(name, { exact: true }).first()).toBeVisible();
});

test('16: a run removes what the owner later added to its item on the page: his notes, his chat messages and his approvals', async ({ page, run }) => {
  test.skip(isPhone(page), 'the box to write into a chat is for a laptop only (point 17)');
  const c = await ready(page, { net: 'messenger' });
  const row = await openRow(page, c);
  await say(row, `The owner's note ${c.mk}`);
  await expect(seen(row, `The owner's note ${c.mk}`)).toBeVisible();
  const box = chatBox(row, 'Messenger');
  await box.fill(`The owner's chat message ${c.mk}`);
  await box.press('Enter');
  await expect(box).toHaveValue('');
  await expect(row.getByText(`The owner's chat message ${c.mk}`).locator('visible=true').first()).toBeVisible();
  const gone = await api(page, 'DELETE', `/api/test-runs/${run}`, undefined, 'agent', null);
  expect(gone.status).toBe(200);
  expect(gone.json.removed.items).toBe(1);
  expect(gone.json.removed.outbox, 'what he wrote for the chat is removed too').toBeGreaterThanOrEqual(1);
  expect(await hits(page, c.mk)).toEqual([]);
});

test('16: Settings has a Test data section that shows the run and removes it with Remove', async ({ page, run }) => {
  const c = await ready(page);
  await visit(page, 'settings');
  const section = page.getByRole('region', { name: 'Test data' })
    .or(page.getByText('Test data', { exact: true }).first().locator('xpath=ancestor::section[1]')).first();
  await expect(section).toBeVisible();
  const entry = section.getByText(run).first().locator('xpath=ancestor::li[1]');
  await expect(entry).toBeVisible();
  page.once('dialog', (d) => d.accept()); // the page may ask once more
  await entry.getByRole('button', { name: 'Remove', exact: true }).click();
  await expect(page.getByText('Removed.').first()).toBeVisible();
  expect((await api(page, 'GET', `/api/items/${c.number}`)).status).toBe(404);
  const list = await api(page, 'GET', '/api/test-runs');
  expect((list.json.runs as any[]).map((r) => r.run)).not.toContain(run);
  await visit(page, 'settings');
  await expect(page.getByText(run)).toHaveCount(0);
});

void go; void chat; void token; void noteBox; void btn;
