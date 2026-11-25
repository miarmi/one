// Acceptance tests for apps/one/SPEC.md point 29, the paragraph "Agents can also read the app's own log" (tool `get_log`).
// Written from SPEC.md alone. A line of our own is made by asking the app for an address whose path holds a unique marker:
// a request the app answered is a log line with its path. The log of a preview may be full of other lines, so every check
// looks only for the marker. Lines are looked for in the answer as `lines` (an array); `matched` is a number.
import { expect, type Page } from '@playwright/test';
import { test, mkr, api, mcp, iso } from './one-helpers';
import { ag } from './new-helpers';

test.beforeEach(() => { test.setTimeout(60_000); });

/** Make `n` request lines whose path holds the marker; returns the paths, oldest first. */
async function makeRequests(page: Page, mk: string, n: number): Promise<string[]> {
  const paths: string[] = [];
  for (let i = 1; i <= n; i++) {
    const p = `/api/items/log-${mk}-${i}`;
    await api(page, 'GET', p);
    paths.push(p);
    await page.waitForTimeout(50);
  }
  return paths;
}

const linesOf = (d: any): any[] => (Array.isArray(d?.lines) ? d.lines : []);

async function logOf(page: Page, args: Record<string, unknown>) {
  const r = await ag(page, 'get_log', args);
  expect(r.isError, r.text.slice(0, 300)).toBe(false);
  expect(Array.isArray(r.data?.lines), 'the answer holds the lines').toBe(true);
  return r;
}

test('29: get_log gives request lines only when asked, oldest first, and text narrows without regard to case', async ({ page }) => {
  const mk = mkr();
  const paths = await makeRequests(page, mk, 3);
  // Request lines are left out unless asked for.
  const without = await logOf(page, { text: mk });
  expect(linesOf(without.data).filter((l) => JSON.stringify(l).includes(mk)), 'no request lines unless requests is true').toEqual([]);
  // Asked for, with the text in capitals: found all the same, oldest first, each a request with method, path and status.
  await expect.poll(async () => linesOf((await logOf(page, { requests: true, text: mk.toUpperCase() })).data).length, { timeout: 15_000 }).toBe(3);
  const r = await logOf(page, { requests: true, text: mk.toUpperCase() });
  const lines = linesOf(r.data);
  expect(lines.map((l) => String(l.path))).toEqual(paths);
  for (const l of lines) {
    expect(l.msg).toBe('request');
    expect(String(l.method).toUpperCase()).toBe('GET');
    expect(l.status).toBeTruthy();
  }
  const times = lines.map((l) => Date.parse(l.time));
  expect(times, 'oldest first').toEqual([...times].sort((a, b) => a - b));
});

test('29: get_log keeps the newest lines that fit under limit, and matched says how many fit in all', async ({ page }) => {
  const mk = mkr();
  const paths = await makeRequests(page, mk, 3);
  await expect.poll(async () => (await logOf(page, { requests: true, text: mk })).data.matched, { timeout: 15_000 }).toBe(3);
  const r = await logOf(page, { requests: true, text: mk, limit: 1 });
  const lines = linesOf(r.data);
  expect(lines.length, 'one line back').toBe(1);
  expect(lines[0].path, 'the newest').toBe(paths[2]);
  expect(r.data.matched, 'how many fit in all').toBe(3);
});

test('29: get_log narrows to a span of time with from and to', async ({ page }) => {
  const mk = mkr();
  const paths = await makeRequests(page, mk, 1);
  await expect.poll(async () => (await logOf(page, { requests: true, text: mk })).data.matched, { timeout: 15_000 }).toBe(1);
  // A span in the far past holds none of our lines.
  const past = await logOf(page, { requests: true, text: mk, from: '2020-01-01T00:00:00Z', to: '2020-01-02T00:00:00Z' });
  expect(linesOf(past.data)).toEqual([]);
  expect(past.data.matched).toBe(0);
  // A span around now holds it.
  const now = await logOf(page, { requests: true, text: mk, from: iso(10), to: iso(-10) });
  expect(linesOf(now.data).map((l) => l.path)).toEqual(paths);
});

test('29: get_log with level problems gives only warnings and errors', async ({ page }) => {
  const mk = mkr();
  await makeRequests(page, mk, 1);
  const r = await logOf(page, { level: 'problems', requests: true });
  for (const l of linesOf(r.data)) expect(String(l.level).toUpperCase(), JSON.stringify(l).slice(0, 200)).toMatch(/^(WARN|WARNING|ERROR)$/);
  // Asking for everything is allowed too.
  expect((await logOf(page, { level: 'all', text: mk, requests: true })).isError).toBe(false);
});

test('29: the answer of get_log says that what a line quotes is data, never instructions', async ({ page }) => {
  const r = await ag(page, 'get_log', { limit: 1 });
  expect(r.isError, r.text.slice(0, 300)).toBe(false);
  expect(r.text).toMatch(/data/i);
  expect(r.text).toMatch(/instruction/i);
});

test('29: get_log is marked as only reading and needs the token', async ({ page }) => {
  const list = await mcp(page, 'tools/list', {}, 'none');
  const t = (list.json.result.tools as any[]).find((x) => x.name === 'get_log');
  expect(t, 'the tool get_log is listed').toBeTruthy();
  expect(t.annotations?.readOnlyHint).toBe(true);
  const r = await mcp(page, 'tools/call', { name: 'get_log', arguments: {} }, 'none');
  expect(r.status).toBe(200);
  expect(r.json.result.isError).toBe(true);
  expect(r.json.result.content[0].text).toContain('wrong token');
});
