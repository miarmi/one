#!/usr/bin/python3
"""lab-runner: runs GitHub Actions jobs of this repo on the test server.

Runs as root on the test server (never on the lab server). For each slot it
loops: wipe the slot's user (files, processes), ask GitHub for a one-job
runner registration (just-in-time config), unpack a fresh copy of the
official runner into the user's home and run it as that user in its own
systemd unit. The runner takes exactly one job and exits; systemd then kills
whatever the job left running, and the next round starts from nothing again.

So every job starts as on GitHub's own machines: an empty home, no process
left by an earlier job, and no way to see another slot's job (own user,
/proc hidden). Caches come only from actions/cache, which GitHub keeps per
branch, so a branch can never plant anything a main build would pick up.

It also keeps itself up to date from the repository's main branch (see
"keeping itself up to date" below).

The token (a fine-grained GitHub token on this one repository with
"Administration: read and write", and "Contents: read-only" for the updates) lives only in /etc/lab-runner/token, readable by
root alone; `lab-runner-token` stores it. Without it the manager waits.

Standard library only. Usage: manager.py [config path]
"""

import hashlib
import json
import os
import re
import shutil
import subprocess
import sys
import tempfile
import threading
import time
import urllib.error
import urllib.request

CONFIG = "/etc/lab-runner/config.json"
TOKEN = "/etc/lab-runner/token"
CACHE = "/var/lib/lab-runner"
API = "https://api.github.com"
LABEL = "lab-runner"
TARBALL_RE = r"actions-runner-linux-x64-(\d+\.\d+\.\d+)\.tar\.gz"


def log(*parts):
    print(*parts, flush=True)


def load_config(path):
    with open(path) as f:
        cfg = json.load(f)
    if not re.fullmatch(r"[A-Za-z0-9-]+/[A-Za-z0-9._-]+", cfg.get("repo", "")):
        raise SystemExit("config: repo must be owner/name")
    slots = int(cfg.get("slots", 2))
    if not 1 <= slots <= 8:
        raise SystemExit("config: slots must be 1..8")
    # Each job needs about 3.5 GB (an app build, five app instances, browsers):
    # a smaller server runs fewer jobs at a time instead of starving them all.
    cfg["slots"] = min(slots, slots_for_memory(memory_bytes()))
    cfg.setdefault("name", "lab-runner")
    return cfg


def memory_bytes():
    try:
        with open("/proc/meminfo") as f:
            for line in f:
                if line.startswith("MemTotal:"):
                    return int(line.split()[1]) * 1024
    except OSError:
        pass
    return 0


def slots_for_memory(total):
    return max(1, int(total / (3.5 * 2**30))) if total else 1


PLACEHOLDER = "PASTE_" + "TOKEN_HERE"  # split, so the setup text has the word only once
ISSUE = "/etc/issue.d/lab-runner.issue"


def clean(text):
    """The token as pasted: without spaces, line breaks or quotes around it."""
    return "".join(text.split()).strip("\"'`")


USER_DATA = "/var/lib/cloud/instance/user-data.txt"  # the setup text, root only


def token_from_setup(text):
    """The token from the setup text's last-line comment `# token: ...`, or None."""
    found = None
    for line in text.splitlines():
        # Only at the start of a line: embedded files are indented.
        # A deleted '#' still counts (cloud-init ignores the unknown key).
        m = re.match(r"#?\s*token\b\s*:?(.*)$", line, re.IGNORECASE)
        if m:
            found = clean(m.group(1))
    return found if found and found != PLACEHOLDER else None


def read_token():
    """The token from lab-runner-token, or else from the setup text."""
    try:
        with open(TOKEN) as f:
            token = clean(f.read())
        if token and token != PLACEHOLDER:
            return token
    except FileNotFoundError:
        pass
    try:
        with open(USER_DATA) as f:
            return token_from_setup(f.read())
    except OSError:
        return None


def describe(token):
    """What a token looks like, without giving it away."""
    return "%d characters, %s...%s" % (len(token), token[:11], token[-4:])


