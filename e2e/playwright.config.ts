import { defineConfig, devices } from '@playwright/test';
import * as fs from 'fs';
import * as os from 'os';
import * as path from 'path';

// Usage: APP=hello BASE_URL=http://127.0.0.1:8080 npx playwright test
// The browser projects (engine and size) are listed in projects.json;
// run-local.sh reads the same file.
const app = process.env.APP;
const baseURL = process.env.BASE_URL;
const projectDevices: Record<string, string> =
  JSON.parse(fs.readFileSync(path.join(__dirname, 'projects.json'), 'utf8'));
if (!app || !/^[a-z][a-z0-9]{1,19}$/.test(app)) {
  throw new Error('Set APP to the app folder name, e.g. APP=hello');
}
if (!baseURL || !/^https?:\/\//.test(baseURL)) {
  throw new Error('Set BASE_URL to where the app runs, e.g. BASE_URL=http://127.0.0.1:8080');
}

// Deployed apps can sit behind the platform login (milestone 2). The pipeline
// then passes a short-lived session in LAB_SESSION.
let storageState: string | undefined;
if (process.env.LAB_SESSION) {
  const url = new URL(baseURL);
  const file = path.join(fs.mkdtempSync(path.join(os.tmpdir(), 'lab-e2e-')), 'state.json');
  fs.writeFileSync(file, JSON.stringify({
    cookies: [{
      name: 'lab_session',
      value: process.env.LAB_SESSION,
      domain: url.hostname,
      path: '/',
      expires: -1,
      httpOnly: true,
      secure: url.protocol === 'https:',
      sameSite: 'Lax',
    }],
    origins: [],
  }), { mode: 0o600 });
  storageState = file;
}

// One app instance per browser project lets the projects run at the same
// time: run-local.sh starts the instances and passes their addresses in
// LAB_PROJECT_URLS ({"chromium-desktop": "http://127.0.0.1:41234", ...}).
// Tests of work toolject still run one after another, in file order, against
// their own instance, so shared app state (a connected practice WhatsApp,
// say) never crosses projects. Without the variable every project uses
// BASE_URL and they run one at a time, as against a live preview.
const projectURLs: Record<string, string> =
  process.env.LAB_PROJECT_URLS ? JSON.parse(process.env.LAB_PROJECT_URLS) : {};
for (const name of Object.keys(projectURLs)) {
  if (!(name in projectDevices)) {
    throw new Error(`LAB_PROJECT_URLS names an unknown project: ${name}`);
  }
}
// Playwright runs test files in parallel and only the tests inside one file
// in order, so an instance per project is only safe while the app has one
// spec file; with more, everything runs one at a time as before.
const testDir = path.join(__dirname, '..', 'apps', app, 'acceptance');
const specFiles = fs.existsSync(testDir) ? fs.readdirSync(testDir).filter((f) => f.endsWith('.spec.ts')) : [];
// run-local.sh may start instances for some projects only (a short run); then
// only those projects run.
const projectNames = Object.keys(projectURLs).length ? Object.keys(projectURLs) : Object.keys(projectDevices);
const ownInstances = specFiles.length === 1 &&
  projectNames.every((name) => name in projectURLs);

// How many browsers at once: e2e/workers.cjs (one per CPU, and 1 GB of memory
// each after 700 MB for the runner, Playwright and the app instances).
const { parallelBrowsers } = require('./workers.cjs');

export default defineConfig({
  testDir,
  // Tests live next to their app; this maps '@playwright/test' to e2e/node_modules.
  tsconfig: path.join(__dirname, 'tsconfig.json'),
  outputDir: path.join(__dirname, 'test-results'),
  workers: ownInstances ? parallelBrowsers(projectNames.length) : 1,
  fullyParallel: false,
  // In the pipeline a failed test gets one more try: three full runs on
  // 2026-11-22 each lost one or two tests to a timeout under Safari's engine,
  // a different test each time, so a verdict became a matter of luck. A test
  // that fails twice still fails; one that passes on its second try is
  // listed as flaky in the run's log. Locally there is no retry.
  retries: process.env.CI ? 1 : 0,
  forbidOnly: !!process.env.CI,
  timeout: 60_000,
  reporter: [
    ['list'],
    ['html', { open: 'never', outputFolder: path.join(__dirname, 'playwright-report') }],
    ...(process.env.CI ? [['github'] as ['github']] : []),
  ],
  use: {
    baseURL,
    storageState,
    ignoreHTTPSErrors: process.env.LAB_INSECURE_TLS === '1',
    screenshot: 'only-on-failure',
    trace: 'retain-on-failure',
  },
  projects: projectNames.map((name) => ({
    name,
    use: { ...devices[projectDevices[name]], ...(projectURLs[name] ? { baseURL: projectURLs[name] } : {}) },
  })),
});
