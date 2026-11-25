#!/usr/bin/env python3
"""Adds invented calendar entries and to-dos to a local ONE data folder (after fill.py),
to look at the agenda. Not shipped. Usage: agenda.py <data-dir>"""
import json, os, sqlite3, sys, time
from datetime import datetime, timedelta, timezone
from zoneinfo import ZoneInfo

PR = ZoneInfo("Europe/Prague")
db = sqlite3.connect(os.path.join(sys.argv[1], "one.db"))
now = int(time.time())
today = datetime.now(PR).replace(hour=0, minute=0, second=0, microsecond=0)

pid = db.execute("INSERT INTO people (name, updated_by, updated_at) VALUES ('Klara','owner',?)", (now,)).lastrowid
cals = [
    {"key": "icloud:aaaaaaaaaaaa", "href": "x", "name": "Home", "show": True},
    {"key": "icloud:bbbbbbbbbbbb", "href": "y", "name": "Shared Calendar", "show": True, "person": pid},
]
db.execute("INSERT INTO settings (key, value, updated_by, updated_at) VALUES ('icloud_calendars', ?, 'system', ?)", (json.dumps(cals), now))

n = [0]


def event(src, calkey, calname, day, hm, mins, title, place="", status="confirmed", allday=0, days=1, uid=None):
    n[0] += 1
    d = today + timedelta(days=day)
    if allday:
        s = d
        e = d + timedelta(days=days)
        sd, ed = s.strftime("%Y-%m-%d"), e.strftime("%Y-%m-%d")
    else:
        s = d.replace(hour=hm[0], minute=hm[1])
        e = s + timedelta(minutes=mins)
        sd = ed = ""
    db.execute("""INSERT INTO agenda (kind, source, source_id, cal_key, calendar, uid, title, place, start_at, end_at, all_day, start_date, end_date, status, state, version, hash, created_at, updated_at)
        VALUES ('event',?,?,?,?,?,?,?,?,?,?,?,?,?, 'open',1,'h',?,?)""",
               (src, "d%d" % n[0], calkey, calname, uid or "u%d" % n[0], title, place, int(s.timestamp()), int(e.timestamp()), allday, sd, ed, status, now, now))


OUT = ("outlookcal", "outlookcal:main", "Outlook · Office")
HOME = ("icloud", "icloud:aaaaaaaaaaaa", "iCloud · Home")
PET = ("icloud", "icloud:bbbbbbbbbbbb", "iCloud · Shared Calendar")
event(*OUT, -12, (10, 0), 60, "Supplier call")
event(*OUT, -3, (14, 0), 90, "Workshop with the design team")
event(*HOME, -1, (18, 30), 60, "Dentist", "Main Street 12")
event(*OUT, 0, (9, 0), 30, "Standup")
event(*OUT, 0, (11, 0), 60, "Budget review with the finance team and the board", "Room 2.14, Office office")
event(*PET, 0, (16, 0), 120, "Dog training", "Park by the river")
# One entry that is under way right now (it began 20 minutes ago), to see the Now line and the running mark.
start = datetime.now(PR) - timedelta(minutes=20)
if start.date() == today.date():
    event(*OUT, 0, (start.hour, start.minute), 60, "Call with the supplier", "Meeting room 1")
event(*OUT, 1, (0, 0), 0, "Company day", allday=1, days=2)
event(*HOME, 1, (19, 0), 150, "Dinner with the neighbours", "Our place")
event(*OUT, 1, (9, 0), 60, "Planning", status="cancelled")
event(*PET, 3, (10, 0), 60, "Vet check-up")
event(*HOME, 3, (10, 0), 60, "Vet check-up", uid="u%d" % (n[0]))
event(*OUT, 6, (13, 0), 45, "<script>alert(1)</script> Offsite")
event(*HOME, 9, (0, 0), 0, "Holiday in the mountains", allday=1, days=5)
event(*OUT, 20, (15, 0), 60, "Quarterly review")
db.execute("UPDATE agenda SET uid = (SELECT uid FROM agenda WHERE title = 'Vet check-up' ORDER BY id LIMIT 1) WHERE title = 'Vet check-up'")

for i, (text, item, ago) in enumerate([("Call the bank about the card", 0, 3 * 86400), ("Reply to Alice about the worktop", 1, 86400), ("Buy stamps", 0, 3600)]):
    db.execute("INSERT INTO agenda (kind, title, state, status, created_by, item_id, created_at, updated_at) VALUES ('todo',?, 'open','confirmed','owner',?,?,?)",
               (text, item or None, now - ago, now - ago))
db.commit()
print("agenda filled")