def shape_problem(token):
    """Why a token cannot be the right kind, judged by its look alone, or None.

    The right kind is a fine-grained personal access token: github_pat_ and 82
    more letters, digits and underscores (93 in all)."""
    if re.fullmatch(r"[A-Z0-9]{29}", token):
        return ("this is a runner registration token (from Settings > Actions > Runners), "
                "the wrong kind. Make a fine-grained token: "
                "github.com/settings/personal-access-tokens/new")
    if re.fullmatch(r"gh[pousr]_[A-Za-z0-9]{36,}", token):
        return ("this is a classic token (%s...), the wrong kind. Make a fine-grained token: "
                "github.com/settings/personal-access-tokens/new" % token[:4])
    if not token.startswith("github_pat_"):
        return ("this does not look like a GitHub token (%s). A fine-grained token "
                "starts with github_pat_" % describe(token))
    if not re.fullmatch(r"github_pat_[A-Za-z0-9_]{60,}", token):
        bad = sorted(set(c for c in token[11:] if not (c.isascii() and (c.isalnum() or c == "_"))))
        if bad:
            return ("the token contains characters a GitHub token never has (%s): it was "
                    "changed on the way (%s)" % (" ".join(bad), describe(token)))
        return "the token is too short, part of it is missing (%s)" % describe(token)
    return None


def check_token(repo, token):
    """None when the token can register runners for repo, else what is wrong."""
    problem = shape_problem(token)
    if problem:
        return problem
    try:
        api("GET", "/repos/%s/actions/runners" % repo, token)
    except urllib.error.HTTPError as e:
        if e.code == 401:
            return ("GitHub does not know this token (%s): it was deleted, regenerated "
                    "since, or expired. Regenerate it on GitHub and run lab-runner-token."
                    % describe(token))
        if e.code in (403, 404):
            return ("the token is valid but may not manage runners of %s (%d): give it "
                    "access to %s with Administration: Read and write." % (repo, e.code, repo))
        return "GitHub answered %d; trying again" % e.code
    except (urllib.error.URLError, OSError) as e:
        return "cannot reach GitHub (%s); trying again" % e
    return None


def status(text):
    """Show the runner's state on the Hetzner console's login screen."""
    try:
        os.makedirs(os.path.dirname(ISSUE), exist_ok=True)
        with open(ISSUE, "w") as f:
            f.write("lab-runner: %s\n\n" % text)
    except OSError:
        pass
    subprocess.run(["agetty", "--reload"], check=False,
                   stdout=subprocess.DEVNULL, stderr=subprocess.DEVNULL)


def api(method, path, token=None, body=None):
    data = json.dumps(body).encode() if body is not None else None
    headers = {
        "Accept": "application/vnd.github+json",
        "X-GitHub-Api-Version": "2022-11-28",
        "User-Agent": "lab-runner",
    }
    if token:
        headers["Authorization"] = "Bearer " + token
    req = urllib.request.Request(API + path, data=data, method=method, headers=headers)
    with urllib.request.urlopen(req, timeout=30) as resp:
        return json.load(resp)


def api_raw(path, token):
    """A file's bytes through the contents API (works for private repositories)."""
    req = urllib.request.Request(API + path, headers={
        "Accept": "application/vnd.github.raw",
        "X-GitHub-Api-Version": "2022-11-28",
        "User-Agent": "lab-runner",
        "Authorization": "Bearer " + token,
    })
    with urllib.request.urlopen(req, timeout=60) as resp:
        return resp.read()


# ---------- keeping itself up to date ----------
#
# The setup text installs these once; afterwards they follow the repository's
# main branch, the same code production is deployed from. Branches never reach
# the test server's root this way. Needs "Contents: Read-only" on the token.

UPDATE_EVERY = 30 * 60
SELF = os.path.abspath(__file__)
UPDATED_FILES = [
    ("platform/runner/manager.py", SELF),
    ("platform/runner/lab-runner-token", "/usr/local/sbin/lab-runner-token"),
    ("platform/runner/install-browser-libs", "/usr/local/sbin/install-browser-libs"),
]
PIPELINE = ".github/workflows/pipeline.yml"
LIBS_STATE = os.path.join(CACHE, "browser-libs-version")


def playwright_version(pipeline_text):
    m = re.search(r"^\s*PLAYWRIGHT_VERSION:\s*['\"]?([0-9]+\.[0-9]+\.[0-9]+)", pipeline_text, re.M)
    return m.group(1) if m else None


