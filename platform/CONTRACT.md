# Platform contract (labd ↔ pipeline ↔ apps)

This is the single source of truth for how the pipeline talks to the server
and how apps run there. labd, the scripts and the workflows must all follow it.

## Milestones

- **M1: bootstrap mode.** Plain HTTP on sslip.io names, the access gate
  (`allow_ips`, below) and `/api/session` for the pipeline.
- **M2: domain mode.** HTTPS with a Let's Encrypt certificate per host, the
  login gate with the owner's password, setup codes. Not built yet: nightly
  backups (see "Instances").

## Server facts

- Debian 13, set up by `platform/cloudinit/build.py` (see that file).
- nginx is the only thing on ports 80/443. labd listens on `127.0.0.1:7070`.
- `labd.service` runs `/usr/local/bin/labd -config /etc/lab/labd.json` as root
  with `Restart=always`. The first binary there is the seed
  (`platform/seed/seed.py`); the pipeline replaces it with the real labd.
- `/etc/lab/labd.json` (written once by the setup text):
  `{"repo": "OWNER/REPO", "listen": "127.0.0.1:7070"}`. labd also accepts
  optional test-only keys `oidc_issuer`, `oidc_jwks_url`, `state_dir`
  (default `/srv/lab`), `runner` (`systemd` default, or `exec` for local
  tests), `edge` (`nginx` default, or `none` for local tests) and `certs`
  (`certbot` default, or `selfsigned`). Unknown keys are an error.
- `labd -version` prints `labd <version>` and exits 0. `labd -selftest -config
  <file>` loads the config and state, checks it can run (directories writable,
  required tools present for the configured runner/edge) and exits 0 or 1.
- Tools available on the server: nginx, certbot, sqlite3, nftables, systemd
  (systemd-run, systemctl, journalctl), python3, unattended-upgrades. labd
  itself uses only the Go standard library.

## Modes

- **Bootstrap mode** (no domain yet): base domain is `<ip-with-dashes>.sslip.io`,
  everything is plain HTTP, no login gate. Apps are only for testing.
- **Domain mode**: base domain is the configured domain (`lab.json` `domain`,
  the lab's own name such as `lab.example.com`; DNS has an A record for it
  and for `*.<domain>`). HTTPS everywhere via certbot (webroot
  `/var/www/acme`), HTTP only serves ACME challenges and redirects, login gate
  on for every non-public app.
- Settings come from `lab.json` via `POST /api/settings`; labd stores them in
  `<state_dir>/settings.json`.

## Hostnames

- Production: `<app>.<base>`. Preview: `<env>--<app>.<base>`. Control (domain
  mode): `<base>` itself, where labd's API, sign-in pages and list of apps
  answer.
