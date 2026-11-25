#!/usr/bin/python3
"""Build the Hetzner "Cloud config" text for the test server (lab-runner).

The test server runs this repository's GitHub Actions jobs instead of
GitHub's machines (whose free minutes ran out). It is a separate server from
the lab server on purpose: jobs run code from every branch, and none of that
should run next to production data. See manager.py for how each job gets a
clean, separate user.

Nothing comes in: no SSH, no open port; the runner only connects out to
GitHub. The text in the repository has no secrets. Its last line is a
comment where the owner puts the GitHub token in his own copy at Hetzner (a
comment, so a stray space or a missing one cannot break the text); the
manager takes it from there. `lab-runner-token` in the console is the other
way.

Usage: python3 platform/runner/build.py [output path]   (default dist/runner-setup.yaml)
"""

import importlib.util
import json
import os
import sys

HERE = os.path.dirname(os.path.abspath(__file__))
ROOT = os.path.dirname(os.path.dirname(HERE))
# The lab server's generator: block() and validate() are shared.
_spec = importlib.util.spec_from_file_location("server_build", os.path.join(ROOT, "platform", "cloudinit", "build.py"))
server = importlib.util.module_from_spec(_spec)
_spec.loader.exec_module(server)

SLOTS = 2  # jobs at the same time; a 4 vCPU / 8 GB server fits two

SERVICE = """\
[Unit]
Description=GitHub Actions jobs for the lab repository (lab-runner)
Wants=network-online.target
After=network-online.target

[Service]
ExecStart=/usr/bin/python3 /usr/local/lib/lab-runner/manager.py /etc/lab-runner/config.json
Restart=always
RestartSec=5

[Install]
WantedBy=multi-user.target
"""

NFTABLES = """\
#!/usr/sbin/nft -f
# Nothing comes in: the runner only connects out to GitHub. No SSH: the
# Hetzner web console is the way in.
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
    }
    chain forward {
        type filter hook forward priority filter; policy drop;
    }
    chain output {
        type filter hook output priority filter; policy accept;
        # Jobs have no business with the cloud's metadata service.
        meta skuid >= 2000 ip daddr 169.254.169.254 drop
    }
}
"""

SYSCTL = """\
# Firefox and WebKit sandbox themselves with user namespaces, as on GitHub's
# Ubuntu 24.04 machines.
kernel.apparmor_restrict_unprivileged_userns = 0
"""

APT_UPGRADES = """\
APT::Periodic::Update-Package-Lists "1";
APT::Periodic::Unattended-Upgrade "1";
Unattended-Upgrade::Automatic-Reboot "true";
Unattended-Upgrade::Automatic-Reboot-Time "04:30";
"""

# needrestart restarts services after updates; the manager is safe to restart
# (running jobs carry on), but there is no reason to do it mid-day.
NEEDRESTART = """\
$nrconf{override_rc}{qr(^lab-runner)} = 0;
"""

MOTD = """\
lab-runner: runs GitHub Actions jobs of the lab repository.
Set or replace its GitHub token with: lab-runner-token
Status: systemctl status lab-runner   Log: journalctl -u lab-runner -f
"""

# What the jobs use besides what actions download themselves: git and jq for
# the scripts, a C compiler and static C library for the app builds (SQLite),
# shellcheck and PyYAML for the checks. The browsers' system libraries come
# from install-browser-libs; actions fetch Go and Node for the jobs.
PACKAGES = [
    "build-essential", "ca-certificates", "curl", "git", "jq", "nftables", "xz-utils",
    "python3", "python3-yaml", "shellcheck", "sqlite3", "unattended-upgrades", "unzip", "zip",
]

# Keep in step with PLAYWRIGHT_VERSION in .github/workflows/pipeline.yml.
PLAYWRIGHT_VERSION = "1.56.0"


