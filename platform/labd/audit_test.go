package main

import (
	"context"
	"strings"
	"testing"
	"time"
)

const ssSample = `tcp   LISTEN 0      511          0.0.0.0:80        0.0.0.0:*    users:(("nginx",pid=612,fd=6),("nginx",pid=611,fd=6))
tcp   LISTEN 0      511          0.0.0.0:443       0.0.0.0:*    users:(("nginx",pid=612,fd=7))
tcp   LISTEN 0      4096       127.0.0.1:7070      0.0.0.0:*    users:(("labd",pid=700,fd=3))
tcp   LISTEN 0      4096       127.0.0.1:20000     0.0.0.0:*    users:(("app",pid=801,fd=3))
tcp   LISTEN 0      511             [::]:443          [::]:*    users:(("nginx",pid=612,fd=8))
udp   UNCONN 0      0         127.0.0.53%lo:53      0.0.0.0:*    users:(("systemd-resolve",pid=400,fd=13))
tcp   LISTEN 0      128          0.0.0.0:22        0.0.0.0:*    users:(("sshd",pid=900,fd=3))
tcp   LISTEN 0      128        127.0.0.1:6379      0.0.0.0:*    users:(("redis-server",pid=950,fd=6))
`

func TestParseListenersAndExpectations(t *testing.T) {
	ls := parseListeners(ssSample)
	if len(ls) != 8 {
		t.Fatalf("parsed %d listeners: %+v", len(ls), ls)
	}
	var unexpected []string
	for _, l := range ls {
		if !expectedListener(l, 7070) {
			unexpected = append(unexpected, l.Process)
		}
	}
	if strings.Join(unexpected, ",") != "sshd,redis-server" {
		t.Fatalf("unexpected listeners: %v", unexpected)
	}
	if ls[0].Port != 22 || ls[len(ls)-1].Port != 20000 {
		t.Fatalf("not sorted by port: %+v", ls)
	}
}

const nftSample = `table inet filter {
	chain input {
		type filter hook input priority filter; policy drop;
		iif "lo" accept
		ct state established,related accept
		tcp dport { 80, 443 } accept
		icmp type echo-request accept
	}
	chain forward {
		type filter hook forward priority filter; policy drop;
	}
}
table inet lab_apps {
	chain output {
		type filter hook output priority filter; policy accept;
	}
}
`

func TestAuditFirewall(t *testing.T) {
	fw := auditFirewall(nftSample)
	if !fw.Present || !fw.InputDrop || !fw.LabApps || fw.SSHAccepted || len(fw.RulesetSHA256) != 64 {
		t.Fatalf("%+v", fw)
	}
	open := strings.Replace(nftSample, "policy drop;\n\t\tiif", "policy accept;\n\t\tiif", 1)
	open = strings.Replace(open, "{ 80, 443 }", "{ 22, 80, 443 }", 1)
	open = strings.Replace(open, "table inet lab_apps", "table inet other", 1)
	fw = auditFirewall(open)
	if fw.InputDrop || !fw.SSHAccepted || fw.LabApps {
		t.Fatalf("open firewall not noticed: %+v", fw)
	}
	if auditFirewall(strings.Replace(nftSample, "{ 80, 443 }", "22", 1)).SSHAccepted == false {
		t.Fatal("plain 'dport 22 accept' not noticed")
	}
	if fw.Blocked == nil || len(fw.Blocked) != 0 {
		t.Fatalf("no blocked table must read as an empty list, got %#v", fw.Blocked)
	}
	blocked := nftSample + "table inet lab_blocked {\n\tchain input {\n\t\ttype filter hook input priority filter - 10; policy accept;\n" +
		"\t\tip saddr { 45.138.12.16, 198.51.100.9 } drop\n\t\tip6 saddr { 2001:db8::7 } drop\n\t}\n}\n"
	fw = auditFirewall(blocked)
	if strings.Join(fw.Blocked, " ") != "45.138.12.16 198.51.100.9 2001:db8::7" || !fw.InputDrop || fw.SSHAccepted {
		t.Fatalf("blocked addresses not read: %+v", fw)
	}
	// nft lists a set of one address without the braces, which is how the
	// table looks with a single blocked scanner.
	one := nftSample + "table inet lab_blocked {\n\tchain input {\n\t\ttype filter hook input priority filter - 10; policy accept;\n" +
		"\t\tip saddr 45.138.12.16 drop\n\t\tip6 saddr 2001:db8::7 drop\n\t}\n}\n"
	if fw = auditFirewall(one); strings.Join(fw.Blocked, " ") != "45.138.12.16 2001:db8::7" {
		t.Fatalf("single blocked addresses not read: %+v", fw)
	}
}

