// Shared code for the new acceptance tests (new-*.spec.ts). Imports from one-helpers.ts, changes nothing there.
import { expect, type Locator, type Page } from '@playwright/test';
import {
  api, mcp, currentRun, go, isPhone, isLaptop, showRow, rowBox, btn, press, toggleOf, expandGroup, moreUntil, GROUPS,
  type Chat,
} from './one-helpers';

/** Tools that only read: they are called without a run name. Every other tool names this test's run. */
const READS = [
  'list_review_queue', 'get_item', 'list_recent_decisions', 'search_items', 'list_agenda', 'history_numbers', 'list_people',
  'service_status', 'get_log',
];
/** Reading tools that take a run name to show what that run made (without it they show only what is not a test's). */
const RUN_READS = ['list_items', 'get_context', 'get_file', 'list_settings'];
/** Call a connector tool (the connector's tools/list shows each tool's name and input). */
export async function ag(page: Page, name: string, args: Record<string, unknown> = {}) {
  const run = currentRun();
  const a = run && (RUN_READS.includes(name) || !READS.includes(name)) && !('run' in args) && name !== 'history_batch' ? { ...args, run } : args;
  const r = await mcp(page, 'tools/call', { name, arguments: a });
  expect(r.status, r.text.slice(0, 200)).toBe(200);
  const res = r.json?.result;
  const text = String(res?.content?.[0]?.text ?? r.json?.error?.message ?? '');
  let data: any = null;
  try { data = JSON.parse(text); } catch { /* plain text */ }
  return { isError: !!res?.isError || !!r.json?.error, text, data, res };
}

/** An agent's comment on the item: on a phone this is what makes a chat row listed (SPEC 17). */
export async function grow(page: Page, c: Chat, text = `grown ${c.mk}`) {
  const r = await api(page, 'POST', `/api/items/${c.number}/comments`, { text });
  expect(r.status, r.text.slice(0, 200)).toBe(200);
}

/** A proposal (pending, resolve, close) made by an agent through the open address. */
export async function propose(page: Page, c: Chat, action: string, reason: string) {
  const r = await api(page, 'POST', `/api/items/${c.number}/proposals`, { action, reason });
  expect(r.status, r.text.slice(0, 300)).toBe(200);
  return r.json;
}

export const stage = async (page: Page, c: Chat): Promise<string> => {
  const r = await api(page, 'GET', `/api/items/${c.number}`);
  return String(r.json?.item?.stage ?? '');
};

/** The thread of an item as text (every entry). */
export async function threadText(page: Page, c: Chat): Promise<string> {
  const r = await api(page, 'GET', `/api/items/${c.number}/thread`);
  return JSON.stringify(r.json);
}

/** Look for the row in the groups (phone) or stages (laptop) without reloading, and return its top line. */
export async function findRowHere(page: Page, c: Chat): Promise<Locator | null> {
  const t = toggleOf(page, c);
  for (const g of GROUPS) {
    if (await t.isVisible().catch(() => false)) return t;
    await expandGroup(page, g);
    if (await moreUntil(page, t, 1500)) return t;
  }
  return (await t.isVisible().catch(() => false)) ? t : null;
}

/** The card (nearest ancestor with an Approve button) of a text, e.g. a proposal in the agenda or in Settings. */
export const cardOf = (scope: Locator | Page, text: string | RegExp): Locator =>
  scope.getByText(text).first().locator('xpath=ancestor::*[.//button[normalize-space()="Approve"]][1]');

export { api, go, isPhone, isLaptop, showRow, rowBox, btn, press };