def selftest(code, config=CONFIG):
    """True when a new manager.py starts: it compiles and its --selftest passes."""
    try:
        compile(code, "manager.py", "exec")
    except SyntaxError:
        return False
    with tempfile.NamedTemporaryFile("wb", suffix=".py", delete=False) as f:
        f.write(code)
    try:
        return subprocess.run([sys.executable, "-I", f.name, "--selftest", config],
                              stdout=subprocess.DEVNULL, stderr=subprocess.DEVNULL,
                              timeout=60).returncode == 0
    except subprocess.TimeoutExpired:
        return False
    finally:
        os.remove(f.name)


def write_file(dest, data, mode):
    tmp = dest + ".new"
    fd = os.open(tmp, os.O_WRONLY | os.O_CREAT | os.O_TRUNC, mode)
    with os.fdopen(fd, "wb") as f:
        f.write(data)
    os.chmod(tmp, mode)
    os.replace(tmp, dest)


def update_once(repo, token):
    """Bring the installed files and the browsers' libraries up to main.
    Returns True when manager.py itself changed (the caller restarts)."""
    restart = False
    for path, dest in UPDATED_FILES:
        new = api_raw("/repos/%s/contents/%s?ref=main" % (repo, path), token)
        try:
            with open(dest, "rb") as f:
                old = f.read()
        except FileNotFoundError:
            old = None
        if new == old:
            continue
        if dest == SELF:
            if not selftest(new):
                log("update: the new manager.py does not start; keeping this one")
                continue
            restart = True
        write_file(dest, new, 0o755)
        log("update: installed", path, "from main")
    version = playwright_version(api_raw("/repos/%s/contents/%s?ref=main" % (repo, PIPELINE), token).decode())
    try:
        with open(LIBS_STATE) as f:
            have = f.read().strip()
    except FileNotFoundError:
        have = None
    if version and version != have:
        log("update: browser libraries for Playwright", version)
        if subprocess.run(["/usr/local/sbin/install-browser-libs", version], check=False).returncode == 0:
            write_file(LIBS_STATE, (version + "\n").encode(), 0o644)
    return restart


def update_loop(cfg, good_token):
    time.sleep(60)
    while True:
        token = good_token()
        if token:
            try:
                if update_once(cfg["repo"], token):
                    log("update: restarting with the new manager (running jobs carry on)")
                    subprocess.run(["systemctl", "restart", "--no-block", "lab-runner.service"], check=False)
            except urllib.error.HTTPError as e:
                if e.code in (403, 404):
                    status("taking jobs, but cannot update itself: give the GitHub token "
                           "Contents: Read-only on %s" % cfg["repo"])
                log("update: GitHub answered", e.code)
            except (urllib.error.URLError, OSError, ValueError) as e:
                log("update:", e)
        time.sleep(UPDATE_EVERY)


# ---------- the official runner ----------

def runner_asset(release):
    """(version, download URL, sha256) of the Linux x64 runner in a release.

    GitHub lists each asset's SHA-256 in the release notes; without it the
    download is not trusted."""
    for asset in release.get("assets", []):
        m = re.fullmatch(TARBALL_RE, asset.get("name", ""))
        if not m:
            continue
        name = asset["name"]
        # The notes say: <!-- BEGIN SHA linux-x64 -->hash<!-- END SHA linux-x64 -->
        sha = re.search(r"BEGIN SHA linux-x64 -->\s*([0-9a-f]{64})\s*<!-- END SHA linux-x64",
                        release.get("body") or "")
        if not sha:
            sha = re.search(re.escape(name) + r"\W+([0-9a-f]{64})", release.get("body") or "")
        if not sha:
            raise ValueError("release notes have no SHA-256 for " + name)
        return m.group(1), asset["browser_download_url"], sha.group(1)
    raise ValueError("release has no Linux x64 runner")


def fetch_runner():
    """Download the newest runner once; return the local tarball path."""
    # A public release: asked without the token, so a token problem can't hide here.
    version, url, sha = runner_asset(api("GET", "/repos/actions/runner/releases/latest"))
    dest = os.path.join(CACHE, "actions-runner-%s.tar.gz" % version)
    if os.path.exists(dest):
        return dest
    tmp = dest + ".part"
    h = hashlib.sha256()
    with urllib.request.urlopen(url, timeout=300) as resp, open(tmp, "wb") as out:
        while chunk := resp.read(1 << 20):
            h.update(chunk)
            out.write(chunk)
    if h.hexdigest() != sha:
        os.remove(tmp)
        raise ValueError("runner %s: SHA-256 does not match the release notes" % version)
    # The runner's own list of system libraries (.NET needs ICU and the like),
    # from the checked download, once per version.
    with tempfile.TemporaryDirectory() as unpack:
        subprocess.run(["tar", "-xzf", tmp, "-C", unpack], check=True)
        subprocess.run([os.path.join(unpack, "bin", "installdependencies.sh")], check=True,
                       stdout=subprocess.DEVNULL)
    os.replace(tmp, dest)
    for old in os.listdir(CACHE):
        if old.startswith("actions-runner-") and os.path.join(CACHE, old) != dest:
            os.remove(os.path.join(CACHE, old))
    log("runner", version, "downloaded")
    return dest