- App name: `^[a-z][a-z0-9]{1,19}$`, not `lab`, `www`, `api`, `mail`.
- Preview name from a branch (`previewName`): the branch name itself, when it
  matches `^[a-z0-9]([a-z0-9-]{0,28}[a-z0-9])?$`, has no `--` and is not `prod`
  or `main`. Any other branch is tested but gets no preview, so no two branches
  can ever share one (and deleting a look-alike branch can't remove another
  branch's preview). `main` never has a preview.

## API authentication

Every `/api/*` request carries `Authorization: Bearer <GitHub Actions OIDC token>`.

labd accepts a token only if all hold:

- RS256 signature valid against the issuer's JWKS
  (`https://token.actions.githubusercontent.com/.well-known/jwks`, cached 10
  minutes, refetched on an unknown `kid` at most once a minute and not again
  within 30 s of a failed fetch; while GitHub can't be reached, keys fetched in
  the last 24 hours keep working; a fetch never blocks other requests; keys <
  2048 bits rejected; any other `alg` rejected).
- `iss` = `https://token.actions.githubusercontent.com`; `exp`, `nbf`, `iat`
  valid with 60 s skew; `repository` = config `repo`; `repository_id` and
  `repository_owner_id` present and equal to the ones pinned by the first
  accepted token (so a repository re-created under the same name can't deploy);
  `jti` present and not seen before (remembered until `exp` + 60 s, also across
  restarts: used tokens and the pinned IDs are kept in `<state_dir>/tokens.json`).
- `aud` binds the token to this exact request:
  - `POST /api/platform`: `lab-platform:sha256:<hex sha256 of body>`
    (identical to the seed, so one upload script works for both).
  - every other endpoint: `lab-api:sha256:<hex>` where `<hex>` is the SHA-256
    of the UTF-8 string `METHOD + " " + REQUEST_URI + "\n" + hex(sha256(body))`.
    `REQUEST_URI` is the path plus query exactly as sent (e.g.
    `/api/deploy?app=hello&env=prod&commit=abc1234`); for an empty body use the
    SHA-256 of the empty string.
- Because the token is bound to method, URI and body, plain HTTP is safe for
  integrity. Endpoints that return secrets (`/api/session`) and logs
  (`/api/logs`) require HTTPS in domain mode; nginx tells labd the scheme with
  the header `X-Lab-Scheme` (labd trusts it only from 127.0.0.1).
  `/api/setup-code` returns no secret: it takes only a code's SHA-256.

Authorization by the token's `ref` (`refs/heads/<branch>`):

| Endpoint | Allowed when |
|---|---|
| `POST /api/platform`, `POST /api/settings`, `POST /api/gc`, `POST /api/setup-code` | ref is `refs/heads/main` |
| `POST /api/deploy`, `POST /api/rollback`, `POST /api/session` with `env=prod` | ref is main |
| same with a preview `env` | `previewName(branch) == env` and branch is not main |
| `DELETE /api/instances` with a preview `env` | ref is main, or `previewName(branch) == env` |
| `DELETE /api/instances` with `env=prod` (retire) | ref is main and the query repeats the app as `retire=<app>` |
| `POST /api/promote-data` | ref is main and the query repeats the app as `confirm=<app>` |
| `GET /api/promote-data` | ref is main |
| `GET /api/host`, `GET /api/host/audit`, `POST /api/host/upgrade`, `POST /api/host/reboot`, `POST /api/notify` | ref is main |
| `GET /api/status`, `GET /api/logs` | any ref of this repo |

Errors are JSON `{"ok": false, "error": "..."}`: 401 missing/invalid token,
403 not allowed, 400 bad input, 404 unknown instance, 409 conflict, 413 too
large (refused before reading when `Content-Length` says so), 422 deploy failed
health check, 429 too many uploads at once (at most 4 large bodies arrive at a
time; nothing was done, try again), 500 internal.

## Endpoints

- `GET /healthz` (no auth) → `{"ok": true, "stage": "labd", "version": "..."}`.
  (The seed answers `"stage": "seed"`.)
- `POST /api/platform` body: new labd binary (≤ 200 MB). labd writes
  `/usr/local/bin/labd.new`, runs `labd.new -version` and
  `labd.new -selftest -config <same config>` (both must exit 0 within 15 s),
  keeps the current binary as `labd.prev`, renames, replies
  `{"ok": true, "installed": "labd <version>"}` and exits 0 so systemd restarts it.
- `POST /api/settings` JSON `{"domain": "lab.example.com" | "", "public_ip": "1.2.3.4",
  "allow_ips": ["203.0.113.7", "198.51.100.0/24"]}` (`allow_ips` optional, at most
  50 IP addresses or CIDR ranges, never `/0`; `domain` a lowercase DNS name of
  at most 200 characters, not an IP address or an sslip.io name)
  → with a domain, first gets the certificate for the domain itself: if that
  fails (usually DNS not pointing here yet) it answers 409 and changes nothing.
  Then stores, restarts running apps when the base changed (so `BASE_URL`
  follows), rewrites nginx, gets the apps' certificates in the background
  (each host switches to HTTPS as soon as it has one), and replies
  `{"ok": true, "mode": "bootstrap" | "domain", "base": "..."}`.
- `POST /api/deploy?app=<name>&env=<prod|preview>&commit=<sha>` body: tar.gz
  bundle with exactly two regular files, `app` (executable) and `app.json`
  (whose `name` equals `app`). ≤ 200 MB. Success →
  `{"ok": true, "url": "...", "release": "...", "previous": "..."}`. Health
  failure → 422 with `rolled_back` (bool) and `logs` (last 100 lines).
  Deploys to the same instance are serialized. In domain mode a new host gets
  its certificate before the deploy answers; if that fails the app runs but
  the answer is 500 , and labd
  tries again every 10 minutes.
- `POST /api/rollback?app=&env=` → previous release becomes current.
- `DELETE /api/instances?app=&env=` → stop unit, remove routes, certificate,
  releases and data (secrets included) of that instance. With `env=prod` it
  retires the app for good and additionally needs `retire=<app>` in the query;
  the `retire` workflow is the only caller and runs it only after the app's
  folder is gone from main.
- `POST /api/promote-data?app=<app>&env=<preview>&confirm=<app>` → starts a
  job that replaces production's data with the preview's, secrets included
  (see "Promote data"), and answers at once: 202 `{"ok": true, "job":
  {...}}`. 409 (nothing started) when production or the preview doesn't
  exist or a job for the app is already running.
- `GET /api/promote-data?app=<app>` → `{"ok": true, "job": {"id", "app",
  "from", "state": "running" | "done" | "failed", "step", "backup",
  "error", "rolled_back", "logs", "started", "finished"}}`: the running or
  latest job; 404 if there never was one. The `promote-data` workflow is
  the only caller of both, and only the owner starts it.
- `POST /api/gc` JSON `{"previews": ["dark-mode", ...]}` → removes every preview
  whose env is not listed.
- `POST /api/session?app=&env=` (HTTPS in domain mode) → `{"ok": true, "cookie":
  "<value>", "expires": <unix>}` — a pass valid 15 minutes for that instance's
  host only, used by CI to test it behind the access gate or the login gate. 404
  if the instance doesn't exist.

## Access gate (allow_ips)

When `allow_ips` is non-empty, every app host only answers requests from those
addresses, or with a valid `lab_session` cookie: nginx uses `satisfy any`
with `allow`/`deny` and `auth_request /_lab/auth` (internal, proxied to
labd with `X-Lab-Host: $host`; labd answers 204 for a cookie valid for that
host and 401 otherwise, and only to 127.0.0.1). Everything else gets 403 `This
lab is private.`. `/healthz`, `/api/` and ACME challenges on the default server
stay reachable. Empty `allow_ips` means apps are open to everyone (bootstrap
mode). nginx removes `lab_session` from the `Cookie` header it passes to apps,
so no app ever sees a pass. In domain mode the login gate (below) takes over:
non-public apps always need home or a pass, and whoever is turned away gets
the sign-in page instead of the 403.
- `POST /api/setup-code[?reset=1]` (main) JSON `{"code_sha256": "<64 hex>"}`
  → stores the SHA-256 of a one-time setup code for the login gate:
  `{"ok": true, "expires": <unix>, "url": "https://<domain>/_lab/setup"}` (`url`
  in domain mode). The code is 16 characters of Crockford base32 (80 random
  bits, shown `XXXX-XXXX-XXXX-XXXX`; `platform/scripts/new-setup-code.sh` makes
  one), valid 60 minutes, works once. Whoever made it gives it to the owner; it
  never passes through GitHub. 409 once a password is set, unless `reset=1`.
- `GET /api/status` → `{"ok": true, "version": "...", "mode": "...", "base":
  "...", "login": {"password_set": bool}, "instances": [{"app", "env", "url",
  "release", "commit", "status", "sandbox": "hardened" | "basic",
  "deployed_at"}]}`; in domain mode also `"certificate": bool` for the domain
  itself and for each instance's host; on a systemd server also
  `"sandbox": {"version", "hardened": bool, "hardened_error"?, "bind_rules":
  bool, "bind_rules_error"?, "firewall": "applied" | "failed: ...",
  "settled": bool}` (see "Instances"; `settled` turns true once labd's
  startup work, hardening the server and restarting apps into a new sandbox,
  is done).
- `GET /api/host/audit` → a read-only security check (used by the `security`
  workflow every 6 hours): `listening` (every TCP/UDP listener with its
  process) and `unexpected_listening` (those that are not nginx on 80/443,
  labd, an app on its own port or the system's DNS client), `firewall`
  (`present`, `input_drop`, `ssh_accepted`, `lab_apps`, `blocked`,
  `ruleset_sha256`),
  `certificates` (`host`, `days_left`, `not_after`), `sign_in_failures_24h`
  (wrong passwords at the login gate, with time and IP; labd remembers the
  last 200 until it restarts), `journal_errors` (count and tail of priority
  `err` in the last 6 hours), `nginx` (requests by status class and the IPs
  with most requests and most refusals in the last 6 hours, from the access
  log), `units` (every `lab-app-*` unit and its state), `memory`, `disk`,
  `host` (the same as `GET /api/host`), and `findings`: a list of
  `{"severity": "critical" | "warning" | "info", "code", "text"}` sorted worst
  first, with `worst` as the top severity (`"ok"` when there is nothing).
  Critical: a listener that is not the lab's, the firewall not dropping by
  default or letting port 22 in or missing the apps' table, an expired
  certificate, 10+ wrong passwords in a day, a failed production unit, less
  than 1 GB of disk, SSH not off. Warning: security updates waiting, a restart
  waiting, a certificate under 14 days, 20+ journal errors, 50+ nginx 5xx, an
  IP with 300+ refused requests, a failed preview unit, under 10 % memory.
- `GET /api/host` → the server itself: `os`, `kernel`, `booted_at`, `disk`,
  `reboot_required` and `reboot_for` (packages), `updates` (`pending`,
  `security`, `packages` from `apt-get -s dist-upgrade`, `lists_updated_at`),
  `last_unattended_upgrade` (time and log tail), `upgrade_job` (the last one
  started through the API), `journal` (`retention`, "30day", and
  `disk_usage` from `journalctl --disk-usage`) and `hardening` (see "The
  server"; its `journal` says whether the retention is in force).
- `POST /api/host/upgrade` → installs pending updates now, the way the nightly
  unattended upgrade does (`apt-get update && unattended-upgrade`), in the unit
  `lab-host-upgrade.service`; answers at once, 409 while one runs.
- `POST /api/host/reboot[?if_required=1]` → restarts the server 2 s after
  answering (with `if_required=1` only when an update asked for it);
  `{"ok": true, "rebooting": bool}`. labd starts the apps again at boot.
- `POST /api/notify` with a JSON body `{"topic", "title", "body", "url"}` →
  pushes a message to the owner's phone through the Notify app's production
  instance (see "Notifications"); `{"ok": true, "delivered": bool}`, where
  `delivered` is false when Notify is not running or has no token yet (the
  message is dropped, not queued). 400 for an unknown topic (`security`,
  `lab` or `bots`), an empty title or one over 120 characters, a body over
  1000, a `url` that is not a web address.
- `GET /api/logs?app=&env=&lines=200` → `text/plain`, the last lines of the app's
  journal (max 20000).

## Instances (how apps run)

- One transient systemd unit per instance: `lab-app-<app>--<env>.service`,
  started with `systemd-run` in the **hardened sandbox**: `DynamicUser=yes`,
  `StateDirectory=lab/<app>--<env>` (mode 0700), `UMask=0077`,
  `NoNewPrivileges=yes`, `CapabilityBoundingSet=` (none), `ProtectSystem=strict`,
  `ProtectHome=yes`, `PrivateTmp=yes`, `PrivateDevices=yes`, `PrivateIPC=yes`,
  `ProtectKernelTunables=yes`, `ProtectKernelModules=yes`,
  `ProtectKernelLogs=yes`, `ProtectControlGroups=yes`, `ProtectClock=yes`,
  `ProtectHostname=yes`, `ProtectProc=invisible`, `ProcSubset=pid`,
  `RestrictSUIDSGID=yes`, `RestrictRealtime=yes`, `RestrictNamespaces=yes`,
  `LockPersonality=yes`, `MemoryDenyWriteExecute=yes`,
  `RestrictAddressFamilies=AF_INET AF_INET6`, `SystemCallArchitectures=native`,
  `SystemCallFilter=@system-service` minus `@aio @chown @clock @cpu-emulation
  @debug @keyring @memlock @module @mount @obsolete @privileged @raw-io @reboot
  @resources @setuid @swap userfaultfd` (io_uring, keyrings, bpf, ptrace and
  the like are where recent kernel attacks start), `SystemCallErrorNumber=EPERM`,
  `TemporaryFileSystem=<state_dir>:ro` with `BindReadOnlyPaths=` the instance's
  own directory (an app sees none of labd's state but its own releases),
  `NoExecPaths=/ -/tmp -/var/tmp -/dev/shm -<both data paths>` with
  `ExecPaths=` that directory (it can run nothing but its own binary), `MemoryMax=<app.json memory, default 256M>`, `TasksMax=256`,
  `CPUQuota=100%` (previews also `CPUWeight=20`, `IOWeight=20`, so production
  wins when both are busy; the basic sandbox has these too), `Restart=always`, `RestartSec=1`,
  `TimeoutStopSec=15`, and, where systemd supports it,
  `SocketBindAllow=tcp:<PORT>` and `SocketBindDeny=any`: an app can bind only
  its own port.
- labd checks once at start what this systemd can enforce: binding rules by
  running `labd -try-bind 127.0.0.1:29999` in a unit that may bind only port 9
  (systemd without its BPF framework accepts the rules but ignores them), the
  hardened sandbox by running its own binary (a static Go program like the
  apps) with `-sandbox-check` in it: listen on its port, write and read its
  data folder. If the hardened sandbox
  fails there, every app gets the **basic sandbox** (sandbox version 2:
  `DynamicUser=yes`, `StateDirectory`, `NoNewPrivileges`, `ProtectSystem=strict`,
  `ProtectHome`, `PrivateTmp`, `PrivateDevices`, `ProtectKernelTunables`,
  `ProtectKernelModules`, `ProtectControlGroups`, `RestrictSUIDSGID`,
  `LockPersonality`, `RestrictNamespaces`, `RestrictAddressFamilies=AF_INET
  AF_INET6 AF_UNIX`, `MemoryMax`, `TasksMax=256`, restart settings, binding
  rules) and `/api/status` says why.
- When the sandbox changes (`<state_dir>/sandbox` holds the version, now 3),
  labd restarts running apps into it once. A production app that doesn't
  become healthy in the hardened sandbox goes back to the basic one instead of
  staying down; the state records it (`"sandbox": "basic"`), restarts keep it,
  and its next deploy tries the hardened sandbox again (a new release that
  fails there fails its deploy, and the previous release keeps running). The
  pipeline's deploy fails while any production app runs in the basic sandbox
  or the hardened sandbox or app firewall is not in force. A preview that
  fails in the hardened sandbox is stopped (`"status": "failed"`, with a
  `note`) until it is deployed again.
- Apps can't open connections to the server itself or to private networks:
  labd keeps an nftables table `inet lab_apps` (applied at start, with every
  settings change and every reconcile) that, for the dynamic-user range
  61184–65519, accepts DNS to 127.0.0.0/8 and to the name servers in
  `/etc/resolv.conf`, then drops new connections to 127.0.0.0/8, ::1 and every
  address of the server (and the settings' `public_ip`), drops anything an app
  sends from 127.0.0.1 or ::1 on a port outside 20000–29999 (so an app that
  took labd's port while labd restarted could never answer in its place), and
  drops everything to private, link-local (with the cloud metadata service),
  carrier-grade NAT, multicast and other special ranges. Replies to nginx
  pass; the internet stays reachable.
- Files: `<state_dir>/instances/<app>--<env>/releases/<release>/{app,app.json}`
  (world-readable, `app` 0755) and a `current` symlink. Keep the last 5 releases.
- Environment: `PORT` (allocated from 20000–29999, stable per instance),
  `DATA_DIR=/var/lib/lab/<app>--<env>`, `APP_ENV` (`prod` or the preview name),
  `APP_NAME`, `APP_VERSION` (commit), `BASE_URL` (`https://host`, or `http://host`
  in bootstrap mode).
- Outbox: an app that wants to tell the owner something writes a JSON file
  `{"title", "body", "url", "topic"}` into `$DATA_DIR/notify/outbox/` under a
  name ending in `.json` (write it under another name first and rename it,
  as `appkit.Notify` does). labd empties the folder every 15 seconds; see
  "Notifications" for what happens to the files.
- Health: `GET http://127.0.0.1:<port><health>` must return 200 within 30 s
  after (re)start. On failure the previous release is restored and restarted;
  with no previous release the unit is stopped and the instance marked failed
  (no route).
- A new preview first receives a copy of production's data directory (if prod
  exists). Production's app owns that directory, so nothing in it is followed:
  each entry is opened relative to its parent with `O_NOFOLLOW`, links, pipes
  and sockets are skipped, `*.db` files are copied with
  `sqlite3 -nofollow <src> ".backup '<dst>'"` when they are at the top of the
  folder (deeper ones by name only where no folder could be swapped: they are
  copied like other files), other regular files from the descriptor that was
  checked and only as far as their size when checked, `-wal`/`-shm`/`-journal`
  skipped. The top-level
  `secrets/` folder is never copied: **an app keeps API keys and other secrets
  in `$DATA_DIR/secrets/`**, so prototype code in a preview never gets
  production's. The copy runs as `labd -copy-data <src> <dst>` in a throwaway
  unit that can write only the two directories, can't see labd's state or any
  other app's data, has no network or Unix sockets, keeps only the
  capabilities to read and hand over the app's files, and can't change kernel
  settings, modules, namespaces or the clock. It stops at 16 GB, 200000 files, 16
  levels, or when less than 1 GB of disk would be left (the owner: a
  preview gets the whole data even of an app with real mail and files; old
  previews are dumped after shipping to give the disk back). systemd fixes ownership
  on start.
- On start, and every 10 minutes, labd reconciles: every instance that should
  run has its unit running, nginx config matches state, missing certificates are
  retried. When less than 1 GB of disk is left where app data lives, it first
  stops every preview (`"status": "failed"`, `note` says why), and new
  previews are refused (409) until there is room, so a runaway preview can't
  take production, labd or the system updates down with it.
- A rollback is remembered (`"rolled_back": true` in `/api/status`) until the
  next deploy of that instance.
- Planned, not built yet: nightly at 03:30 server time labd backs up every
  production `*.db` to `<state_dir>/backups/<app>/<YYYY-MM-DD>-<file>` with
  `sqlite3 .backup` and keeps 14 days.

## Promote data (a preview's data becomes production's)

For an app whose real data grew in a preview before it shipped. The owner runs
the `promote-data` workflow on main with the branch, the app and the app's
name again; it calls `POST /api/promote-data`, which starts a job, then
polls `GET /api/promote-data` every 10 s (for up to 90 minutes) and ends
with the job's true result: green with the backup's path when it is
`done`, red with the reason when it `failed` (including "its own data was
put back"). While the job runs, a second job for the app and any deploy,
rollback or retire of the app are refused (409); the job holds the lock
every change takes, so changes to other apps wait for it. The job:

1. Fails with nothing changed unless production of the app is running and
   answers its health path, the preview has a data folder, and the disk is
   not nearly full.
2. Stops the preview, then production, so nothing writes while it copies.
3. Renames production's data folder to
   `<data root>/<app>--prod.before-promote-<UTC yyyymmddThhmmssZ>` (on the
   server `/var/lib/private/lab/`). The rename keeps every file as it was;
   this backup is never deleted by promote-data. A second run is safe: it
   makes another backup and copies again.
4. Copies the preview's whole folder into a new production folder with
   `labd -copy-data -all` in the same throwaway unit as preview copies (see
   "Instances"), except that only the new folder is writable. It follows no
   link and skips pipes and sockets like the preview copy, but takes
   **everything else, `secrets/` included**, byte for byte: SQLite's `-wal`
   and `-shm` go with their database, which is consistent because the app
   is stopped. Files keep their owner's permission bits only (0600, 0700),
   folders are 0700. The new folder belongs to root, so systemd gives the
   whole tree to production's user when it starts. Real data outgrows
   the limits of a preview copy, so here only the disk decides: the copy
   stops when less than 1 GB of disk would be left (or at 500000 files).
5. Starts production and waits for its health check. On failure (or a
   failed copy) it removes the new folder (the preview still has that
   data), renames the backup back, starts production again and answers 422
   with `rolled_back: true`.
6. Starts the preview again; it keeps its own copy, secrets included.

The job's state is kept in `<state_dir>/promote/<app>.json`, written before
each step. If labd restarts while a job runs, it marks the job `failed`
before it starts any app and says in `error` what state the data is in:
stopped before anything moved ; copying or putting
back (the partial copy is removed and production's own data put back);
starting on the finished copy (the promoted data stays, not checked
healthy, and the backup's path is given). Reconcile then starts both
instances as usual. The workflow never reports success for a job that did
not finish.

labd's log says each step, the paths and the backup's name, never what is in
the files. Retiring the app (`DELETE /api/instances` with `env=prod`)
removes these backups with the rest of its data.

To restore a backup (by hand, as root on the server; no workflow does it):

```
systemctl stop lab-app-<app>--prod.service
cd /var/lib/private/lab
mv <app>--prod <app>--prod.promoted-<today>      # keep the promoted data aside
mv <app>--prod.before-promote-<time> <app>--prod
chown root:root <app>--prod                      # so systemd hands the tree to the app again
systemctl start lab-app-<app>--prod.service       # or wait for labd's reconcile (10 min)
```

Remove the aside folder once production looks right.

## Notifications (the way to the owner's phone)

- One channel: the Notify app's production instance (an app named `notify`, which is not part of this template; without it messages are logged and dropped), which
  sends Web Push to the devices registered on its page. Everything else on
  the server goes through labd, which posts to
  `http://127.0.0.1:<notify's port>/send` with the token Notify keeps in its
  own `secrets/token` (labd reads it from disk; it is never in GitHub, CI or
  any other app). Topics are Notify's: `security` (urgent), `lab`, `bots`.
- Senders: labd itself; the pipeline and the `security` workflow through
  `POST /api/notify` with their OIDC token; apps through their outbox folder
  (see "Instances"). Sending never blocks or fails the caller: with Notify
  not deployed, not running or without a token, labd logs the drop (at most
  once an hour) and goes on.
- The outbox: every 15 seconds labd reads `<data root>/<app>--<env>/notify/outbox/`
  of every running instance: plain files ending in `.json` only (anything
  else is removed, never followed), at most 16 KB each, at most 20 per
  instance per pass. Each file is removed once read, whatever is in it. A
  production app's message is sent as `<app>: <title>` (default topic
  `bots`; a title is cut at 120 characters, a body at 1000) with a budget of
  30 messages an hour per instance, beyond which messages are dropped and
  logged. A preview's messages are dropped and logged: a preview holds a copy
  of production data and must never reach the phone.
- labd's own messages (topic `lab`): "Deploy of <app> failed" when a
  production deploy fails its health check (the body says whether the
  previous release is serving again); "<app> is down" when a running
  production app stops answering its health check at the 10-minute tick
  (repeated at most every 6 hours while it stays down) and "<app> is back"
  when it answers again; "Lab is alive" once a day at or after 08:00 Europe/Prague
  with how many production apps run, the last audit's verdict and labd's
  version — a day without it means the server or Notify is in trouble. On
  topic `security`: "Server: N critical finding(s)" with the first four
  texts, at most once a day for the same set of findings, sent when
  `GET /api/host/audit` finds them.
- Sent-once state (`<state_dir>/notify-sent.json`: key → time) keeps these
  from repeating across labd restarts; entries older than 7 days are dropped.

## The server

On a systemd server labd keeps the host hardened, at start (failures are
reported in `GET /api/host`, never fatal):

- Kernel settings in `/etc/sysctl.d/90-lab-hardening.conf`, applied at once
  and at every boot: no unprivileged BPF, user namespaces (Debian's
  `kernel.unprivileged_userns_clone`), io_uring, userfaultfd, kexec or line
  discipline autoloading; hidden kernel pointers and log; `ptrace` only for
  root; strict link/FIFO protections; no ICMP redirects or source routing.
  Settings a kernel doesn't have are listed, not errors.
- `/etc/modprobe.d/00-lab-hardening.conf` stops kernel modules this server
  never needs from loading (`install <module> /bin/false`, with an empty
  `softdep` line first, since modprobe ignores install commands of modules
  with soft dependencies): in-kernel SMB/NFS/NVMe
  servers, kernel TLS, the AF_ALG crypto sockets, ebtables, rare network
  protocols, vsock, serial line disciplines, hardware a cloud server lacks,
  unusual file systems. Blocked modules that are loaded but unused are
  unloaded.
- The SSH server is stopped, disabled and masked (no SSH is a design rule; the
  firewall blocks port 22 anyway; the Hetzner console is the emergency door).
- root's password aging is kept sane: Hetzner's images ship root with
  "change the password at the next login", and cron then refuses every job
  it would run for root, logging "Authentication token is no longer valid"
  each hour. labd sets the last-change day to today (and lifts a maximum age
  that ran out); the password itself is not touched. Reported as
  `root_password` in `GET /api/host`.
- The apps' firewall (see "Instances") is also kept in
  `/etc/lab/apps-firewall.nft`, which a drop-in for `nftables.service`
  (`50-lab-apps.conf`) loads right after the system's rules, at boot and on
  every reload, so it never lapses until labd's next reconcile. The same file
  carries the table `inet lab_blocked`: addresses the daily security review
  blocked for scanning the server (`blockedClients` in
  `platform/labd/firewall.go`, one pull request per address, single
  addresses only) are dropped there before the system's rules see them.
- A guard for labd itself: the drop-in
  `/etc/systemd/system/labd.service.d/50-lab-guard.conf`
  (`StartLimitIntervalSec=300`, `StartLimitBurst=5`,
  `OnFailure=labd-fallback.service`) and `labd-fallback.service`, which puts
  `labd.prev` back and starts labd again: a new labd that can't start is
  replaced by the previous one within about a minute.
- `<state_dir>`, `instances/`, each instance's folder and its `releases/` are
  0711 (apps in the basic sandbox pass through them to their binary but can't
  list them); `state.json`, `settings.json`, `secret`, `tokens.json`,
  `auth.json` and `sandbox` are 0600.
- Updates: unattended-upgrades installs them every night and restarts the
  server at 04:30 when an update needs it (setup text); `POST /api/host/upgrade`
  and `POST /api/host/reboot` do it at once.
- Logs: `/etc/systemd/journald.conf.d/50-lab.conf` keeps the journal 30 days
  (`Storage=persistent`, `MaxRetentionSec=30day`, `SystemMaxUse=2G`,
  `SystemKeepFree=5G`, `Compress=yes`). The setup text writes the same file
  for a fresh server; labd rewrites it and restarts journald only when the
  content differs. See "Logging".

## Logging (what apps write, what the lab reads)

- Apps log one JSON object per line to stdout; `lab/platform/appkit` does it
  for them: `appkit.Logger()` (slog, JSON, level INFO and up) adds `time`
  (UTC, RFC 3339), `level` (`DEBUG`, `INFO`, `WARN`, `ERROR`), `msg`, and
  the constant `app`, `env` (`local` when run by hand), `version` (`dev`) from
  the environment labd sets. `appkit.LogRequests` logs every request as
  `msg: "request"` with `method`, `path`, `status`, `ms`, `bytes`, `rid`
  (the request id, from the caller's `X-Request-ID` or a random 8-byte hex,
  echoed in the answer's `X-Request-ID`): INFO for 2xx/3xx, WARN for 4xx,
  ERROR for 5xx; a healthy `/healthz` is not logged. A panic in a handler is
  logged at ERROR (`panic`, `at`) and answered 500. `appkit.Error(msg, err,
  attrs...)` logs at ERROR with the error under `err`.
- journald keeps the lines 30 days (see "The server"). labd reads them with
  `journalctl --unit=lab-app-<app>--<env> --since=-<n>min --output=short-iso`
  (at most 4 MB per instance) and understands both JSON lines and bare
  output: a bare line counts as ERROR when it starts with `panic:` or
  `fatal error:` or contains the word "error", else as INFO.
- `GET /_lab/errors` (the lab's own host, behind the same gate as the home
  page, linked from it) shows every instance's last 24 hours: counts of
  errors, warnings and other lines, and the newest 30 error lines (time in
  Prague, message, `err`, request id), production first, then previews by
  name; an instance whose journal could not be read says so. The data is
  cached for a minute. Czech when the browser prefers it.
- Error-rate alert (every 10 minutes, production instances only): labd counts
  ERROR lines in the last hour and in the 24 hours before; when the last hour
  has at least 10 and at least `3 × (the usual hourly count) + 5`, it sends
  on topic `lab` "<app>: N errors in the last hour" with the three newest
  messages (200 characters each) and the usual rate, linking to
  `/_lab/errors`, at most once per 6 hours per instance (sent-once key
  `errors:<app>--<env>`).

## nginx

labd owns `/etc/nginx/sites-available/lab` (it replaces the bootstrap file) and
`/etc/nginx/conf.d/lab-common.conf`. Every change: write new files, run
`nginx -t`, restore the old files if it fails, else `systemctl reload nginx`.

- Default server on :80 always serves `/.well-known/acme-challenge/` from
  `/var/www/acme`, and proxies `/healthz` and `/api/` to labd (so the pipeline
  can always reach labd by IP). In bootstrap mode it also routes app hosts over
  HTTP.
- Domain mode, :80: every host labd knows redirects to its https address
  (`301`), apart from ACME challenges; a host still waiting for its
  certificate answers 503 instead.
- Domain mode, :443: a default server with `ssl_reject_handshake on` (names
  labd doesn't know get no TLS at all); `<domain>` → labd (`/api/`,
  `/healthz`, `/_lab/` and `/`); each app host with a certificate → its port,
  behind the login gate unless the app is public, with `/_lab/` answering 404
  there (signing in happens only on `<domain>`, so no app's code can run on
  the page where the owner types his password). TLS 1.2 and 1.3 only, ECDHE AEAD
  ciphers, no session tickets, HSTS (`max-age=31536000; includeSubDomains`; an
  app's own HSTS header is dropped), `server_tokens off`, websocket upgrade
  headers, `client_max_body_size 200m`, `X-Lab-Scheme $scheme` and
  `X-Forwarded-Proto $scheme` to upstreams. No HTTP/2: HTTP/1.1 is plenty for
  the lab, and it keeps nginx's HTTP/2 code out of reach.
- Every app request has `lab_session` taken out of its `Cookie` header (two of
  them; a request with more is refused with 400), so no app ever sees a pass,
  not even when a page planted a second cookie to push the real one into
  second place.
- A host gets its 443 block only after its certificate exists
  (`certbot certonly --webroot -w /var/www/acme -d <host> --cert-name <host>
  --non-interactive --agree-tos --register-unsafely-without-email
  --keep-until-expiring --deploy-hook "systemctl reload nginx"`; one certbot at
  a time, waiting while the renewal timer runs one; after a failure that host
  waits 30 minutes, since Let's Encrypt allows few failures an hour).
  `certbot.timer` renews. Removing an instance removes its block and runs
  `certbot delete --cert-name <host> --non-interactive`. Config `"certs":
  "selfsigned"` makes labd's own certificates instead (tests).

## Login gate (domain mode)

- **Open paths.** An app's `app.json` may list up to four `open_paths`, each a
  single lowercase segment (`^/[a-z0-9][a-z0-9-]{0,30}/?$`, never `/_lab/`):
  with a trailing slash it is a prefix (`"/kid/"`, nginx `location ^~`),
  without one a single exact path (`"/send"`, nginx `location =`, so `/send/x`
  stays gated). Requests there skip the access gate and the login gate (in
  both modes) and go straight to the app, still with `lab_session` stripped,
  the two-cookie refusal and HSTS. The app must guard them itself (signed,
  expiring links, or a token of its own as Notify's `/send` does) and must not
  serve anything else there. Everything outside them stays gated. labd refuses
  a manifest or a route with any other shape.
- Every non-public app host: nginx lets the `allow_ips` addresses (home) in
  without a pass; everyone else needs a `lab_session` cookie that `GET
  /_lab/auth` accepts (internal, from nginx with `X-Lab-Host: $host`; 204 or
  401, only to 127.0.0.1). Every `lab_session` cookie is checked, so a
  planted one can't hide the real one. Turned away, a visitor goes to
  `https://<domain>/_lab/login?next=<the address they asked for>` (nginx hands
  the request to labd's `/_lab/redirect`, which builds that address).
- Pages, only on `<domain>` over HTTPS, in Czech for browsers that prefer it
  (or Slovak), else English: `GET/POST /_lab/login` (password),
  `POST /_lab/logout`, `GET/POST /_lab/setup` (setup code + new password, 12
  to 1024 characters; `?code=` fills the code in, so a link can carry it),
  `GET /` (the list of apps and previews; at home or signed in, else to the
  sign-in page), `GET /_lab/lab.css`, `/_lab/icon.png` and
  `/_lab/manifest.webmanifest` (for the iPhone home screen). A form with a
  mistake answers with a redirect back to its page (`?e=...`), so reloading
  never sends it again; a setup code that came in the link is checked at once
  and then kept as a hidden field; once a password is set and no new code is
  waiting, the setup page only points to signing in. No JavaScript. Headers:
  `Content-Security-Policy: default-src 'none'; style-src 'self'; img-src
  'self'; manifest-src 'self'; form-action 'self' https://*.<domain>; frame-ancestors 'none';
  base-uri 'none'`, `X-Frame-Options: DENY`, `Referrer-Policy: no-referrer`,
  `Cache-Control: no-store`. Posts from other sites, other lab hosts included,
  are refused (Go's `http.CrossOriginProtection`).
- After signing in, `next` is followed only to an https address on `<domain>`
  or a name below it (no user info, no port), else to `/`.
- The owner's pass (from signing in or setup): `lab_session` cookie with
  `Domain=<domain>; Path=/; Secure; HttpOnly; SameSite=Lax`, 180 days, valid
  on `<domain>` and every name below it while the password is the same: its
  scope is the first 16 bytes of SHA-256(`"lab-user:" + generation + ":" +
  domain`), and every new password raises the generation, which ends every
  earlier pass. Signing out removes the cookie from that browser.
- The pipeline's pass (`/api/session`): `base64url(expiry uint64 BE || 16
  random bytes || first 16 bytes of SHA-256("lab-host:" + host)) "."
  base64url(HMAC-SHA256(secret, first part))`, valid 15 minutes on that one
  host. The owner's pass has the same shape with its own scope. Secret: 32 random
  bytes in `<state_dir>/secret` (0600).
- Password: PBKDF2-HMAC-SHA256, 600000 iterations, 16-byte salt, in
  `<state_dir>/auth.json` (0600) with the generation and the SHA-256 and expiry
  of the current setup code (never the code itself).
- Guessing: after 5 failed passwords in a minute from one address (IPv6: one
  /64), or 20 from everywhere, sign-in tries are refused until the minute is
  over; wrong setup codes are counted the same way but apart, so setting up
  never locks out signing in, and a new password that is too short or
  doesn't match its repeat is a typo, not a try. At most two passwords are
  checked at once.

## Test server (lab-runner)

GitHub's own machines cost Actions minutes, so the pipeline's jobs run on a
separate Hetzner server, never on the lab server: jobs run code from every
branch, and none of it belongs next to production data.

- Ubuntu 24.04 (as GitHub's machines), set up by `platform/runner/build.py`.
  Nothing comes in (no SSH, no open port); the runner only connects out.
- `lab-runner.service` runs `platform/runner/manager.py` as root. It needs a
  fine-grained GitHub token for this repository with "Administration: read
  and write" (runners) and "Contents: read-only" (updating itself), typed once into the Hetzner web console with
  `lab-runner-token`, or put on the last line (`# token: ...`, a comment, so
  spacing cannot break the text) of the owner's own copy of the setup text at
  Hetzner (the Hetzner console mangles pasted text); the manager reads it
  from cloud-init's root-only copy. It
  lives in `/etc/lab-runner/token` (root, 0600); jobs cannot reach the cloud
  metadata service that serves the setup text. No token is in the repo or in
  CI. The console's login screen shows whether the runner is taking jobs.
- Every job gets a one-job runner (GitHub's just-in-time registration, label
  `lab-runner`) in a fresh, empty home of its slot's own user (`labr1`,
  `labr2`, ...), in its own systemd unit with a private `/tmp`, no new
  privileges and the rest of the system read-only; `/proc` hides other
  users' processes. When the job ends, everything it started is killed and
  its files are deleted before the next job. Jobs never get root.
- Caches come only from `actions/cache`, which GitHub keeps per branch, so a
  branch cannot plant anything a main build or deploy would pick up.
- Jobs pick their machine with `runs-on: ${{ vars.LAB_RUNNER || 'lab-runner' }}`.
  Setting the repository variable `LAB_RUNNER` to `ubuntu-24.04` sends them
  back to GitHub's machines (when the test server is down, say).
- The `integration` job always runs on GitHub's machines: it installs labd,
  nginx and systemd services as root on a throwaway machine, which the test
  server is not. It runs only when the platform changes.
- It keeps itself up to date from the main branch, never from another one:
  every 30 minutes it compares `manager.py`, `lab-runner-token` and
  `install-browser-libs` with main and installs what changed (a new
  `manager.py` must first pass `--selftest`; running jobs carry on through
  the restart), and when the pipeline's `PLAYWRIGHT_VERSION` changes it
  installs the browsers' system libraries for it. The rest of the setup text
  (firewall, packages, users) changes only by rebuilding the server.

## Pipeline side

- `lab.json`: `{"domain": "", "server_ip": "", "repo": "OWNER/REPO",
  "allow_ips": []}`. Deploy steps are skipped (with a notice) while `server_ip`
  is empty. Smoke checks and preview tests get a pass from `/api/session`.
- No app is built without a SPEC.md. Big and small releases (decision): `platform/scripts/release-kind.sh <app> <running.json>` calls
  an app's release *big* when its SPEC.md differs from the SPEC.md at the
  commit production runs (labd's `GET /api/status`, which any ref may read),
  when it is not running in production, or when that cannot be told (status
  unavailable, unknown commit, a forced run); otherwise *small*. The plan
  computes it per app, on branches too, and writes it into the run summary;
  `release-kind-test.sh` tests it in `checks`.
- Branch pushes deploy previews right after planning; `checks` and `apps`
  `need` the `preview` job and start once it has finished or been skipped,
  whatever its result (`!cancelled()`), so a pushed change reaches its preview
  first (the owner). `checks` (and `integration` when the platform
  changed) run after it; no browser tests on a push. The verdict says "Ready to
  ship" only when nothing broke and no app is a big release. The full run is
  `workflow_dispatch` of `pipeline` on the branch with `full: true`: `apps`
  (full matrix, local copy: four browsers) and `preview-tests` (against the
  branch's preview, with a fresh pass from `/api/session` for each of two
  browsers, `chromium-desktop` and `webkit-phone`) run for the
  branch's own apps (`changes.sh` `own`: the apps whose folder changed;
  no app, no test, green; plus the canary, below), and the verdict also
  requires each big release to be `ready`. `changes.sh` `apps` is still what a
  push builds: the changed apps, or all apps when `go.mod`, `go.sum`,
  `platform/appkit/`, `e2e/` or `build-app.sh` changed. Acceptance tests run against a preview only then.
  The verdict notice "Ready to ship" ends with "Tested apps: ..." and "Big
  releases tested: ..."; `platform/scripts/verdict.sh` reads those lines.
- Shared files, quick checks only (the owner): an app that is in
  `apps` only because a shared file changed (not in `own`) is never a big
  release and gets no browser matrix, in a full run on a branch or on main:
  `checks` builds, vets and unit-tests every app and, on main, the deploy job
  ships it with its health check, smoke check and rollback. The plan outputs
  these as `quick` and the summary says "quick checks only: shared files
  changed". On main `own` also covers an app whose folder changed since the
  commit production runs, or that production does not run. Canary: when `e2e/`
  or `platform/appkit/` changed and `own` is empty, `changes.sh` names one app
  (`canary`, `hello`, the smallest; the first app if hello is gone) and it
  runs its matrix (a branch's full run, or main unless the branch run for the
  same commit did); `go.mod`, `go.sum` or `build-app.sh` alone need none.
  The verdict's "Tested apps" and "Big releases tested" keep their meaning:
  only apps that had browser tests are named, so quick apps are in neither, and
  main reads them with `verdict.sh accepted <sha> <apps> ["Tested apps"]`
  (the canary) or the default list "Big releases tested" (big releases).
  Tests: `changes-test.sh` (own, canary, main after such a branch) and
  `verdict-test.sh`.
- Short run (the owner): when an earlier full run on the branch ended
  "Ready to ship" with the app tested, for an ancestor commit X, and
  `short-run.sh X <app>` finds only `*.css`, images, fonts and `*.md` (not
  SPEC.md) changed under `apps/<app>/` since, the full run tests that app in
  `chromium-desktop` and `webkit-phone` locally (`E2E_PROJECTS`) and
  `chromium-desktop` against the preview, and says "short run: only style or
  documents changed since <X>" in the summary. Any other change: whole matrix.
- Main accepts a branch's verdict for the same commit (the owner): in
  the plan, `verdict.sh accepted <sha> <big apps>` asks the GitHub API (the
  job's token, `actions: read` and `checks: read`; `gh api`, or curl when `gh`
  is not installed) for a successful `workflow_dispatch` run of `pipeline.yml`
  on a branch with `head_sha` equal to the commit whose `result` job's "Ready
  to ship" notice lists the app under "Big releases tested". Those apps get no
  acceptance matrix on main; the deploy job builds them, `checks` still runs
  vet, unit tests and the lock check, and the deploy is health- and
  smoke-checked with rollback as always. Anything missing, wrong or unclear:
  the full matrix. The summary says which path each app took.
- The plan's summary shows the test server's size (memory, CPUs and the
  Playwright worker count from `e2e/workers.cjs`, the formula
  `playwright.config.ts` uses).
- main runs `apps` only for big releases (of apps whose own folder changed)
  without an accepted branch verdict, and for the canary if needed
  and refuses a big release that
  isn't `ready` according to `platform/scripts/app-state.sh` (SPEC.md,
  acceptance tests and a matching lock). Small releases are built by the
  deploy job itself with `build-app.sh` after `checks` (vet, unit tests)
  passed; a stale lock does not block them. Production deploys only after
  `checks`, `integration` and `apps` passed or were skipped; every deploy is
  health-checked and smoke-checked with automatic rollback.
- On main, the plan ships what the push changed plus whatever production
  lacks (`GET /api/status`: labd's version, each production app's commit), so
  a change whose run was cancelled or failed ships with the next run. A
  rolled-back app is held: it ships again only when it changes. Branches
  compare with the previous push.
- A push that changes the test harness (`e2e/`, `app-state.sh`,
  `acceptance-lock.sh`, `changes.sh`, `build-app.sh`, the workflows) is flagged
  like a change to an app's SPEC.md or acceptance tests.
- `checks` runs `govulncheck` on all Go code (and Go itself): known reachable
  vulnerabilities fail main and make a branch "Not ready to ship"; on main the
  check must run (no result fails it).
- The deploy job takes each app's build only from that app's own artifact.
  When the artifact store refused it (GitHub's storage quota), the deploy job
  builds the app again from the same commit with `build-app.sh`; the tests
  that gate the job ran on exactly that code. Small releases have no
  artifact and are always built there.
- `integration` (whenever the platform changes) runs
  `platform/scripts/integration-test.sh` on the runner as root: the real labd
  as `labd.service` from the setup text, the systemd runner, nginx; it deploys
  hello and a probe app that tries what a broken-into app would try, and checks
  the hardened sandbox, the gate, preview copies, a simulated reboot, a
  platform update and the guard.
- Actions are pinned to commits (`uses: owner/action@<sha> # vX.Y.Z`).
- labd keeps working on a deploy, rollback, delete or settings change after the
  caller disconnects (a cancelled run), so an instance is never left halfway.
  So `lab-api.sh` retries a change only when labd surely didn't act on it (no
  connection, 502/503 from nginx, 429); after a timeout or a cut connection it
  retries only reads, since a deploy or rollback may already have happened.
- labd endpoint: `https://<domain>` in domain mode, else `http://<server_ip>`.
  `POST /api/platform` and `POST /api/settings` always use `http://<server_ip>`
  (works for the seed and before certificates exist).
- labd build: `CGO_ENABLED=0 go build -trimpath -ldflags "-s -w -X main.version=<sha7>" -o dist/labd ./platform/labd`.
- App build: `platform/scripts/build-app.sh <app>` → `dist/<app>.tar.gz`.
