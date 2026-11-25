#!/usr/bin/env python3
"""Fills a running local ONE with invented chats through the bridge's own
addresses, to look at them. Not shipped. Use as PREPARE for e2e/screenshot.sh:
  PREPARE=apps/one/demo/chats.py e2e/screenshot.sh one /items/1 /connectors
SILENT=1 makes the bridge look as if it had not been heard for an hour."""
import json, os, re, sqlite3, struct, sys, time, urllib.parse, urllib.request, zlib
from datetime import datetime, timedelta, timezone

base = sys.argv[1]


def call(method, path, token=None, body=None, raw=None, form=None):
    data = raw if raw is not None else (json.dumps(body).encode() if body is not None else (urllib.parse.urlencode(form).encode() if form else None))
    req = urllib.request.Request(base + path, data=data, method=method)
    if token:
        req.add_header("Authorization", "Bearer " + token)
    if body is not None:
        req.add_header("Content-Type", "application/json")
    if form is not None:
        req.add_header("X-Requested-With", "one")
    with urllib.request.urlopen(req) as r:
        b = r.read()
        return json.loads(b) if b[:1] == b"{" else b.decode()


page = call("GET", "/connectors")
bridge = re.search(r'id="bridge-token"[^>]*value="([^"]+)"', page).group(1)
agent = re.search(r'id="agent-token"[^>]*value="([^"]+)"', page).group(1)


def ago(minutes):
    return (datetime.now(timezone.utc) - timedelta(minutes=minutes)).strftime("%Y-%m-%dT%H:%M:%SZ")


def png(w=240, h=120):
    raw = b"".join(b"\x00" + b"".join(bytes((x * 255 // w, y * 255 // h, 160 if (x // 20 + y // 20) % 2 else 60)) for x in range(w)) for y in range(h))
    def chunk(t, d):
        c = struct.pack(">I", len(d)) + t + d
        return c + struct.pack(">I", zlib.crc32(t + d) & 0xFFFFFFFF)
    return b"\x89PNG\r\n\x1a\n" + chunk(b"IHDR", struct.pack(">IIBBBBB", w, h, 8, 2, 0, 0, 0)) + chunk(b"IDAT", zlib.compress(raw)) + chunk(b"IEND", b"")


me = {"handle": "me", "name": "the owner", "self": True}
chats = [
    {"network": "whatsapp", "chat_id": "cabin", "name": "Cabin Group", "type": "group", "muted": False,
     "participants": [me, {"handle": "+420700000001", "name": "Jana"}, {"handle": "+420700000002", "name": "Karel"}, {"handle": "+420700000003", "name": "Eva"}],
     "messages": [
         {"id": "m1", "sender": "+420700000001", "sender_name": "Jana", "at": ago(300), "text": "Kdo veze dřevo na sobotu?"},
         {"id": "m2", "sender": "+420700000002", "sender_name": "Karel", "at": ago(290), "text": "Já přivezu nosníky, ale potřebuju pomoct s vyložením."},
         {"id": "m3", "sender": "me", "sender_name": "the owner", "at": ago(280), "text": "Přijedu v deset.", "own": True, "read": True},
         {"id": "m4", "sender": "+420700000003", "sender_name": "Eva", "at": ago(270), "text": "Tady je plánek, kam co pojede.",
          "attachments": [{"id": "a1", "name": "plan.png", "type": "image/png", "size": len(png())}]}]},
    {"network": "messenger", "chat_id": "ondra", "name": "Ondra", "type": "direct", "muted": False,
     "participants": [me, {"handle": "ondra.k", "name": "Ondra"}],
     "messages": [
         {"id": "m1", "sender": "ondra.k", "sender_name": "Ondra", "at": ago(9), "text": "Jsi tam?"},
         {"id": "m2", "sender": "ondra.k", "sender_name": "Ondra", "at": ago(2), "text": "Potřebuju vědět, jestli zítra zvládneš ten převoz."}]},
    {"network": "whatsapp", "chat_id": "jana", "name": "Jana Nováková", "type": "direct", "muted": False,
     "participants": [me, {"handle": "+420700000001", "name": "Jana Nováková"}],
     "messages": [
         {"id": "m1", "sender": "+420700000001", "sender_name": "Jana Nováková", "at": ago(200), "text": "Ahoj, stihneš zítra odpoledne zavolat? Chtěla bych probrat termín schůzky s architektem."}]},
]
call("POST", "/api/bridge/alive", bridge, {"networks": [{"network": "whatsapp", "state": "connected"}, {"network": "messenger", "state": "connected"}], "version": "0.1"})
call("POST", "/api/bridge/batch", bridge, {"chats": chats})
call("PUT", "/api/bridge/files?network=whatsapp&chat_id=cabin&message_id=m4&attachment=a1", bridge, raw=png())

# Claude looks at what waits, then proposes a reply for Jana, and the owner approves it.
for i in call("GET", "/api/queue", agent)["items"]:
    call("POST", "/api/items/%s/seen" % i["id"], agent, {})
prop = call("POST", "/api/items/Q-0003/reply-proposals", agent, {"text": "Ahoj Jano, zavolám ti zítra ve tři. the owner", "reason": "She asks for a call tomorrow afternoon."})
call("POST", "/proposals/%d/approve" % prop["proposal_id"], form={"text": "Ahoj Jano, zavolám ti zítra ve tři. the owner", "open": "1"})

if os.environ.get("SILENT"):
    dd = None
    for pid in os.listdir("/proc"):
        if pid.isdigit():
            try:
                env = open("/proc/%s/environ" % pid, "rb").read().split(b"\0")
            except OSError:
                continue
            if any(e == ("BASE_URL=" + base).encode() for e in env):
                dd = [e[9:].decode() for e in env if e.startswith(b"DATA_DIR=")][0]
    db = sqlite3.connect(os.path.join(dd, "one.db"))
    db.execute("UPDATE settings SET value=? WHERE key='bridge.heard_at'", (str(int(time.time()) - 3600),))
    db.commit()
