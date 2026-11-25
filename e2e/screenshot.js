// Take phone and desktop screenshots of pages of a running app.
// Usage: node screenshot.js <base-url> <out-dir> <path>...
const { chromium, devices } = require('@playwright/test');
const path = require('path');

async function main() {
  const [base, out, ...paths] = process.argv.slice(2);
  const phone = { ...devices['iPhone 15'] };
  delete phone.defaultBrowserType;
  // LOCALE (e.g. cs-CZ, en-US) and COLOR_SCHEME (light, dark) as
  // platform/UX-REVIEW.md describes.
  const common = {};
  if (process.env.LOCALE) common.locale = process.env.LOCALE;
  if (process.env.COLOR_SCHEME) common.colorScheme = process.env.COLOR_SCHEME;
  const sizes = {
    phone: { ...phone, ...common },
    desktop: { viewport: { width: 1280, height: 800 }, deviceScaleFactor: 1, ...common },
  };
  const browser = await chromium.launch();
  try {
    for (const [size, options] of Object.entries(sizes)) {
      const context = await browser.newContext(options);
      const page = await context.newPage();
      for (const p of paths) {
        await page.goto(new URL(p, base).toString(), { waitUntil: 'load' });
        const name = p.replace(/[^A-Za-z0-9]+/g, '-').replace(/^-+|-+$/g, '') || 'home';
        const file = path.join(out, `${name}-${size}.png`);
        await page.screenshot({ path: file, fullPage: true });
        console.log(file);
      }
      await context.close();
    }
  } finally {
    await browser.close();
  }
}

main().catch((err) => {
  console.error(err);
  process.exit(1);
});
