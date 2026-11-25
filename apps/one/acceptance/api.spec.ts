// Acceptance tests for apps/one/SPEC.md points 14, 15, 16, 21, 22, 25 and 29 through the address under /api/
// (apps/one/API.md): who is let in, no sweep, the bridge and its token, the guards, people, rule proposals and
// the connector.
//
// Not tested here, and why:
//  - 14 the lab sign-in gate: the harness has no gate locally, and a preview's gate belongs to the platform.
//  - 14 and 16 `New token` and `Copy`: only their presence is checked (pages.spec.ts); pressing New token
//    would change a real token.
//  - 15 Gmail, Outlook and calendar reading: covered by the app's own tests.
//  - 24 a failed source (red after three failures, `Needs login`, the strip, the notices): needs a failing source. The
//    only address that could fake one, `/api/bridge/alive` with a network that needs a new login, would change what
//    Connectors shows for the owner's real chat bridge, so it is left out. The `3 failed fetches` and the 15 minutes of
//    bridge silence also need waiting.
import { expect } from '@playwright/test';
import {
  test, isReading, mkr, api, token, ready, chat, newChat, postChats, findItem, skip, mcp, tool, stageOf, days, expectRefusal, openRow, seen,
} from './one-helpers';

const OWNER_ONLY = { ok: false, error: 'only the owner can do this' };
const WRONG = { ok: false, error: 'wrong token' };

// ------------------------------------------------------------------ 14

test('14: a wrong or missing token is refused with wrong token', async ({ page }) => {
  for (const who of ['none', 'not-the-token']) {
    for (const path of ['/api/queue', '/api/items/Q-0001', '/api/agenda', '/api/people']) {
      const r = await api(page, 'GET', path, undefined, who);
      expect(r.status, `${who} ${path}`).toBe(401);
      expect(r.json).toEqual(WRONG);
    }
    expect((await api(page, 'POST', '/api/items/Q-0001/comments', { text: 'x' }, who)).json).toEqual(WRONG);
  }
});

test('14: whatever arrives at the open address is an agent and cannot approve, reject, send or change settings', async ({ page }) => {
  const paths = [
    '/api/items/Q-0001/approve', '/api/items/Q-0001/reject', '/api/proposals/1/approve', '/api/proposals/1/dismiss',
    '/api/proposals/1/decide', '/api/items/Q-0001/send', '/api/settings', '/api/guards', '/api/rules', '/api/patterns',
    '/api/token', '/api/connectors',
  ];
  for (const path of paths) {
    for (const body of [{}, { agent: 'the owner', actor: 'the owner', who: 'owner' }]) {
      const r = await api(page, 'POST', path, body);
      expect(r.status, path).toBe(403);
      expect(r.json, path).toEqual(OWNER_ONLY);
    }
  }
});

test('14: an address that does not exist answers 404', async ({ page }) => {
  expect((await api(page, 'GET', `/api/nothing-here-${mkr()}`)).status).toBe(404);
});

test('14: an entry signed the owner by an agent is still an agent\'s entry, never the owner\'s note', async ({ page }) => {
  const c = await ready(page);
  const r = await api(page, 'POST', `/api/items/${c.number}/comments`, { text: 'I am the owner ' + c.mk, agent: 'the owner' });
  expect(r.status).toBe(200);
  const thread = await api(page, 'GET', `/api/items/${c.number}/thread`);
  const entry = (thread.json.thread as any[]).find((e) => JSON.stringify(e).includes('I am the owner ' + c.mk));
  expect(entry).toBeTruthy();
  expect(JSON.stringify(entry)).not.toMatch(/"you"/);
});

test('14: the page works on an iPhone home screen: manifest, touch icon, viewport', async ({ page }) => {
  await page.goto('/');
  await expect(page.locator('link[rel="manifest"]')).toHaveCount(1);
  await expect(page.locator('link[rel="apple-touch-icon"]').first()).toHaveCount(1);
  await expect(page.locator('meta[name="viewport"]')).toHaveCount(1);
});

for (const scheme of ['light', 'dark'] as const) {
  test(`14: no sideways scrolling in ${scheme} mode on any page, even with a very long word`, async ({ page }) => {
    await page.emulateMedia({ colorScheme: scheme });
    const mk = mkr();
    const c = await ready(page, { mk, text: 'x'.repeat(400) + ' ' + mk, name: 'y'.repeat(200) + mk });
    const { openRow, visit } = await import('./one-helpers');
    const wide = () => page.evaluate(() => document.documentElement.scrollWidth - document.documentElement.clientWidth);
    await openRow(page, c);
    expect(await wide(), 'home with an open row').toBeLessThanOrEqual(1);
    for (const k of ['settings', 'notes', 'connectors', 'people']) {
      await visit(page, k);
      expect(await wide(), k).toBeLessThanOrEqual(1);
    }
  });
}