# ---------- one slot ----------

def slot_user(n):
    return "labr%d" % n


def unit_name(n):
    return "lab-runner-job-%d" % n


def wipe(user):
    """Kill every process of the user and empty its home."""
    subprocess.run(["systemctl", "stop", unit_name(int(user[4:]))], check=False,
                   stdout=subprocess.DEVNULL, stderr=subprocess.DEVNULL)
    subprocess.run(["pkill", "-KILL", "-u", user], check=False)
    # Shared memory and the like outlive the job's private /tmp.
    for shared in ("/dev/shm", "/var/tmp"):
        subprocess.run(["find", shared, "-mindepth", "1", "-maxdepth", "1", "-user", user,
                        "-exec", "rm", "-rf", "{}", "+"], check=False)
    home = "/home/" + user
    if os.path.isdir(home):
        for entry in os.listdir(home):
            p = os.path.join(home, entry)
            if os.path.isdir(p) and not os.path.islink(p):
                shutil.rmtree(p, ignore_errors=True)
            else:
                os.remove(p)
    else:
        os.makedirs(home, mode=0o700)
    shutil.chown(home, user, user)
    os.chmod(home, 0o700)


def job_command(n, user, slots):
    """systemd-run line for one job: own unit, own user, private /tmp."""
    home = "/home/" + user
    return [
        "systemd-run", "--wait", "--collect", "--quiet",
        "--unit=" + unit_name(n),
        "--uid=" + user, "--gid=" + user,
        "--working-directory=" + home + "/runner",
        "--property=PrivateTmp=yes",
        "--property=NoNewPrivileges=yes",
        "--property=ProtectSystem=strict",
        "--property=ReadWritePaths=" + home,
        "--property=ProtectKernelTunables=yes",
        "--property=ProtectKernelModules=yes",
        "--property=ProtectControlGroups=yes",
        "--property=RestrictSUIDSGID=yes",
        "--property=KillMode=control-group",
        # When the kernel kills work toolcess for memory (a browser, say), only
        # that test fails; systemd's default would stop the whole job.
        "--property=OOMPolicy=continue",
        "--property=RuntimeMaxSec=4h",
        "--property=MemoryMax=%d%%" % (90 // slots),
        "--setenv=HOME=" + home,
        # The one-job registration is read from a 0600 file, so it shows up
        # neither in the unit (which any user can list) nor, with /proc
        # hidden, in another slot's view of the processes.
        "/bin/bash", "-c", 'exec ./run.sh --jitconfig "$(cat "$HOME/.jit")"',
    ]


def runner_env():
    # Variables the runner hands to every job (its .env file).
    return "LAB_RUNNER=1\nLANG=C.UTF-8\n"


def job_running(n):
    return subprocess.run(["systemctl", "is-active", "--quiet", unit_name(n)], check=False).returncode == 0


def run_one(n, cfg, token, tarball):
    user = slot_user(n)
    home = "/home/" + user
    # A job from before a restart of this manager (an update restarts it) keeps
    # running in its own unit: let it finish instead of killing it.
    while job_running(n):
        time.sleep(10)
    wipe(user)
    reg = api("POST", "/repos/%s/actions/runners/generate-jitconfig" % cfg["repo"], token, {
        "name": "%s-%d-%d" % (cfg["name"], n, int(time.time())),
        "runner_group_id": 1,
        "labels": [LABEL],
        "work_folder": "_work",
    })
    os.makedirs(home + "/runner")
    subprocess.run(["tar", "-xzf", tarball, "-C", home + "/runner"], check=True)
    with open(home + "/runner/.env", "w") as f:
        f.write(runner_env())
    fd = os.open(home + "/.jit", os.O_WRONLY | os.O_CREAT | os.O_TRUNC, 0o600)
    with os.fdopen(fd, "w") as f:
        f.write(reg["encoded_jit_config"])
    subprocess.run(["chown", "-R", "%s:%s" % (user, user), home], check=True)
    log("slot", n, "waiting for a job")
    rc = subprocess.run(job_command(n, user, cfg["slots"]), check=False).returncode
    log("slot", n, "runner exited", rc)
    wipe(user)


def slot_loop(n, cfg, state):
    backoff = 10
    while True:
        token = state["good_token"]()
        if not token:
            time.sleep(10)
            continue
        try:
            run_one(n, cfg, token, state["tarball"]())
            backoff = 10
        except (urllib.error.URLError, OSError, ValueError, KeyError, subprocess.CalledProcessError) as e:
            log("slot", n, "error:", e)
            if isinstance(e, urllib.error.HTTPError) and e.code in (401, 403, 404):
                state["recheck"]()
            else:
                status("error, trying again: %s" % e)
            time.sleep(backoff)
            backoff = min(backoff * 2, 600)


def set_token(cfg):
    """lab-runner-token: ask for the token, check it, store it."""
    import getpass
    repo = cfg["repo"]
    print("Paste the fine-grained GitHub token for %s (it is not shown) and press Enter." % repo)
    token = clean(getpass.getpass("Token: "))
    if not token:
        print("No token given; nothing changed.")
        return 1
    problem = check_token(repo, token)
    if problem:
        print("Not saved: " + problem)
        return 1
    os.makedirs(os.path.dirname(TOKEN), exist_ok=True)
    fd = os.open(TOKEN + ".new", os.O_WRONLY | os.O_CREAT | os.O_TRUNC, 0o600)
    with os.fdopen(fd, "w") as f:
        f.write(token + "\n")
    os.replace(TOKEN + ".new", TOKEN)
    subprocess.run(["systemctl", "restart", "lab-runner.service"], check=False)
    print("Saved (%s). GitHub accepts it; the test server takes jobs within a minute." % describe(token))
    return 0


def main():
    args = sys.argv[1:]
    if args and args[0] == "--set-token":
        cfg = load_config(args[1] if len(args) > 1 else CONFIG)
        sys.exit(set_token(cfg))
    if args and args[0] == "--selftest":
        # A new version proves it can start before it replaces this one.
        load_config(args[1] if len(args) > 1 else CONFIG)
        job_command(1, slot_user(1), 1)
        sys.exit(0)
    cfg = load_config(args[0] if args else CONFIG)
    os.makedirs(CACHE, exist_ok=True)
    lock = threading.Lock()
    cached = {"path": None, "at": 0.0}
    checked = {"token": None, "ok": False, "at": 0.0}

    def tarball():
        # Look for a newer runner at most once a day; GitHub stops sending jobs
        # to runners that fall too far behind.
        with lock:
            if cached["path"] is None or time.time() - cached["at"] > 86400:
                try:
                    cached["path"] = fetch_runner()
                    cached["at"] = time.time()
                except Exception:
                    if cached["path"] is None:
                        raise
            return cached["path"]

    def good_token():
        """The stored token once GitHub accepted it; checked again when it changes
        or GitHub refuses it, and at most every 30 s while it is wrong."""
        token = read_token()
        with lock:
            if not token:
                status("waiting for the GitHub token: log in as root and run lab-runner-token")
                checked.update(token=None, ok=False)
                return None
            if token == checked["token"] and (checked["ok"] or time.time() - checked["at"] < 30):
                return token if checked["ok"] else None
            problem = check_token(cfg["repo"], token)
            checked.update(token=token, ok=problem is None, at=time.time())
            if problem:
                log("token:", problem)
                status(problem)
                return None
            log("token accepted:", describe(token))
            status("GitHub accepts the token; taking jobs (%d at a time)" % cfg["slots"])
            return token

    def recheck():
        with lock:
            checked.update(ok=False, at=0.0)

    state = {"tarball": tarball, "good_token": good_token, "recheck": recheck}
    threads = [threading.Thread(target=slot_loop, args=(n, cfg, state), daemon=True)
               for n in range(1, cfg["slots"] + 1)]
    threads.append(threading.Thread(target=update_loop, args=(cfg, good_token), daemon=True))
    for t in threads:
        t.start()
    for t in threads:
        t.join()


if __name__ == "__main__":
    main()
