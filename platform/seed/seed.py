#!/usr/bin/python3
"""labd seed: the first program on a fresh server.

It has exactly one job: accept the real labd binary from this repo's GitHub
Actions pipeline on the main branch, check it, install it over itself and exit
so systemd restarts the unit as the real labd.

Trust comes only from GitHub's OIDC token. The token's audience must be
"lab-platform:sha256:<hex digest of the uploaded binary>", so a token is bound
to exactly one file: it cannot be reused for a different body, and the jti is
remembered so it cannot be reused at all. That makes the upload safe even over
plain HTTP before the server has a domain or certificates.

Standard library only (runs on Debian's python3).
"""

import base64
import hashlib
import hmac
import http.server
import json
import os
import re
import subprocess
import sys
import tempfile
import threading
import time
import urllib.request

VERSION = "labd-seed 1"
ISSUER = "https://token.actions.githubusercontent.com"
CONFIG = os.environ.get("LAB_SEED_CONFIG", "/etc/lab/labd.json")
TARGET = os.environ.get("LAB_SEED_TARGET", "/usr/local/bin/labd")
JWKS_URL = os.environ.get("LAB_JWKS_URL", ISSUER + "/.well-known/jwks")
MAX_BODY = 200 * 1024 * 1024
SKEW = 60
AUD_RE = re.compile(r"^lab-platform:sha256:([0-9a-f]{64})$")
# DER prefix of DigestInfo for SHA-256 (RFC 8017, section 9.2, note 1).
SHA256_DIGEST_INFO = bytes.fromhex("3031300d060960864801650304020105000420")


def log(msg):
    print(msg, file=sys.stderr, flush=True)


def b64url_decode(data):
    if isinstance(data, str):
        data = data.encode("ascii")
    if not re.fullmatch(rb"[A-Za-z0-9_-]*", data):
        raise ValueError("invalid base64url")
    return base64.urlsafe_b64decode(data + b"=" * (-len(data) % 4))


class Keys:
    """GitHub's signing keys, cached for 10 minutes, refreshed on unknown kid
    at most once a minute."""

    def __init__(self, url):
        self.url = url
        self.keys = {}
        self.fetched = 0.0
        self.lock = threading.Lock()

    def _fetch(self):
        with urllib.request.urlopen(self.url, timeout=10) as resp:
            data = json.load(resp)
        keys = {}
        for k in data.get("keys", []):
            if k.get("kty") == "RSA" and k.get("kid") and k.get("n") and k.get("e"):
                keys[k["kid"]] = (
                    int.from_bytes(b64url_decode(k["n"]), "big"),
                    int.from_bytes(b64url_decode(k["e"]), "big"),
                )
        self.keys = keys
        self.fetched = time.time()

    def get(self, kid):
        with self.lock:
            age = time.time() - self.fetched
            if age > 600 or (kid not in self.keys and age > 60):
                self._fetch()
            return self.keys.get(kid)


def rs256_valid(n, e, signing_input, signature):
    """RSASSA-PKCS1-v1_5 verification by full re-encoding and comparison."""
    k = (n.bit_length() + 7) // 8
    if k < 256 or len(signature) != k:
        return False
    s = int.from_bytes(signature, "big")
    if s >= n:
        return False
    em = pow(s, e, n).to_bytes(k, "big")
    t = SHA256_DIGEST_INFO + hashlib.sha256(signing_input).digest()
    expected = b"\x00\x01" + b"\xff" * (k - len(t) - 3) + b"\x00" + t
    return hmac.compare_digest(em, expected)


class Verifier:
    def __init__(self, repo, keys):
        self.repo = repo
        self.keys = keys
        self.seen = {}
        self.lock = threading.Lock()

    def claims(self, token):
        """Check signature, issuer, time and repository. Returns the claims.
        The audience and the jti are checked by the caller."""
        parts = token.split(".")
        if len(parts) != 3:
            raise ValueError("malformed token")
        header = json.loads(b64url_decode(parts[0]))
        if header.get("alg") != "RS256":
            raise ValueError("unsupported algorithm")
        key = self.keys.get(header.get("kid"))
        if key is None:
            raise ValueError("unknown signing key")
        signing_input = (parts[0] + "." + parts[1]).encode("ascii")
        if not rs256_valid(key[0], key[1], signing_input, b64url_decode(parts[2])):
            raise ValueError("bad signature")
        c = json.loads(b64url_decode(parts[1]))
        now = time.time()
        if c.get("iss") != ISSUER:
            raise ValueError("wrong issuer")
        if not isinstance(c.get("exp"), (int, float)) or c["exp"] < now - SKEW:
            raise ValueError("token expired")
        if c.get("nbf", 0) > now + SKEW or c.get("iat", 0) > now + SKEW:
            raise ValueError("token not valid yet")
        if c.get("repository") != self.repo:
            raise ValueError("wrong repository")
        if c.get("ref") != "refs/heads/main":
            raise ValueError("only the main branch may install labd")
        if not isinstance(c.get("jti"), str) or not c["jti"]:
            raise ValueError("token has no jti")
        return c

    def consume(self, claims):
        now = time.time()
        with self.lock:
            for jti, until in list(self.seen.items()):
                if until < now:
                    del self.seen[jti]
            if claims["jti"] in self.seen:
                raise PermissionError("token already used")
            self.seen[claims["jti"]] = claims["exp"] + SKEW