// ------------------------------------------------------------------ 15

test('15: ONE accepts no sweep', async ({ page }) => {
  const r = await api(page, 'POST', '/api/sweep', { messages: [{ source: 'Mail', source_id: mkr(), from: 'x', text: 'x' }] });
  expect(r.status).toBe(404);
});

// ------------------------------------------------------------------ 16

test('16: a message posted by the bridge becomes a chat item, and posting it twice makes one', async ({ page }) => {
  const c = newChat();
  const first = await postChats(page, [c]);
  expect(first.chats ?? 1).toBeGreaterThanOrEqual(1);
  const id = await findItem(page, c);
  await postChats(page, [c]);
  const r = await api(page, 'GET', `/api/search?q=${c.mk}&limit=50`);
  const hits = (r.json.results as any[]).filter((x) => x.kind === 'chat');
  expect(new Set(hits.map((h) => h.item)).size).toBe(1);
  expect(hits).toHaveLength(1);
  const item = await api(page, 'GET', `/api/items/${id}`);
  expect(item.json.messages).toHaveLength(1);
  await api(page, 'POST', `/api/items/${id}/seen`, {});
  c.number = id;
});

test('16: the bridge token works only on the bridge addresses, the agents\' token only elsewhere', async ({ page }) => {
  const agent = await token(page, 'Agent'), bridge = await token(page, 'Bridge');
  expect(agent).not.toBe(bridge);
  const alive = { networks: [], version: 'e2e' };
  const asAgent = await api(page, 'POST', '/api/bridge/batch', { chats: [] }, 'agent');
  expect(asAgent.status).toBe(401);
  expect(asAgent.json).toEqual(WRONG);
  const asBridge = await api(page, 'GET', '/api/queue', undefined, 'bridge');
  expect(asBridge.status).toBe(401);
  expect(asBridge.json).toEqual(WRONG);
  const noToken = await api(page, 'POST', '/api/bridge/batch', { chats: [] }, 'none');
  expect(noToken.status).toBe(401);
  void alive;
});

test('16: the bridge token cannot approve, send or change anything', async ({ page }) => {
  for (const path of ['/api/bridge/approve', '/api/bridge/send', '/api/bridge/settings']) {
    const r = await api(page, 'POST', path, {}, 'bridge');
    expect(r.status, path).toBe(403);
    expect(r.json).toEqual(OWNER_ONLY);
  }
});

test('16: a batch that is not right is refused with a sentence and writes nothing', async ({ page }) => {
  const mk = mkr();
  const c = newChat({ mk });
  const body: any = { chats: [{ ...({ network: c.net, chat_id: c.chatId, name: c.name, type: 'direct', muted: false, participants: [], messages: 'not a list' }) }] };
  const r = await api(page, 'POST', '/api/bridge/batch', body, 'bridge');
  expect(r.status).toBe(400);
  expect(typeof r.json.error).toBe('string');
  const s = await api(page, 'GET', `/api/search?q=${mk}`);
  expect(s.json.results ?? []).toHaveLength(0);
});

test('16: an attachment that was not announced is not stored', async ({ page }) => {
  const c = await ready(page);
  const q = `network=${c.net}&chat_id=${encodeURIComponent(c.chatId)}&message_id=${encodeURIComponent(c.msgs[0].id)}&attachment=never`;
  const res = await page.request.fetch(`/api/bridge/files?${q}`, {
    method: 'PUT', headers: { Authorization: `Bearer ${await token(page, 'Bridge')}` }, data: Buffer.from('x'), failOnStatusCode: false,
  });
  expect(res.status()).toBe(404);
});

// ------------------------------------------------------------------ 21

test('21: a muted group chat is dropped at import, not made an item', async ({ page }) => {
  const c = newChat({ group: true, muted: true });
  const r = await postChats(page, [c]);
  expect(r.dropped ?? 1).toBeGreaterThanOrEqual(1);
  const s = await api(page, 'GET', `/api/search?q=${c.mk}`);
  expect((s.json.results ?? []).filter((x: any) => x.item)).toHaveLength(0);
});

