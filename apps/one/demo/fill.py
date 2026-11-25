#!/usr/bin/env python3
"""Fills a local ONE data folder with invented demo items, to look at the list.
Not shipped: the build only takes the Go program. Usage: fill.py <data-dir> [count]"""
import hashlib, os, sqlite3, struct, sys, time, zlib, json

data = sys.argv[1]
extra = int(sys.argv[2]) if len(sys.argv) > 2 else 0
db = sqlite3.connect(os.path.join(data, "one.db"))
now = int(time.time())
DAY = 86400
n = [0]


def png(w=240, h=120):
    raw = b"".join(b"\x00" + b"".join(bytes((x * 255 // w, y * 255 // h, 160 if (x // 20 + y // 20) % 2 else 60)) for x in range(w)) for y in range(h))
    def chunk(t, d):
        c = struct.pack(">I", len(d)) + t + d
        return c + struct.pack(">I", zlib.crc32(t + d) & 0xFFFFFFFF)
    return b"\x89PNG\r\n\x1a\n" + chunk(b"IHDR", struct.pack(">IIBBBBB", w, h, 8, 2, 0, 0, 0)) + chunk(b"IDAT", zlib.compress(raw)) + chunk(b"IEND", b"")


def pool(content):
    sha = hashlib.sha256(content).hexdigest()
    d = os.path.join(data, "files", sha[:2])
    os.makedirs(d, exist_ok=True)
    with open(os.path.join(d, sha), "wb") as f:
        f.write(content)
    return sha, sha[:2] + "/" + sha


def item(stage, subject, msgs, pending_ago=None, number=None):
    n[0] += 1
    num = n[0]
    last = max(m["at"] for m in msgs)
    first = min(m["at"] for m in msgs)
    cur = db.execute("INSERT INTO items (number, kind, subject, stage, needs_claude, pending_since, first_message_at, last_message_at, moved_at) VALUES (?,?,?,?,?,?,?,?,?)",
                     (num, "thread", subject, stage, 0, (now - pending_ago) if pending_ago else None, first, last, last))
    iid = cur.lastrowid
    db.execute("INSERT INTO item_links (item_id, kind, value, method, linked_by, linked_at) VALUES (?,?,?,?,?,?)", (iid, "gmail_thread", "demo%d" % num, "auto", "system", now))
    db.execute("INSERT INTO events (at, item_id, actor, type, data) VALUES (?,?,?,?,?)", (first, iid, "system", "ingested", "{}"))
    for i, m in enumerate(msgs):
        sid = "demo-%d-%d" % (num, i)
        rc = db.execute("INSERT INTO raw (source, source_id, version_key, thread_key, received_at, ingested_at, blob_sha256, payload) VALUES (?,?,?,?,?,?,?,?)",
                        ("gmail", sid, "a@0", "gmail:demo%d" % num, m["at"], m["at"], "ab", "{}"))
        mc = db.execute("""INSERT INTO messages (item_id, raw_id, source, source_id, thread_key, network, sender, sender_name, subject, to_addrs, cc_addrs, addressed,
            has_attachments, text, sent_by_me, received_at, labels, flags, seen, starred, header_id) VALUES (?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)""",
                        (iid, rc.lastrowid, "gmail", sid, "gmail:demo%d" % num, "mail", m["from"], m["name"], m.get("subject", subject), "", "", 1,
                         1 if m.get("files") else 0, m["text"], 1 if m.get("mine") else 0, m["at"], "[]", "", 1, 0, ""))
        for j, f in enumerate(m.get("files", [])):
            sha, rel = pool(f[3])
            db.execute("INSERT INTO files (item_id, source, message_id, part, filename, content_type, disposition, content_id, size, sha256, path, status, added_at) VALUES (?,?,?,?,?,?,?,?,?,?,?,?,?)",
                       (iid, "gmail", mc.lastrowid, "p%d" % j, f[0], f[1], f[2], "", len(f[3]), sha, rel, "saved", now))
    return iid


def M(who, addr, ago, text, **kw):
    d = {"name": who, "from": addr, "at": now - int(ago * DAY), "text": text}
    d.update(kw)
    return d

people = [("Alice Novak", "alice@example.test"), ("Bob Dvorak", "bob@example.test"), ("Carla Svoboda", "carla@example.test"),
          ("Dobrá Pošta", "post@example.test"), ("Eva Horáková", "eva@example.test"), ("Radek Kolář", "radek@example.test")]
long_text = "\n\n".join("Paragraph %d. This is a long message to show how the text is folded and opened on request. It goes on for a while so that it does not fit." % i for i in range(1, 26))
subjects = ["Offer for the kitchen worktop", "Parcel 4432 is on its way", "Invoice 2026-14", "Re: Meeting on Friday", "Question about the permit",
            "Your order has shipped", "Insurance renewal", "Tickets for Saturday", "School trip form", "Dinner on Sunday?"]

item("new", "Offer for the kitchen worktop", [
    M("Alice Novak", "alice@example.test", 70, "First contact about the worktop, from a long time ago."),
    M("Alice Novak", "alice@example.test", 40, "Following up with the measurements."),
    M("Owner", "owner@example.test", 9, "Thanks, I will look at it.", mine=True),
    M("Alice Novak", "alice@example.test", 0.2, long_text, files=[("plan.png", "image/png", "inline", png()), ("tall.png", "image/png", "inline", png(400, 1400)), ("wide.png", "image/png", "inline", png(1600, 300)), ("quote.pdf", "application/pdf", "attachment", b"%PDF-1.4 demo quote")]),
])
item("new", "<script>alert(1)</script> Special offer", [M("<b>Mallory</b>", "mallory@example.test", 0.3, "<script>alert('x')</script> Click <a href=\"https://example.test\">here</a>")])
item("new", "Parcel 4432 is on its way", [M("Dobrá Pošta", "post@example.test", 0.1, "Your parcel will arrive tomorrow between 9 and 12.")])
item("in_progress", "Question about the permit", [M("Eva Horáková", "eva@example.test", 1.5, "Could you send the drawings of the extension? We need them by Thursday."),
                                                    M("Eva Horáková", "eva@example.test", 0.6, "Also the ownership sheet, please.")])
item("pending", "Invoice 2026-14", [M("Owner", "owner@example.test", 3.2, "Sent the invoice, please confirm.", mine=True)], pending_ago=int(3.2 * DAY))
item("pending", "Tickets for Saturday", [M("Radek Kolář", "radek@example.test", 0.4, "I will check with the others and tell you.")], pending_ago=int(0.3 * DAY))
item("resolved", "Insurance renewal", [M("Carla Svoboda", "carla@example.test", 5, "Renewed. Thank you.")])
item("closed", "Old thing", [M("Bob Dvorak", "bob@example.test", 50, "Closed long ago.")])
item("history", "A year ago", [M("Bob Dvorak", "bob@example.test", 300, "From a year ago.")])
for i in range(extra):
    who = people[i % len(people)]
    item("new" if i % 3 else "in_progress", "%s #%d" % (subjects[i % len(subjects)], i), [M(who[0], who[1], 0.35 + i * 0.002, "Demo message number %d about %s." % (i, subjects[i % len(subjects)]))])
db.execute("INSERT INTO events (at, item_id, actor, type, data) VALUES (?,1,'owner','noted',?)", (now - 600, json.dumps({"text": "Ask Claude to compare this with the other offer."})))
db.commit()
print("filled", n[0], "items")