def build():
    with open(os.path.join(ROOT, "lab.json")) as f:
        lab = json.load(f)
    with open(os.path.join(HERE, "manager.py")) as f:
        manager = f.read()
    with open(os.path.join(HERE, "lab-runner-token")) as f:
        token_cmd = f.read()
    with open(os.path.join(HERE, "install-browser-libs")) as f:
        libs_cmd = f.read()
    config = json.dumps({"repo": lab["repo"], "slots": SLOTS, "name": "lab-runner"}, indent=2) + "\n"
    files = [
        ("/etc/lab-runner/config.json", "0644", config),
        ("/usr/local/lib/lab-runner/manager.py", "0755", manager),
        ("/usr/local/sbin/lab-runner-token", "0755", token_cmd),
        ("/usr/local/sbin/install-browser-libs", "0755", libs_cmd),
        ("/etc/systemd/system/lab-runner.service", "0644", SERVICE),
        ("/etc/lab-runner/nftables.conf", "0644", NFTABLES),
        ("/etc/sysctl.d/60-lab-runner.conf", "0644", SYSCTL),
        ("/etc/apt/apt.conf.d/52lab-upgrades", "0644", APT_UPGRADES),
        ("/etc/needrestart/conf.d/lab-runner.conf", "0644", NEEDRESTART),
        ("/etc/motd", "0644", MOTD),
    ]
    out = [
        "#cloud-config",
        "# lab test server (lab-runner) setup for Hetzner's Cloud config box.",
        "#",
        "#",
        "# >>> The GitHub token goes on the very LAST line of this text. <<<",
        "#",
        "# The token is only in your copy at Hetzner, never in the repository. On the",
        "# server it is readable by root alone; jobs are blocked from the place Hetzner",
        "# serves this text from. Without it, run lab-runner-token in the console.",
        "hostname: lab-runner",
        "timezone: Europe/Prague",
        "ssh_pwauth: false",
        "package_update: true",
        "package_upgrade: true",
        "packages:",
    ] + ["  - " + p for p in PACKAGES] + [
        "write_files:",
    ]
    for path, mode, content in files:
        out += ["  - path: " + path, "    permissions: '%s'" % mode, "    content: |", server.block(content)]
    out += ["runcmd:"]
    out += [
        "  - [cp, /etc/lab-runner/nftables.conf, /etc/nftables.conf]",
        "  - [systemctl, enable, --now, nftables.service]",
        "  - [nft, -f, /etc/nftables.conf]",
        "  - [systemctl, disable, --now, ssh.service, ssh.socket]",
        "  - [sysctl, --system]",
        # Jobs cannot see each other's processes.
        "  - [sh, -c, 'echo \"proc /proc proc defaults,hidepid=invisible 0 0\" >>/etc/fstab && mount -o remount,hidepid=invisible /proc']",
    ]
    for n in range(1, SLOTS + 1):
        out.append("  - [useradd, --uid, '%d', --create-home, --shell, /bin/bash, labr%d]" % (2000 + n, n))
        out.append("  - [chmod, '0700', /home/labr%d]" % n)
    out += [
        "  - [mkdir, -p, /var/lib/lab-runner]",
        # The browsers' system libraries; the browsers themselves are fetched by each job.
        "  - [/usr/local/sbin/install-browser-libs, '%s']" % PLAYWRIGHT_VERSION,
        # The manager installs them again by itself when the pipeline's version changes.
        "  - [sh, -c, 'echo %s >/var/lib/lab-runner/browser-libs-version']" % PLAYWRIGHT_VERSION,
        "  - [systemctl, daemon-reload]",
        "  - [systemctl, enable, --now, lab-runner.service]",
    ]
    out += [
        "#",
        "# Replace the word after 'token:' on the next line with your GitHub token",
        "# (github_pat_..., only %s, Administration: Read and write," % lab["repo"],
        "# Contents: Read-only). Spaces around it do not matter.",
        "# token: PASTE_TOKEN_HERE",
    ]
    text = "\n".join(out) + "\n"
    return text, files


def main():
    dest = sys.argv[1] if len(sys.argv) > 1 else os.path.join(ROOT, "dist", "runner-setup.yaml")
    text, files = build()
    size = server.validate(text, files)
    os.makedirs(os.path.dirname(dest), exist_ok=True)
    with open(dest, "w") as f:
        f.write(text)
    print("%s (%d bytes of %d)" % (dest, size, server.LIMIT))


if __name__ == "__main__":
    main()
