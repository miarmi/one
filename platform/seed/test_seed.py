#!/usr/bin/python3
"""End-to-end test for seed.py: runs the real seed against a local JWKS server
with tokens signed by a throwaway key (openssl). Standard library + openssl CLI.

Run: python3 platform/seed/test_seed.py
"""

import base64
import hashlib
import http.server
import json
import os
import socket
import subprocess
import sys
import tempfile
import threading
import time
import unittest
import urllib.error
import urllib.request

HERE = os.path.dirname(os.path.abspath(__file__))
SEED = os.path.join(HERE, "seed.py")
REPO = "OWNER/REPO"
ISSUER = "https://token.actions.githubusercontent.com"


def b64url(data):
    return base64.urlsafe_b64encode(data).rstrip(b"=").decode()


def free_port():
    with socket.socket() as s:
        s.bind(("127.0.0.1", 0))
        return s.getsockname()[1]


class SeedTest(unittest.TestCase):
    @classmethod
    def setUpClass(cls):
        cls.tmp = tempfile.mkdtemp(prefix="seedtest-")
        cls.key = os.path.join(cls.tmp, "key.pem")
        cls.other_key = os.path.join(cls.tmp, "other.pem")
        for path in (cls.key, cls.other_key):
            subprocess.run(["openssl", "genrsa", "-out", path, "2048"], check=True, capture_output=True)
        mod = subprocess.run(["openssl", "rsa", "-in", cls.key, "-noout", "-modulus"],
                             check=True, capture_output=True, text=True).stdout.strip().split("=", 1)[1]
        jwks = {"keys": [{"kty": "RSA", "kid": "k1", "alg": "RS256", "use": "sig",
                          "n": b64url(bytes.fromhex(mod)), "e": "AQAB"}]}
        body = json.dumps(jwks).encode()

        class JWKS(http.server.BaseHTTPRequestHandler):
            def do_GET(self):
                self.send_response(200)
                self.send_header("Content-Type", "application/json")
                self.send_header("Content-Length", str(len(body)))
                self.end_headers()
                self.wfile.write(body)

            def log_message(self, *a):
                pass

        cls.jwks_server = http.server.ThreadingHTTPServer(("127.0.0.1", 0), JWKS)
        threading.Thread(target=cls.jwks_server.serve_forever, daemon=True).start()

        cls.port = free_port()
        cls.config = os.path.join(cls.tmp, "labd.json")
        with open(cls.config, "w") as f:
            json.dump({"repo": REPO, "listen": "127.0.0.1:%d" % cls.port}, f)
        cls.bindir = os.path.join(cls.tmp, "bin")
        os.mkdir(cls.bindir)
        cls.target = os.path.join(cls.bindir, "labd")
        with open(cls.target, "w") as f:
            f.write("original\n")
        env = dict(os.environ, LAB_SEED_CONFIG=cls.config, LAB_SEED_TARGET=cls.target, LAB_SEED_TIMEOUT="2",
                   LAB_JWKS_URL="http://127.0.0.1:%d/jwks" % cls.jwks_server.server_address[1])
        cls.log = open(os.path.join(cls.tmp, "seed.log"), "w+")
        cls.proc = subprocess.Popen([sys.executable, SEED], env=env, stdout=cls.log, stderr=cls.log)
        deadline = time.time() + 10
        while time.time() < deadline:
            try:
                urllib.request.urlopen("http://127.0.0.1:%d/healthz" % cls.port, timeout=1)
                break
            except Exception:
                time.sleep(0.1)
        else:
            raise RuntimeError("seed did not start")

    @classmethod
    def tearDownClass(cls):
        if cls.proc.poll() is None:
            cls.proc.kill()
        cls.jwks_server.shutdown()

    # helpers

    def token(self, digest=None, key=None, kid="k1", alg="RS256", **overrides):
        now = int(time.time())
        claims = {"iss": ISSUER, "aud": "lab-platform:sha256:" + (digest or "0" * 64),
                  "repository": REPO, "ref": "refs/heads/main", "sha": "abc1234",
                  "jti": os.urandom(8).hex(), "iat": now, "nbf": now, "exp": now + 300}
        claims.update(overrides)
        claims = {k: v for k, v in claims.items() if v is not None}
        head = b64url(json.dumps({"alg": alg, "kid": kid, "typ": "JWT"}).encode())
        payload = b64url(json.dumps(claims).encode())
        signing_input = (head + "." + payload).encode()
        sig = subprocess.run(["openssl", "dgst", "-sha256", "-sign", key or self.key],
                             input=signing_input, check=True, capture_output=True).stdout
        return head + "." + payload + "." + b64url(sig)

    def post(self, body, token=None, path="/api/platform"):
        req = urllib.request.Request("http://127.0.0.1:%d%s" % (self.port, path), data=body, method="POST")
        if token:
            req.add_header("Authorization", "Bearer " + token)
        try:
            with urllib.request.urlopen(req, timeout=20) as resp:
                return resp.status, json.loads(resp.read())
        except urllib.error.HTTPError as err:
            return err.code, json.loads(err.read() or b"{}")

    def binary(self, exit_code=0, text="labd test 2"):
        return ("#!/bin/sh\necho '%s'\nexit %d\n" % (text, exit_code)).encode()

    def assert_target_unchanged(self):
        with open(self.target) as f:
            self.assertEqual(f.read(), "original\n")

    # tests (run in name order; test_z_* must be last because it ends the seed)

    def test_a_health(self):
        with urllib.request.urlopen("http://127.0.0.1:%d/healthz" % self.port) as resp:
            self.assertEqual(json.loads(resp.read())["stage"], "seed")

    def test_a_stalled_client_dropped(self):
        s = socket.create_connection(("127.0.0.1", self.port), timeout=10)
        s.sendall(b"POST /api/platform HTTP/1.1\r\nContent-Length: 1000\r\n")
        time.sleep(4)
        try:
            self.assertEqual(s.recv(100), b"", "the seed kept a stalled connection open")
        finally:
            s.close()

    def test_b_rejections(self):
        body = self.binary()
        good = hashlib.sha256(body).hexdigest()
        cases = {
            "no token": None,
            "wrong repository": self.token(good, repository="someone/lab"),
            "branch, not main": self.token(good, ref="refs/heads/idea"),
            "digest of another file": self.token(hashlib.sha256(b"other").hexdigest()),
            "plain audience": self.token(aud="lab-deploy"),
            "expired": self.token(good, exp=int(time.time()) - 600),
            "issued in the future": self.token(good, iat=int(time.time()) + 600),
            "wrong issuer": self.token(good, iss="https://example.com"),
            "no jti": self.token(good, jti=None),
            "signed by another key": self.token(good, key=self.other_key),
            "unknown kid": self.token(good, kid="nope"),
            "alg none": self.token(good, alg="none"),
        }
        for name, tok in cases.items():
            with self.subTest(name):
                status, resp = self.post(body, tok)
                self.assertIn(status, (401, 403), "%s -> %s %s" % (name, status, resp))
        tampered = self.token(good)
        head, payload, sig = tampered.split(".")
        forged = json.loads(base64.urlsafe_b64decode(payload + "=" * (-len(payload) % 4)))
        forged["ref"] = "refs/heads/main"
        forged["repository"] = REPO
        forged["sha"] = "evil"
        status, _ = self.post(body, head + "." + b64url(json.dumps(forged).encode()) + "." + sig)
        self.assertEqual(status, 403)
        status, _ = self.post(body, self.token(good), path="/api/deploy")
        self.assertEqual(status, 404)
        self.assert_target_unchanged()

    def test_c_broken_binary_and_replay(self):
        body = self.binary(exit_code=1)
        tok = self.token(hashlib.sha256(body).hexdigest())
        status, resp = self.post(body, tok)
        self.assertEqual(status, 400, resp)
        self.assert_target_unchanged()
        status, resp = self.post(body, tok)
        self.assertEqual(status, 403, resp)
        self.assertIn("already used", resp["error"])
        self.assertEqual([n for n in os.listdir(self.bindir) if n != "labd"], [])

    def test_z_install(self):
        body = self.binary()
        status, resp = self.post(body, self.token(hashlib.sha256(body).hexdigest()))
        self.assertEqual(status, 200, resp)
        self.assertEqual(resp["installed"], "labd test 2")
        with open(self.target, "rb") as f:
            self.assertEqual(f.read(), body)
        self.assertTrue(os.access(self.target, os.X_OK))
        self.assertEqual(self.proc.wait(timeout=5), 0)


if __name__ == "__main__":
    unittest.main(verbosity=2)