def expected_digest(claims):
    aud = claims.get("aud")
    auds = aud if isinstance(aud, list) else [aud]
    digests = [m.group(1) for m in (AUD_RE.match(a) for a in auds if isinstance(a, str)) if m]
    if len(digests) != 1:
        raise ValueError("audience must be lab-platform:sha256:<digest>")
    return digests[0]


class Handler(http.server.BaseHTTPRequestHandler):
    server_version = "labd-seed"
    sys_version = ""
    verifier = None
    # A client that stops sending is dropped instead of holding a thread and
    # a half-written upload forever.
    timeout = int(os.environ.get("LAB_SEED_TIMEOUT", "60"))

    def log_message(self, fmt, *args):
        log("%s %s" % (self.address_string(), fmt % args))

    def reply(self, code, obj):
        body = (json.dumps(obj) + "\n").encode()
        self.send_response(code)
        self.send_header("Content-Type", "application/json")
        self.send_header("Content-Length", str(len(body)))
        self.end_headers()
        self.wfile.write(body)

    def do_GET(self):
        if self.path == "/healthz":
            return self.reply(200, {"ok": True, "stage": "seed", "version": VERSION})
        self.reply(404, {"error": "not found"})

    def do_POST(self):
        if self.path != "/api/platform":
            return self.reply(404, {"error": "not found"})
        auth = self.headers.get("Authorization", "")
        if not auth.startswith("Bearer "):
            return self.reply(401, {"error": "missing bearer token"})
        try:
            claims = self.verifier.claims(auth[len("Bearer "):].strip())
            want = expected_digest(claims)
        except Exception as exc:  # any parsing or validation failure
            log("rejected token: %s" % exc)
            return self.reply(403, {"error": str(exc)})
        try:
            length = int(self.headers.get("Content-Length", ""))
        except ValueError:
            return self.reply(411, {"error": "Content-Length required"})
        if length <= 0 or length > MAX_BODY:
            return self.reply(413, {"error": "body must be 1 byte to %d bytes" % MAX_BODY})

        fd, tmp = tempfile.mkstemp(prefix=".labd-", dir=os.path.dirname(TARGET))
        try:
            digest = hashlib.sha256()
            remaining = length
            with os.fdopen(fd, "wb") as f:
                while remaining:
                    chunk = self.rfile.read(min(remaining, 1 << 20))
                    if not chunk:
                        raise ValueError("upload ended early")
                    digest.update(chunk)
                    f.write(chunk)
                    remaining -= len(chunk)
                f.flush()
                os.fsync(f.fileno())
            if not hmac.compare_digest(digest.hexdigest(), want):
                raise PermissionError("upload does not match the token's digest")
            self.verifier.consume(claims)
            os.chmod(tmp, 0o755)
            run = subprocess.run([tmp, "-version"], capture_output=True, timeout=15)
            out = (run.stdout + run.stderr).decode("utf-8", "replace").strip()[-2000:]
            if run.returncode != 0:
                raise ValueError("new labd failed '-version': " + out)
            os.replace(tmp, TARGET)
            dirfd = os.open(os.path.dirname(TARGET), os.O_DIRECTORY)
            try:
                os.fsync(dirfd)
            finally:
                os.close(dirfd)
        except PermissionError as exc:
            os.path.exists(tmp) and os.unlink(tmp)
            return self.reply(403, {"error": str(exc)})
        except Exception as exc:
            os.path.exists(tmp) and os.unlink(tmp)
            log("install failed: %s" % exc)
            return self.reply(400, {"error": str(exc)})

        log("installed %s (sha256 %s, commit %s); restarting" % (out, want, claims.get("sha")))
        self.reply(200, {"ok": True, "installed": out, "sha256": want, "commit": claims.get("sha")})
        self.wfile.flush()
        # systemd (Restart=always) starts the unit again, now as the real labd.
        threading.Timer(0.5, lambda: os._exit(0)).start()


def main():
    if "-version" in sys.argv[1:] or "--version" in sys.argv[1:]:
        print(VERSION)
        return 0
    with open(CONFIG) as f:
        cfg = json.load(f)
    repo = cfg["repo"]
    host, _, port = cfg.get("listen", "127.0.0.1:7070").rpartition(":")
    Handler.verifier = Verifier(repo, Keys(JWKS_URL))
    server = http.server.ThreadingHTTPServer((host, int(port)), Handler)
    log("%s waiting for labd from %s (main) on %s:%s" % (VERSION, repo, host, port))
    server.serve_forever()
    return 0


if __name__ == "__main__":
    sys.exit(main())
