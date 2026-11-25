#!/usr/bin/python3
"""Build the Hetzner "Cloud config" text for a fresh lab server.

The text has no secrets and no domain in it: it hardens the box, installs
nginx, certbot and sqlite3, and starts the labd seed, which waits for the real
labd from this repo's pipeline (see platform/seed/seed.py).

Usage: python3 platform/cloudinit/build.py [output path]   (default dist/server-setup.yaml)
"""

import json
import os
import sys

ROOT = os.path.dirname(os.path.dirname(os.path.dirname(os.path.abspath(__file__))))
LIMIT = 32 * 1024  # Hetzner's user data limit

LABD_SERVICE = """\
[Unit]
Description=lab server program (labd)
Wants=network-online.target
After=network-online.target nginx.service

[Service]
ExecStart=/usr/local/bin/labd -config /etc/lab/labd.json
Restart=always
RestartSec=2

[Install]
WantedBy=multi-user.target
"""

NGINX_SITE = """\
# Bootstrap site from the server setup text. labd rewrites this once it runs.
server {
    listen 80 default_server;
    listen [::]:80 default_server;
    server_name _;
    server_tokens off;
    client_max_body_size 200m;

    location ^~ /.well-known/acme-challenge/ {
        root /var/www/acme;
        default_type text/plain;
    }
    location = /healthz {
        proxy_pass http://127.0.0.1:7070;
    }
    location ^~ /api/ {
        proxy_pass http://127.0.0.1:7070;
        proxy_set_header Host $host;
        proxy_set_header X-Real-IP $remote_addr;
        proxy_read_timeout 300s;
        proxy_send_timeout 300s;
    }
    location / {
        return 404;
    }
}
"""

NFTABLES = """\
#!/usr/sbin/nft -f
# Only web traffic comes in. No SSH: the Hetzner web console is the emergency door.
flush ruleset
table inet filter {
    chain input {
        type filter hook input priority filter; policy drop;
        ct state established,related accept
        ct state invalid drop
        iif "lo" accept
        meta l4proto icmp accept
        meta l4proto ipv6-icmp accept
        udp sport 67 udp dport 68 accept
        tcp dport { 80, 443 } accept
    }
    chain forward {
        type filter hook forward priority filter; policy drop;
    }
    chain output {
        type filter hook output priority filter; policy accept;
    }
}
"""

APT_UPGRADES = """\
APT::Periodic::Update-Package-Lists "1";
APT::Periodic::Unattended-Upgrade "1";
Unattended-Upgrade::Automatic-Reboot "true";
Unattended-Upgrade::Automatic-Reboot-Time "04:30";
"""

# The same text labd writes (platform/labd/host.go, journaldConf): logs are
# kept 30 days for the lab's Errors page and the daily review.
JOURNALD = """\
# Managed by labd. Keep the apps' logs 30 days (CLAUDE.md, "Logging").
[Journal]
Storage=persistent
MaxRetentionSec=30day
SystemMaxUse=2G
SystemKeepFree=5G
Compress=yes
"""

MOTD = """\
This server is managed by labd from the {repo} repository.
Changes made by hand here will be overwritten. See CLAUDE.md in the repo.
"""


def block(text, indent=6):
    pad = " " * indent
    return "\n".join(pad + line if line else "" for line in text.rstrip("\n").split("\n"))


def build():
    with open(os.path.join(ROOT, "lab.json")) as f:
        lab = json.load(f)
    with open(os.path.join(ROOT, "platform", "seed", "seed.py")) as f:
        seed = f.read()
    labd_json = json.dumps({"repo": lab["repo"], "listen": "127.0.0.1:7070"}, indent=2) + "\n"

    files = [
        ("/etc/lab/labd.json", "0644", labd_json),
        ("/usr/local/bin/labd", "0755", seed),
        ("/etc/systemd/system/labd.service", "0644", LABD_SERVICE),
        ("/etc/nginx/sites-available/lab", "0644", NGINX_SITE),
        ("/etc/lab/nftables.conf", "0644", NFTABLES),
        ("/etc/apt/apt.conf.d/52lab-upgrades", "0644", APT_UPGRADES),
        ("/etc/systemd/journald.conf.d/50-lab.conf", "0644", JOURNALD),
        ("/etc/motd", "0644", MOTD.format(repo=lab["repo"])),
    ]
    out = [
        "#cloud-config",
        "# lab server setup. No secrets inside; safe to paste into Hetzner's Cloud config box.",
        "hostname: lab",
        "timezone: Europe/Prague",
        "ssh_pwauth: false",
        "package_update: true",
        "package_upgrade: true",
        "packages:",
        "  - ca-certificates",
        "  - certbot",
        "  - nftables",
        "  - nginx",
        "  - python3",
        "  - sqlite3",
        "  - unattended-upgrades",
        "write_files:",
    ]
    for path, mode, content in files:
        out += ["  - path: " + path, "    permissions: '%s'" % mode, "    content: |", block(content)]
    out += [
        "runcmd:",
        "  - [mkdir, -p, /var/www/acme, /srv/lab]",
        "  - [cp, /etc/lab/nftables.conf, /etc/nftables.conf]",
        "  - [systemctl, enable, --now, nftables.service]",
        "  - [nft, -f, /etc/nftables.conf]",
        "  - [rm, -f, /etc/nginx/sites-enabled/default]",
        "  - [ln, -sf, /etc/nginx/sites-available/lab, /etc/nginx/sites-enabled/lab]",
        "  - [systemctl, daemon-reload]",
        "  - [systemctl, enable, --now, labd.service]",
        "  - [systemctl, restart, nginx.service]",
    ]
    text = "\n".join(out) + "\n"
    return text, files


def validate(text, files):
    size = len(text.encode())
    if size > LIMIT:
        raise SystemExit("server setup is %d bytes, over Hetzner's %d byte limit" % (size, LIMIT))
    try:
        import yaml
    except ImportError:
        print("warning: PyYAML missing, YAML structure not checked", file=sys.stderr)
        return size
    doc = yaml.safe_load(text)
    if not text.startswith("#cloud-config\n"):
        raise SystemExit("must start with #cloud-config")
    got = {f["path"]: (f["permissions"], f["content"]) for f in doc["write_files"]}
    for path, mode, content in files:
        if got.get(path) != (mode, content):
            raise SystemExit("embedded file differs after YAML round trip: " + path)
    if not all(isinstance(cmd, list) for cmd in doc["runcmd"]):
        raise SystemExit("runcmd entries must be lists")
    return size


def main():
    dest = sys.argv[1] if len(sys.argv) > 1 else os.path.join(ROOT, "dist", "server-setup.yaml")
    text, files = build()
    size = validate(text, files)
    os.makedirs(os.path.dirname(dest), exist_ok=True)
    with open(dest, "w") as f:
        f.write(text)
    print("%s (%d bytes of %d)" % (dest, size, LIMIT))


if __name__ == "__main__":
    main()
