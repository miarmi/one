# Lab — rules for agents

The owner owns this repo and does not read code. The owner describes what is wanted in plain
words; agents build it; tests decide what ships. Everything here exists to make
that safe.

## Layout

```
lab.json                 domain, server IP, repo (the only global config)
apps/<name>/             one app per folder (package main)
  app.json               manifest (see "App contract")
  SPEC.md                numbered acceptance criteria in plain language
  acceptance/*.spec.ts   browser tests implementing SPEC.md, one test per criterion
  acceptance.lock        hashes of SPEC.md + acceptance/ (see "Tests are the contract")
platform/labd/           server program (Go, standard library only)
platform/seed/           first-boot program that installs labd (Python, standard library only)
platform/cloudinit/      generator for the Hetzner setup text
platform/runner/         the test server that runs the pipeline's jobs (setup text, job manager)
platform/scripts/        build, deploy and CI helper scripts
platform/appkit/         shared helpers apps import (`lab/platform/appkit`): notifications, logging
platform/CONTRACT.md     how the pipeline, labd and apps talk to each other
platform/UX-REVIEW.md    brief for the UX reviewer (see "How work flows")
e2e/                     Playwright harness, local app runner, screenshot helper
.github/workflows/       pipeline (test → preview/prod), cleanup, server tools, security (every 6 h)
```

## How work flows (spec first, then two speeds)

- **Idea = spec first.** Write the idea up with the owner in the chat as a short
  numbered list of what it must do, in his words. Build nothing until he OKs
  it. The agreed `apps/<name>/SPEC.md` is the first commit on the idea's
  branch. If the idea changes later, change the spec first (with his OK), then
  the code. The pipeline enforces the minimum: it builds no app without a
  SPEC.md.
- **Build = prototype on the branch.** Name it like `dark-mode` (lowercase
  letters, digits, single hyphens, max 30 chars; other names get no preview).
  Prototypes need no tests yet.
  Every push builds the changed apps and deploys them straight away to
  `http://<branch>--<app>.<base>` (about a minute; a new preview starts with a
  copy of production data). Only the quick checks (vet, unit tests) run
  alongside; no browser tests run on a push. While iterating, show the owner
  screenshots from your own workspace first: `e2e/screenshot.sh <app> [path ...]`
  writes phone and desktop pictures to `dist/screenshots/<app>/` (set
  `PREPARE=<script>` to fill the app with demo data first).
- **While building: check it yourself, then show the owner.** Before each push the
  builder runs `gofmt`, `go vet` and `go test` in its own workspace, starts the
  app there with demo data and looks at every changed screen at the owner's two
  real sizes (a window about 1130 px wide, and an iPhone), in light and dark.
  Then it pushes once and the owner gets the link straight away; what he sees
  wrong, he says. No separate review rounds while a prototype is being built.
  Decision: the checks belong at shipping, and
  several review rounds per change were slowing every iteration down.
- **UX review = once, before shipping.** Before a branch is shipped, every new
  or changed screen goes once to a separate agent acting as UX designer and
  usability tester, briefed with `platform/UX-REVIEW.md`. Fix what it finds
  (findings that change what the app does are spec changes and need the owner's
  OK) and let the same reviewer check the fixes.
- **Big and small releases.** Decision: the long checks
  run only for big releases, "version 1, not 1.01". An app's release is
  *big* when its SPEC.md differs from the work toolduction runs, or it is not
  in production yet (or the pipeline cannot tell); *small* when SPEC.md is
  unchanged. Small releases go out without acceptance tests: build, vet, unit
  tests, deploy with health check, smoke check and automatic rollback. The run
  summary says which kind each app is.
- **Keep = tests (big releases).** When the owner wants to keep a new app or a
  spec change, a separate agent that has not seen the code writes the
  acceptance tests from SPEC.md alone (see below).