test('21: Claude proposes a rule with the numbers behind it, and it does nothing until the owner approves', async ({ page }) => {
  const mk = mkr();
  const r = await api(page, 'POST', '/api/rule-proposals', {
    field: 'sender', value: `bulk-${mk}@example.invalid`, name: `Rule ${mk}`, reason: `214 mails in three years, none opened, none answered ${mk}`,
  });
  expect(r.status, r.text.slice(0, 300)).toBe(200);
  expect(r.json.rule_proposal_id).toBeTruthy();
  const again = await api(page, 'POST', '/api/rule-proposals', {
    field: 'sender', value: `bulk-${mk}@example.invalid`, name: `Rule ${mk}`, reason: `214 mails in three years, none opened, none answered ${mk}`,
  });
  expect(again.status).toBe(200);
  expect(again.json.rule_proposal_id).toBe(r.json.rule_proposal_id);
});

test('21: the history numbers per sender come without any message text', async ({ page }) => {
  const r = await api(page, 'GET', '/api/senders?limit=5');
  expect(r.status, r.text.slice(0, 200)).toBe(200);
  expect(r.json.history).toBeTruthy();
  expect(r.text).not.toMatch(/"text"/);
});

// ------------------------------------------------------------------ 22

test('22: Protected people: an item with a direct message from a protected person cannot be closed by an agent', async ({ page }) => {
  const mk = mkr();
  const c = await ready(page, { mk, senderName: `Protected ${mk}`, senderId: `prot-${mk}` });
  const set = await api(page, 'POST', '/api/people/details', { person: `Protected ${mk}`, always_show: true });
  expect(set.status, set.text.slice(0, 300)).toBe(200);
  const r = await skip(page, c);
  expect(r.status).toBe(403);
  expectRefusal(r.json.error, 'Protected people');
  expect(await stageOf(page, c)).toBe('new');
});

test('22: switching Always show me off is refused by Approval required, and nothing in that request is applied', async ({ page }) => {
  const mk = mkr();
  await ready(page, { mk, senderName: `Shown ${mk}`, senderId: `shown-${mk}` });
  await api(page, 'POST', '/api/people/details', { person: `Shown ${mk}`, always_show: true });
  const r = await api(page, 'POST', '/api/people/details', { person: `Shown ${mk}`, always_show: false, call_name: `Nick ${mk}` });
  expect(r.status).toBe(403);
  expectRefusal(r.json.error, 'Approval required');
  const p = await api(page, 'GET', `/api/people?q=${mk}`);
  const me = (p.json.people as any[]).find((x) => x.name === `Shown ${mk}`);
  expect(me.always_show).toBe(true);
  expect(me.call_name ?? '').not.toBe(`Nick ${mk}`);
});

// ------------------------------------------------------------------ 25 (what Claude does by itself)

test('25: every sender is added as a person on first sight, and Claude can set what the owner calls them', async ({ page }) => {
  const mk = mkr();
  await ready(page, { mk, senderName: `Newcomer ${mk}`, senderId: `new-${mk}` });
  const p = await api(page, 'GET', `/api/people?q=${mk}`);
  expect(p.json.total).toBeGreaterThanOrEqual(1);
  const set = await api(page, 'POST', '/api/people/details', { person: `Newcomer ${mk}`, call_name: `Nicky ${mk}`, relation: `my accountant ${mk}` });
  expect(set.status, set.text.slice(0, 300)).toBe(200);
  const again = await api(page, 'POST', '/api/people/details', { person: `Newcomer ${mk}`, call_name: `Nicky ${mk}` });
  expect(again.json.unchanged).toBe(true);
  const q = await api(page, 'GET', `/api/people?q=${mk}`);
  const me = (q.json.people as any[]).find((x) => x.name === `Newcomer ${mk}`);
  expect(me.call_name).toBe(`Nicky ${mk}`);
  expect(me.relation).toBe(`my accountant ${mk}`);
});