func TestAuditNginx(t *testing.T) {
	now := time.Date(2026, 11, 2, 12, 0, 0, 0, time.UTC)
	log := `203.0.113.7 - - [02/Nov/2026:11:30:00 +0000] "GET / HTTP/1.1" 200 512 "-" "Safari"
203.0.113.7 - - [02/Nov/2026:11:31:00 +0000] "POST /notes HTTP/1.1" 303 0 "-" "Safari"
198.51.100.9 - - [02/Nov/2026:11:40:00 +0000] "GET /wp-login.php HTTP/1.1" 404 0 "-" "scanner"
198.51.100.9 - - [02/Nov/2026:11:41:00 +0000] "GET /.env HTTP/1.1" 403 0 "-" "scanner"
198.51.100.9 - - [01/Nov/2026:11:41:00 +0000] "GET /old HTTP/1.1" 403 0 "-" "scanner"
203.0.113.7 - - [02/Nov/2026:11:50:00 +0000] "GET /x HTTP/1.1" 502 0 "-" "Safari"
garbage line
`
	a := auditNginx(strings.NewReader(log), now, 6*time.Hour)
	if !a.Parsed || a.Requests != 5 || a.Status["2xx"] != 1 || a.Status["3xx"] != 1 || a.Status["4xx"] != 2 || a.Status["5xx"] != 1 {
		t.Fatalf("%+v", a)
	}
	if len(a.TopErrIPs) == 0 || a.TopErrIPs[0].IP != "198.51.100.9" || a.TopErrIPs[0].Count != 2 {
		t.Fatalf("top error ips %+v", a.TopErrIPs)
	}
}

func TestParseUnitsAndMemory(t *testing.T) {
	us := parseUnits(`lab-app-hello--prod.service loaded active running lab app hello (prod)
lab-app-hello--dark-mode.service loaded failed failed lab app hello (dark-mode)
some-other.service loaded active running other
`)
	if len(us) != 2 || us[1].Active != "failed" || us[0].Unit != "lab-app-hello--prod.service" {
		t.Fatalf("%+v", us)
	}
	m := memoryStatus("MemTotal:        4000000 kB\nMemFree:          100000 kB\nMemAvailable:     200000 kB\n")
	if m["available_percent"] != 5 {
		t.Fatalf("%+v", m)
	}
}

func TestSignInFailuresWindow(t *testing.T) {
	signInFailures.Lock()
	signInFailures.list = nil
	signInFailures.Unlock()
	now := time.Now()
	recordSignInFailure("198.51.100.1", now.Add(-48*time.Hour))
	for i := 0; i < 12; i++ {
		recordSignInFailure("198.51.100.2", now.Add(-time.Hour))
	}
	if got := recentSignInFailures(now.Add(-24 * time.Hour)); len(got) != 12 {
		t.Fatalf("got %d recent failures", len(got))
	}
}

// hostAudit with a fake server: the findings reflect the commands' output.
func TestHostAuditFindings(t *testing.T) {
	env := &hostEnv{run: func(_ context.Context, _ time.Duration, name string, args ...string) (string, error) {
		switch name {
		case "ss":
			return ssSample, nil
		case "nft":
			return nftSample, nil
		case "systemctl":
			if args[0] == "list-units" {
				return "lab-app-hello--prod.service loaded failed failed x\n", nil
			}
			return "ActiveState=inactive\nResult=success\n", nil
		case "journalctl":
			return "", nil
		case "apt-get":
			return "Inst openssl [3.0.1] (3.0.2 Debian-Security:13/stable-security [amd64])\n", nil
		}
		return "", nil
	}}
	out := hostAudit(context.Background(), env, 7070, nil, time.Now())
	fs := out["findings"].(findings)
	codes := map[string]string{}
	for _, f := range fs {
		if cur, ok := codes[f.Code]; !ok || sevRank(f.Severity) < sevRank(cur) {
			codes[f.Code] = f.Severity
		}
	}
	if codes["unexpected-listener"] != "critical" || codes["unit-failed"] != "critical" || codes["security-updates"] != "warning" {
		t.Fatalf("findings: %+v", fs)
	}
	if out["worst"] != "critical" || fs[0].Severity != "critical" {
		t.Fatalf("worst %v, first %+v", out["worst"], fs[0])
	}
}