- **Full run = on request only.** On a branch, browser tests run only when
  asked, and the agent asks, never the owner: `gh workflow run pipeline.yml --ref
  <branch> -f full=true` (in GitHub: Actions → `pipeline` → Run workflow →
  the branch → tick "full"). It runs the full matrix on a local copy and against the
  branch's preview and ends with "Ready to ship" or "Not ready to ship".
  Decision: it builds the apps a push of the branch
  builds (compared with main): the apps whose folder changed, or every app
  when shared files (`go.mod`, `go.sum`, `platform/appkit/`, `e2e/`,
  `build-app.sh`) changed; a branch that changes no app and no shared file
  says so and ends green. Its preview tests use two browsers, desktop Chrome and the
  iPhone (Safari engine), each with its own pass through the gate.
- **Shared files: quick checks for the other apps.** Decision: in a full run on a branch and in
  the run on main, an app whose own folder did not change (it is in the run
  only because a shared file changed) gets the quick checks only: build,
  `go vet`, unit tests, and on main the deploy with health check, smoke check
  and automatic rollback as for any small release; no browser matrix, local or
  on the preview. An app whose own folder changed is treated as before (big or
  small by its SPEC.md, short run, main accepting the branch verdict). One
  safety net stays: when `e2e/` or `platform/appkit/` changed and no app's own
  folder did, the smallest app, `hello`, runs its browser matrix as a canary
  (on main too, unless the branch run for the same commit already did); a
  change of `go.mod`, `go.sum` or `build-app.sh` alone needs no canary beyond
  building and unit-testing every app. The run summary says "quick checks only:
  shared files changed" for each such app. "Tested apps" lists only apps that
  had browser tests, so main never skips a matrix the branch did not run.
- **The preview comes first.** Decision: in every run
  that deploys a preview, the preview job runs before any test job: `checks`
  and `apps` start only once it has finished or been skipped, whatever its
  result. A pushed change is on its preview within about two minutes, even
  while a full run is queued behind it.
- **Do not collide with a full run.** Decision: after a
  push, let the preview deploy finish (about a minute) before asking for a full
  run, because a new run on the same branch cancels the one in progress; and
  never push to a branch while its full run is running, the push cancels it.
- **A short run after a green one.** Decision: when an
  earlier full run on the same branch ended "Ready to ship" for an earlier
  commit X, and since X an app's folder changed only in files that cannot
  change what it does (`*.css`, images, fonts, and `*.md` other than SPEC.md;
  nothing under `acceptance/`, no `.go`, `.html`, `.js`, `app.json` or lock),
  the full run is short for that app: local tests in desktop Chrome and the
  iPhone only, preview tests in desktop Chrome only, and the verdict is still
  "Ready to ship". The run summary says "short run: only style or documents
  changed since <X>" and lists the files. Any other change, anywhere, means
  the whole matrix (`platform/scripts/short-run.sh`).