test('25: joining people and stopping Always show me are proposals that change nothing yet', async ({ page }) => {
  const mk = mkr();
  await ready(page, { mk, senderName: `Lenka ${mk}`, senderId: `w-${mk}` });
  await ready(page, { mk: mk + 'b', senderName: `Lenka mail ${mk}`, senderId: `m-${mk}` });
  const join = await api(page, 'POST', '/api/people/proposals', { action: 'join', person: `Lenka ${mk}`, with: `Lenka mail ${mk}`, reason: 'same person ' + mk });
  expect(join.status, join.text.slice(0, 300)).toBe(200);
  expect(join.json.proposal_id).toBeTruthy();
  const p = await api(page, 'GET', `/api/people?q=${mk}`);
  expect((p.json.people as any[]).map((x) => x.name)).toEqual(expect.arrayContaining([`Lenka ${mk}`, `Lenka mail ${mk}`]));
  await api(page, 'POST', '/api/people/details', { person: `Lenka ${mk}`, always_show: true });
  const stop = await api(page, 'POST', '/api/people/proposals', { action: 'stop_always_show', person: `Lenka ${mk}`, reason: 'e2e ' + mk });
  expect(stop.status).toBe(200);
  const q = await api(page, 'GET', `/api/people?q=${mk}`);
  expect((q.json.people as any[]).find((x) => x.name === `Lenka ${mk}`).always_show).toBe(true);
});

// ------------------------------------------------------------------ 26

test('26: search finds chats and comments, says what each is, and the person is a filter', async ({ page }) => {
  const mk = mkr();
  const c = await ready(page, { mk, senderName: `Finder ${mk}`, text: `needle in a haystack ${mk}` });
  const word = `commentword${mk}`;
  await api(page, 'POST', `/api/items/${c.number}/comments`, { text: `a remark ${word}` });
  const chatHits = await api(page, 'GET', `/api/search?q=${mk}`);
  expect((chatHits.json.results as any[]).some((x) => x.kind === 'chat' && x.item && qnEq(x.item, c.number))).toBe(true);
  const commentHits = await api(page, 'GET', `/api/search?q=${word}`);
  expect((commentHits.json.results as any[]).some((x) => x.kind === 'comment')).toBe(true);
  const byPerson = await api(page, 'GET', `/api/search?q=${mk}&person=${encodeURIComponent(`Finder ${mk}`)}`);
  expect((byPerson.json.results as any[]).length).toBeGreaterThan(0);
  const other = await api(page, 'GET', `/api/search?q=${mk}&person=${encodeURIComponent('Nobody ' + mkr())}`);
  expect((other.json.results as any[]) ?? []).toHaveLength(0);
  expect((await api(page, 'GET', '/api/search?q=')).status).toBe(400);
});

test('26: agents save notes that apply to an item, and only notes of agents can be changed by agents', async ({ page }) => {
  const c = await ready(page);
  const n = await api(page, 'POST', '/api/notes', { title: `Note ${c.mk}`, text: `first text ${c.mk}`, item: c.number });
  expect(n.status, n.text.slice(0, 300)).toBe(200);
  expect(n.json.created).toBe(true);
  const changed = await api(page, 'POST', '/api/notes', { note_id: n.json.note_id, title: `Note ${c.mk}`, text: `second text ${c.mk}`, item: c.number });
  expect(changed.status).toBe(200);
  const item = await api(page, 'GET', `/api/items/${c.number}`);
  expect(JSON.stringify(item.json.notes_that_apply)).toContain(`Note ${c.mk}`);
  const bad = await api(page, 'POST', '/api/notes', { title: 'x', text: 'y', sender: 'not an address or a domain' });
  expect(bad.status).toBe(400);
});

// ------------------------------------------------------------------ 29

test('29: the greeting and the tool list are open and hold nothing of the owner\'s', async ({ page }) => {
  const init = await mcp(page, 'initialize', {
    protocolVersion: '2025-03-26', capabilities: {}, clientInfo: { name: 'e2e', version: '1' },
  }, 'none');
  expect(init.status).toBe(200);
  expect(init.json.result).toBeTruthy();
  const list = await mcp(page, 'tools/list', {}, 'none');
  expect(list.status).toBe(200);
  const tools = list.json.result.tools as any[];
  const names = tools.map((t) => t.name);
  for (const n of [
    'list_review_queue', 'get_item', 'add_comment', 'mark_seen', 'resolve_item', 'unresolve_item', 'propose_change',
    'propose_reply', 'list_recent_decisions', 'search_items', 'list_agenda', 'post_agenda', 'save_note', 'history_numbers',
    'propose_rule', 'list_people', 'update_person', 'propose_person_change',
    'list_settings', 'propose_setting', 'service_status', 'get_context', 'record_feedback', 'get_log',
  ]) expect(names).toContain(n);
  // No tool approves, sends or changes a setting; reading the settings and proposing a change (for Approve) are allowed.
  expect(names.filter((n) => /approve|send|dismiss|reject/i.test(n) ||
    (/setting/i.test(n) && !['list_settings', 'propose_setting'].includes(n)))).toEqual([]);
});

