// Acceptance tests for apps/hello/SPEC.md, one test per numbered criterion.
// Written from the spec alone. Safe on a preview holding a copy of real data:
// every test makes its own uniquely marked notes and deletes only those.
import { test, expect, type Page, type TestInfo } from '@playwright/test';

const created: string[] = [];

function marker(label: string): string {
  return `${label}-${Date.now().toString(36)}-${Math.random().toString(36).slice(2, 8)}`;
}

const field = (page: Page) => page.getByRole('textbox', { name: 'Note', exact: true });
const addButton = (page: Page) => page.getByRole('button', { name: 'Add', exact: true });
const notes = (page: Page) => page.getByRole('list', { name: 'Notes', exact: true });
const noteItem = (page: Page, text: string) => notes(page).getByRole('listitem').filter({ hasText: text });

async function addNote(page: Page, text: string, how: 'click' | 'enter' = 'click') {
  created.push(text);
  await field(page).fill(text);
  if (how === 'click') {
    await addButton(page).click();
  } else {
    await field(page).press('Enter');
  }
  await expect(noteItem(page, text)).toHaveCount(1);
  // The page after the redirect must be fully loaded (stylesheet included)
  // before anything measures layout; over a real network it arrives later.
  await page.waitForLoadState('load');
}

async function shot(page: Page, info: TestInfo, name: string) {
  const file = info.outputPath(`${name}.png`);
  await page.screenshot({ path: file, fullPage: true });
  await info.attach(name, { path: file, contentType: 'image/png' });
}

// Delete every note this test created, even if the test failed.
test.afterEach(async ({ page }) => {
  for (const text of created.splice(0)) {
    // Reload before every look so a stale page is never counted twice.
    for (let attempt = 0; attempt < 5; attempt++) {
      await page.goto('/');
      const item = noteItem(page, text);
      if ((await item.count()) === 0) break;
      await item.first().getByRole('button', { name: 'Delete', exact: true }).click();
      await page.waitForLoadState('load');
    }
  }
});

test('1: front page shows the title, heading and environment', async ({ page }, info) => {
  await page.goto('/');
  await expect(page).toHaveTitle('Hello');
  await expect(page.getByRole('heading', { level: 1, name: 'Hello', exact: true })).toBeVisible();
  await expect(page.getByText(/^Environment: (production|preview: \S+|development)$/)).toBeVisible();
  await shot(page, info, 'front-page');
});

test('2: adding a note puts it at the top and empties the field', async ({ page }, info) => {
  await page.goto('/');
  const first = marker('click');
  await addNote(page, first, 'click');
  await expect(notes(page).getByRole('listitem').first()).toContainText(first);
  await expect(field(page)).toHaveValue('');

  const second = marker('enter');
  await addNote(page, second, 'enter');
  await expect(notes(page).getByRole('listitem').first()).toContainText(second);
  await expect(field(page)).toHaveValue('');
  await shot(page, info, 'note-added');
});

test('3: a note is kept after reload', async ({ page }, info) => {
  await page.goto('/');
  const text = marker('kept');
  await addNote(page, text);
  await page.reload();
  await expect(noteItem(page, text)).toHaveCount(1);
  await shot(page, info, 'after-reload');
});

test('4: the newest note is at the top', async ({ page }, info) => {
  await page.goto('/');
  const older = marker('older');
  const newer = marker('newer');
  await addNote(page, older);
  await addNote(page, newer);
  const texts = await notes(page).getByRole('listitem').allInnerTexts();
  const iNewer = texts.findIndex((t) => t.includes(newer));
  const iOlder = texts.findIndex((t) => t.includes(older));
  expect(iNewer).toBeGreaterThanOrEqual(0);
  expect(iNewer).toBeLessThan(iOlder);
  await shot(page, info, 'newest-first');
});

test('5: empty notes are refused with a message', async ({ page }, info) => {
  await page.goto('/');
  const before = await notes(page).getByRole('listitem').count();

  await field(page).fill('');
  await addButton(page).click();
  await expect(page.getByText('Write something first.')).toBeVisible();
  await expect(notes(page).getByRole('listitem')).toHaveCount(before);

  await field(page).fill('     ');
  await addButton(page).click();
  await expect(page.getByText('Write something first.')).toBeVisible();
  await expect(notes(page).getByRole('listitem')).toHaveCount(before);
  await shot(page, info, 'empty-refused');
});

test('6: a note holds at most 280 characters', async ({ page }, info) => {
  await page.goto('/');
  await field(page).click();
  await field(page).pressSequentially('x'.repeat(290));
  const value = await field(page).inputValue();
  expect(value.length).toBeLessThanOrEqual(280);
  await shot(page, info, 'at-most-280');
});

test('7: delete removes a note for good', async ({ page }, info) => {
  await page.goto('/');
  const text = marker('delete');
  await addNote(page, text);
  await noteItem(page, text).getByRole('button', { name: 'Delete', exact: true }).click();
  await expect(noteItem(page, text)).toHaveCount(0);
  await page.reload();
  await expect(noteItem(page, text)).toHaveCount(0);
  await shot(page, info, 'deleted');
});

test('8: fits the screen without sideways scrolling, even with a long note', async ({ page }, info) => {
  const noSideways = async () => {
    await page.waitForLoadState('load');
    const { scroll, width } = await page.evaluate(() => ({
      scroll: document.documentElement.scrollWidth,
      width: window.innerWidth,
    }));
    expect(scroll).toBeLessThanOrEqual(width + 1);
  };
  await page.goto('/');
  await noSideways();
  const long = marker('w') + 'w'.repeat(280);
  const text = long.slice(0, 280);
  await addNote(page, text);
  await noSideways();
  await shot(page, info, 'long-note');
});

test('9: can be added to an iPhone home screen', async ({ page }) => {
  await page.goto('/');
  await expect(page.locator('head meta[name="viewport"]')).toHaveCount(1);

  const manifestHref = await page.locator('head link[rel="manifest"]').getAttribute('href');
  expect(manifestHref).toBeTruthy();
  const manifestURL = new URL(manifestHref!, page.url()).toString();
  const manifestResp = await page.request.get(manifestURL);
  expect(manifestResp.status()).toBe(200);
  const manifest = await manifestResp.json();
  expect([manifest.name, manifest.short_name]).toContain('Hello');
  expect(Array.isArray(manifest.icons) && manifest.icons.length > 0).toBeTruthy();
  const iconResp = await page.request.get(new URL(manifest.icons[0].src, manifestURL).toString());
  expect(iconResp.status()).toBe(200);
  expect(iconResp.headers()['content-type']).toMatch(/^image\//);

  const touchHref = await page.locator('head link[rel="apple-touch-icon"]').first().getAttribute('href');
  expect(touchHref).toBeTruthy();
  const touchResp = await page.request.get(new URL(touchHref!, page.url()).toString());
  expect(touchResp.status()).toBe(200);
  expect(touchResp.headers()['content-type']).toMatch(/^image\//);
});

test('10: the bottom of the page shows the version', async ({ page }, info) => {
  await page.goto('/');
  await expect(page.getByText(/^Version: (dev|[0-9a-f]{7})$/)).toBeVisible();
  await shot(page, info, 'version');
});

test('11: /healthz answers ok', async ({ page }) => {
  const resp = await page.request.get('/healthz');
  expect(resp.status()).toBe(200);
  expect((await resp.text()).trim()).toBe('ok');
});