- **The owner approves ahead; nothing waits for him.** Decision: he starts nothing in GitHub Actions to get a change into
  production. Shipping needs only his OK in the chat, and the agent asks for
  it ahead of time. When it expects to finish and test something while he is
  away (typically overnight), it asks in the evening, before the work is done,
  naming the app, what will change for him and whether the release is big or
  small. With that OK the agent does every remaining step itself, without
  another question: requests the full run, waits for "Ready to ship",
  fast-forwards main, watches the production deploy and smoke check, runs
  `dump`, and reports the outcome when he is back. The OK covers exactly what
  was named and only that round of work. Anything beyond it (another app, a
  spec change he has not OK'd, `retire`, `promote-data`) is asked separately,
  and the named part still ships. If the run does not end "Ready to ship",
  nothing ships and the report says why. Work finished without an OK waits on
  its branch with its verdict and ships on his one "yes" in the chat. Record
  each OK and what it covered in your notes with the date.
- **Ship = merge to main.** Fast-forward main to the branch and push, then
  dump the branch. Small release: when the branch's latest run is green and
  says "Ready to ship". Big release: only after a full run on the branch
  (requested as above) ended "Ready to ship"; a later push needs a new full
  run. On main a big release is refused if its tests don't match its
  SPEC.md; a failed smoke check rolls back automatically. Decision: main accepts the branch's verdict for the same commit: when
  a full run of the pipeline on a branch for exactly the commit main moves to
  ended "Ready to ship" and names the app as a big release it tested, main does
  not run the acceptance matrix for that app again. It still builds, runs vet
  and unit tests, checks that SPEC.md, tests and lock match, and deploys with
  the health check, smoke check and automatic rollback. The pipeline finds the
  verdict through the GitHub API with its own token
  (`platform/scripts/verdict.sh`); if anything is missing or unclear, the full
  matrix runs as before. The run summary says which path each app took and
  links the accepted run. Merge the branch as it is (fast-forward): a merge
  commit is a different commit and gets the full matrix.
- **Platform fixes merge themselves.** A pull request that changes files only
  under `platform/`, `.github/` or `e2e/` (plus top-level docs), nothing under
  `apps/` and no SPEC.md, may be merged by the agent that opened it once its
  pipeline run is green, telling the owner afterwards. Decision: routine hardening does not wait for the owner's click. Everything
  under `apps/` ships only with his OK in the chat (see "The owner approves
  ahead"); the agent then merges it. Every SPEC.md, `retire`, `promote-data`
  and repository secrets stay his.
- **Dump = run the `dump` workflow** with the branch name (workflow_dispatch on
  main). It removes the branch's previews, including apps that only exist on
  it, and deletes the branch. Agents here cannot delete branches directly. The
  agent that shipped a branch runs it (`gh workflow run dump.yml --ref main -f
  branch=<branch>`).
- **Retire = two steps, code first, data last.** To take an app out of
  production for good: (1) a pull request removes `apps/<name>` from main;
  (2) the owner runs the `retire` workflow with the app's name typed twice. It
  removes the production instance and any previews — unit, routes,
  certificate, releases and all data, secrets included. Nothing else can
  delete production, and agents never start `retire` themselves.
- **Promote data = once the app runs in production.** When an app's real
  data lives in a preview, the owner runs the `promote-data` workflow with
  the branch, the app and the app's name typed again. labd stops both,
  copies the preview's whole data, **secrets included**, into production,
  keeps production's old data on the server as a dated backup, and puts it
  back if production fails its health check. The run waits for the end
  and shows the true result. Only the owner starts it; agents never do.
- Branch runs stay green unless something broke: "Not ready to ship" is a
  notice, not a failure, so prototypes don't send the owner failure emails. Keep it
  that way.
- Never commit straight to main except trivial docs fixes.
- **Looking at the server.** What an app logged as an error in the last 24
  hours is on the lab's own page `https://<domain>/_lab/errors` (behind the
  login gate, linked from the lab's home page); no workflow needed. When a
  production app has clearly more errors than usual, labd sends one
  notification with a sample. For everything else (status, full logs,
  updates) there is the `server` workflow.
- Report to the owner in outcomes , never in code.

## Tests are the contract

- Every app has a SPEC.md before anything is built. Prototypes may have no
  tests yet; no big release reaches production unless SPEC.md, the acceptance
  tests and the lock match (`platform/scripts/app-state.sh` says `ready`). A
  stale lock does not hold back a small release (the owner).
- `SPEC.md` is the owner's. Change it only when he asked for the change; keep his
  words. Each criterion is numbered and has an **Exact wording** line for any
  UI text tests depend on.
- Acceptance tests are written from `SPEC.md` alone, by an agent that has not
  seen the implementation. Each test title starts with its criterion number
  (`"3: notes are kept after reload"`).
- The agent implementing a feature must not edit acceptance tests. If a test
  looks wrong, stop and raise it with the owner.
- After an approved spec change, the test-writing agent updates the tests and
  runs `platform/scripts/acceptance-lock.sh <app> --update`. Until then the
  app's tests count as pending: on a branch they don't run and the verdict says
  "Not ready to ship"; on main the pipeline refuses the app (a spec change is
  always a big release). Every push that
  changes a SPEC.md, the tests or the lock is flagged in the run summary.
- Acceptance tests must be safe on a preview that holds a copy of production
  data, and run against a preview only in a full run on request: create their own data with a unique marker, clean up only what they
  created, never assume an empty database.
- Browser matrix: Chrome and Safari engines; phone and desktop sizes. Firefox
  is left out for now. Against a preview
  only desktop Chrome and the iPhone run (by decision); the
  local copy still runs all four.
- A "Ready to ship" verdict names the apps it tested ("Tested apps", "Big
  releases tested"); main and a later short run read exactly those lines, so
  do not reword them without changing `platform/scripts/verdict.sh` and its
  test.
- Unit tests live next to the code (`*_test.go`). `go vet` and `go test ./...`
  must pass.

## App contract

- Name matches `^[a-z][a-z0-9]{1,19}$`. Reserved: `lab`, `www`, `api`, `mail`.
- `app.json`: `{"name": "hello", "title": "Hello", "health": "/healthz", "public": false}`.
  Optional `"memory": "256M"`. `public: true` skips the login gate (only for
  apps with nothing private in them). Optional `"open_paths": ["/kid/"]`: up
  to four paths that skip the gates while the rest of the app stays behind
  them, a prefix with a trailing slash (`/kid/`) or one exact path without
  (`/send`); the app must guard them itself (signed, expiring links, or its
  own token as Notify does) and serve nothing else there. Only with the owner's OK.
- Built by `platform/scripts/build-app.sh <name>` into one static Linux binary;
  templates and static files are embedded with `go:embed`.
- Environment at runtime: `PORT` (listen on `127.0.0.1:$PORT` only),
  `DATA_DIR` (the only writable place; SQLite file at `$DATA_DIR/<name>.db`),
  `APP_ENV` (`prod`, a preview name, or empty for local), `APP_NAME`,
  `APP_VERSION` (commit), `BASE_URL`.
- `GET /healthz` returns 200 `ok` only when the app can serve (database open).
- SIGTERM: finish in-flight requests and exit within 10 seconds.
- Log to stdout as JSON lines through `appkit.Logger()` and wrap the handler
  in `appkit.LogRequests` (`lab/platform/appkit`): every line carries `time`,
  `level`, `msg`, `app`, `env`, `version`, a request line also `method`,
  `path`, `status`, `ms`, `bytes`, `rid`. The server keeps them 30 days.
  Level `error` is what the lab's Errors page shows and what alerts the owner's
  phone, so log at `error` only what someone should look at. Paths under
  `/_lab/` belong to the platform (login, Errors page); apps must not use
  them.
- To tell the owner something, call `appkit.Notify(title, body, url)` from
  `lab/platform/appkit`: it drops a file into `$DATA_DIR/notify/outbox/` and
  labd pushes it to the owner's phone (through the optional Notify app, which is not part of this template; without it the message is only logged) as "<app>: <title>" (topic `bots`). No key, no
  network; previews' messages are dropped by labd; 30 messages an hour at most.
- Keep API keys and other secrets in `$DATA_DIR/secrets/`: a new preview gets a
  copy of production's data, but never that folder.
- Apps run in a tight sandbox (`platform/CONTRACT.md`, "Instances"): their own
  port only, no other programs, no Unix sockets, nothing on the server or the
  local network; the internet is reachable. Refuse cross-site form posts with
  Go's `http.CrossOriginProtection`.

## Stack rules

- Go, standard library first. Every new dependency needs a one-line reason in
  the commit message. `platform/labd` stays standard-library only.
- SQLite through `github.com/mattn/go-sqlite3`, WAL mode, `busy_timeout`
  5000, foreign keys on.
- Plain HTML, CSS and a little JavaScript. No frameworks, no build step.
- Pages must work on an iPhone home screen (manifest, apple-touch-icon,
  viewport meta, no sideways scrolling).
- No Docker, no Node at runtime. Node is only used to run Playwright in CI.

## Platform invariants (labd)

- Deploys are authenticated only by GitHub Actions OIDC tokens from this repo.
  Production and platform changes are accepted only from `refs/heads/main`.
  A branch can deploy only its own preview.
- There are no secrets in this repo, in CI or in the server setup text.
- Every deploy is health-checked; a failed health check restores the previous
  release.
- Previews and production apps are behind the login gate unless `public`,
  apart from an app's declared `open_paths` (see "App contract").

## Commits

Plain English: what changed and why, first line under 72 characters.