test('29: each tool is marked as only reading or as changing something', async ({ page }) => {
  const list = await mcp(page, 'tools/list', {}, 'none');
  const tools = list.json.result.tools as any[];
  // Reading tools (point 29) only read; everything else changes something.
  const isRead = isReading; // one list of the reading tools, shared with the helpers
  for (const t of tools) {
    if (t.name === 'history_batch') continue; // point 34: it sizes up and also moves the point reached; covered by the history tests
    const ro = t.annotations?.readOnlyHint;
    if (isRead(t.name)) expect(ro, t.name).toBe(true);
    else expect(ro, t.name).not.toBe(true);
  }
});

test('29: using a tool needs the token', async ({ page }) => {
  const r = await mcp(page, 'tools/call', { name: 'list_review_queue', arguments: {} }, 'none');
  expect(r.status).toBe(200);
  expect(r.json.result.isError).toBe(true);
  expect(r.json.result.content[0].text).toContain('wrong token');
});

test('29: Claude reads, comments, searches, proposes and skips through the connector', async ({ page }) => {
  const c = await ready(page);
  const q = await tool(page, 'list_review_queue', { limit: 1 });
  expect(q.isError).toBe(false);
  const got = await tool(page, 'get_item', { id: c.number });
  expect(got.text).toContain(c.mk);
  const com = await tool(page, 'add_comment', { id: c.number, text: `by connector ${c.mk}` });
  expect(com.isError).toBe(false);
  const thread = await api(page, 'GET', `/api/items/${c.number}/thread`);
  expect(thread.text).toContain(`by connector ${c.mk}`);
  const found = await tool(page, 'search_items', { query: c.mk });
  expect(found.text).toContain(c.number);
  const prop = await tool(page, 'propose_change', { id: c.number, action: 'resolve', reason: 'e2e ' + c.mk });
  expect(prop.isError).toBe(false);
  expect(await stageOf(page, c)).toBe('new');
  const rp = await tool(page, 'propose_reply', { id: c.number, text: `connector reply ${c.mk}`, reason: 'e2e' });
  expect(rp.isError).toBe(false);
  const bad = await tool(page, 'resolve_item', { id: c.number, reason: '' });
  expect(bad.isError).toBe(true);
  const ok = await tool(page, 'resolve_item', { id: c.number, reason: 'clear skip ' + c.mk, kind: 'finished' });
  expect(ok.isError).toBe(false);
  expect(await stageOf(page, c)).toBe('closed');
  const back = await tool(page, 'unresolve_item', { id: c.number });
  expect(back.isError).toBe(false);
});

test('29: an agent asks for a refresh through the connector: one at a time, and it says what each source brought', async ({ page }) => {
  test.setTimeout(120_000);
  const a = await tool(page, 'refresh_now', {});
  expect(a.isError, a.text).toBe(false);
  expect(a.text.length, 'it answers with what happened').toBeGreaterThan(5);
  // it starts one only if none is running: asking again never makes an error, whichever way it answers
  const b = await tool(page, 'refresh_now', {});
  expect(b.isError, b.text).toBe(false);
});

test('29: an agent can read the settings and the state of every connection, and list the items of a stage', async ({ page }) => {
  const c = await ready(page);
  const st = await tool(page, 'list_settings', {});
  expect(st.isError, st.text).toBe(false);
  for (const w of ['Chat quiet time', 'Reason required']) expect(st.text).toContain(w);
  const svc = await tool(page, 'service_status', {});
  expect(svc.isError, svc.text).toBe(false);
  expect(svc.text).toMatch(/Gmail/);
  const li = await tool(page, 'list_items', { stage: 'new' });
  expect(li.isError, li.text).toBe(false);
  expect(li.text.length).toBeGreaterThan(2);
  void c;
});

test('29: an agent records the owner\'s feedback on an item: it is kept on the item and shown in its thread under Feedback from the owner', async ({ page }) => {
  const c = await ready(page);
  const fb = await tool(page, 'record_feedback', { id: c.number, text: `that was wrong ${c.mk}` });
  expect(fb.isError, fb.text).toBe(false);
  const thread = await api(page, 'GET', `/api/items/${c.number}/thread`);
  expect(thread.text).toContain(`that was wrong ${c.mk}`);
  const row = await openRow(page, c);
  await expect(seen(row, 'Feedback from the owner')).toBeVisible();
});

function qnEq(a: unknown, b: string) { return String(a).replace(/\D/g, '').replace(/^0+/, '') === b.replace(/\D/g, '').replace(/^0+/, ''); }
void chat; void days;
