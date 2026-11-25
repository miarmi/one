package main

import (
	"bufio"
	"context"
	"crypto/sha256"
	"crypto/x509"
	"encoding/hex"
	"encoding/pem"
	"io"
	"os"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
)

// GET /api/host/audit: a security check of the server, for the security
// workflow (CONTRACT.md, "The server"). Everything here is read-only; the
// workflow and the owner decide what to do with the findings. Each check adds
// findings with a severity: "critical" (someone should look now), "warning"
// (fix soon, often automatically) or "info".

type finding struct {
	Severity string `json:"severity"`
	Code     string `json:"code"`
	Text     string `json:"text"`
}

type findings []finding

func (f *findings) add(severity, code, text string) {
	*f = append(*f, finding{severity, code, text})
}

// --- failed sign-ins, remembered by labd itself ---

type signInFailure struct {
	At time.Time `json:"at"`
	IP string    `json:"ip"`
}

var signInFailures struct {
	sync.Mutex
	list []signInFailure
}

// recordSignInFailure keeps the last 200 wrong passwords (lost on restart,
// which is fine: the audit runs every few hours).
func recordSignInFailure(ip string, at time.Time) {
	signInFailures.Lock()
	defer signInFailures.Unlock()
	signInFailures.list = append(signInFailures.list, signInFailure{At: at, IP: ip})
	if n := len(signInFailures.list); n > 200 {
		signInFailures.list = signInFailures.list[n-200:]
	}
}

func recentSignInFailures(since time.Time) []signInFailure {
	signInFailures.Lock()
	defer signInFailures.Unlock()
	var out []signInFailure
	for _, f := range signInFailures.list {
		if !f.At.Before(since) {
			out = append(out, f)
		}
	}
	if out == nil {
		out = []signInFailure{}
	}
	return out
}

// --- listening sockets ---

type listener struct {
	Proto   string `json:"proto"`
	Address string `json:"address"`
	Port    int    `json:"port"`
	Process string `json:"process"`
}

var ssUsersRe = regexp.MustCompile(`users:\(\("([^"]+)"`)

// parseListeners reads `ss -Hltnup` (TCP and UDP listeners with processes).
func parseListeners(out string) []listener {
	var ls []listener
	for _, line := range strings.Split(out, "\n") {
		f := strings.Fields(line)
		if len(f) < 5 {
			continue
		}
		local := f[4]
		i := strings.LastIndex(local, ":")
		if i < 0 {
			continue
		}
		port, err := strconv.Atoi(local[i+1:])
		if err != nil {
			continue
		}
		addr := strings.Trim(local[:i], "[]")
		if addr == "*" || addr == "0.0.0.0" || addr == "::" {
			addr = "any"
		}
		if strings.HasPrefix(addr, "%") || addr == "" {
			addr = "any"
		}
		l := listener{Proto: f[0], Address: addr, Port: port}
		if m := ssUsersRe.FindStringSubmatch(line); m != nil {
			l.Process = m[1]
		}
		ls = append(ls, l)
	}
	sort.Slice(ls, func(a, b int) bool {
		if ls[a].Port != ls[b].Port {
			return ls[a].Port < ls[b].Port
		}
		return ls[a].Address < ls[b].Address
	})
	return ls
}

// expectedListener says whether a listener is one the lab runs on purpose.
func expectedListener(l listener, labdPort int) bool {
	local := l.Address == "127.0.0.1" || l.Address == "::1" || strings.HasPrefix(l.Address, "127.")
	switch {
	case l.Proto == "tcp" && (l.Port == 80 || l.Port == 443) && l.Process == "nginx":
		return true
	case l.Proto == "tcp" && local && l.Port == labdPort && l.Process == "labd":
		return true
	case l.Proto == "tcp" && local && l.Port >= portMin && l.Port <= portMax && l.Process == "app":
		return true
	case l.Proto == "udp" && local && l.Port == 53: // systemd-resolved
		return true
	case l.Proto == "udp" && l.Port == 68 || l.Proto == "udp" && l.Port == 546: // DHCP client
		return true
	case l.Proto == "udp" && l.Port == 323 && local: // chrony
		return true
	}
	return false
}

// --- firewall ---

type firewallAudit struct {
	Present       bool     `json:"present"`         // the system rules exist
	LabApps       bool     `json:"lab_apps"`        // labd's table for the apps exists
	InputDrop     bool     `json:"input_drop"`      // the input chain drops by default
	SSHAccepted   bool     `json:"ssh_accepted"`    // port 22 is let in
	Blocked       []string `json:"blocked"`         // addresses the lab_blocked table drops (firewall.go)
	RulesetSHA256 string   `json:"ruleset_sha256"`  // to spot changes between audits
	Error         string   `json:"error,omitempty"` // when nft could not be read
}

var (
	nftInputChainRe = regexp.MustCompile(`(?s)chain input \{.*?\n\t\}`)
	nftPort22Re     = regexp.MustCompile(`dport (?:22|\{[^}]*\b22\b[^}]*\}) accept`)
	nftBlockedRe    = regexp.MustCompile(`(?s)table inet lab_blocked \{.*?\n\}`)
	// nft prints a set of one address without the braces (`ip saddr 1.2.3.4 drop`).
	nftSaddrSetRe = regexp.MustCompile(`saddr (?:\{ ([^}]*) \}|(\S+)) drop`)
)

// auditFirewall reads `nft list ruleset`.
func auditFirewall(ruleset string) firewallAudit {
	a := firewallAudit{}
	sum := sha256.Sum256([]byte(ruleset))
	a.RulesetSHA256 = hex.EncodeToString(sum[:])
	a.Present = strings.Contains(ruleset, "table inet filter") || strings.Contains(ruleset, "table inet lab")
	a.LabApps = strings.Contains(ruleset, "table inet lab_apps")
	for _, chain := range nftInputChainRe.FindAllString(ruleset, -1) {
		if strings.Contains(chain, "type filter hook input") && strings.Contains(chain, "policy drop") {
			a.InputDrop = true
		}
		if nftPort22Re.MatchString(chain) {
			a.SSHAccepted = true
		}
	}
	a.Blocked = []string{}
	if t := nftBlockedRe.FindString(ruleset); t != "" {
		for _, m := range nftSaddrSetRe.FindAllStringSubmatch(t, -1) {
			for _, ip := range strings.Split(m[1]+m[2], ",") {
				if ip = strings.TrimSpace(ip); ip != "" {
					a.Blocked = append(a.Blocked, ip)
				}
			}
		}
	}
	return a
}

// --- certificates ---

type certAudit struct {
	Host     string `json:"host"`
	NotAfter string `json:"not_after,omitempty"`
	DaysLeft int    `json:"days_left"`
	Error    string `json:"error,omitempty"`
}

func auditCert(host, path string, now time.Time) certAudit {
	c := certAudit{Host: host}
	b, err := os.ReadFile(path)
	if err != nil {
		c.Error = "unreadable"
		return c
	}
	block, _ := pem.Decode(b)
	if block == nil {
		c.Error = "not PEM"
		return c
	}
	cert, err := x509.ParseCertificate(block.Bytes)
	if err != nil {
		c.Error = "not a certificate"
		return c
	}
	c.NotAfter = cert.NotAfter.UTC().Format(time.RFC3339)
	c.DaysLeft = int(cert.NotAfter.Sub(now).Hours() / 24)
	return c
}

// --- nginx access log ---

type nginxAudit struct {
	Requests  int            `json:"requests"`
	Status    map[string]int `json:"status"` // "2xx", "3xx", "4xx", "5xx"
	TopIPs    []ipCount      `json:"top_ips"`
	TopErrIPs []ipCount      `json:"top_error_ips"` // most 4xx/5xx
	Parsed    bool           `json:"parsed"`
}

type ipCount struct {
	IP    string `json:"ip"`
	Count int    `json:"count"`
}

var nginxLineRe = regexp.MustCompile(`^(\S+) \S+ \S+ \[([^\]]+)\] "[^"]*" (\d{3}) `)

// auditNginx reads the tail of the access log (combined format) and counts
// the requests of the last `window`.
func auditNginx(r io.Reader, now time.Time, window time.Duration) nginxAudit {
	a := nginxAudit{Status: map[string]int{"2xx": 0, "3xx": 0, "4xx": 0, "5xx": 0}}
	ips := map[string]int{}
	errs := map[string]int{}
	since := now.Add(-window)
	sc := bufio.NewScanner(r)
	sc.Buffer(make([]byte, 0, 64<<10), 1<<20)
	for sc.Scan() {
		m := nginxLineRe.FindStringSubmatch(sc.Text())
		if m == nil {
			continue
		}
		a.Parsed = true
		t, err := time.Parse("02/Jan/2006:15:04:05 -0700", m[2])
		if err != nil || t.Before(since) {
			continue
		}
		a.Requests++
		class := m[3][:1] + "xx"
		a.Status[class]++
		ips[m[1]]++
		if class == "4xx" || class == "5xx" {
			errs[m[1]]++
		}
	}
	a.TopIPs = topCounts(ips, 10)
	a.TopErrIPs = topCounts(errs, 10)
	return a
}

func topCounts(m map[string]int, n int) []ipCount {
	out := make([]ipCount, 0, len(m))
	for ip, c := range m {
		out = append(out, ipCount{ip, c})
	}
	sort.Slice(out, func(a, b int) bool {
		if out[a].Count != out[b].Count {
			return out[a].Count > out[b].Count
		}
		return out[a].IP < out[b].IP
	})
	if len(out) > n {
		out = out[:n]
	}
	return out
}

// tailBytes reads at most n bytes from the end of a file.
func tailBytes(path string, n int64) io.Reader {
	f, err := os.Open(path)
	if err != nil {
		return strings.NewReader("")
	}
	defer f.Close()
	info, err := f.Stat()
	if err != nil {
		return strings.NewReader("")
	}
	start := info.Size() - n
	if start < 0 {
		start = 0
	}
	b := make([]byte, info.Size()-start)
	if _, err := f.ReadAt(b, start); err != nil && err != io.EOF {
		return strings.NewReader("")
	}
	if start > 0 {
		if i := strings.IndexByte(string(b), '\n'); i >= 0 {
			b = b[i+1:]
		}
	}
	return strings.NewReader(string(b))
}

// --- units ---

type unitState struct {
	Unit   string `json:"unit"`
	Active string `json:"active"`
	Sub    string `json:"sub"`
}

// parseUnits reads `systemctl list-units 'lab-app-*' --all --plain --no-legend`.
func parseUnits(out string) []unitState {
	var us []unitState
	for _, line := range strings.Split(out, "\n") {
		f := strings.Fields(line)
		if len(f) < 4 || !strings.HasPrefix(f[0], "lab-app-") {
			continue
		}
		us = append(us, unitState{Unit: f[0], Active: f[2], Sub: f[3]})
	}
	return us
}

// --- memory ---

func memoryStatus(meminfo string) map[string]any {
	kb := map[string]int64{}
	for _, line := range strings.Split(meminfo, "\n") {
		k, v, ok := strings.Cut(line, ":")
		if !ok {
			continue
		}
		n, err := strconv.ParseInt(strings.Fields(v)[0], 10, 64)
		if err == nil {
			kb[k] = n
		}
	}
	out := map[string]any{"total_gb": gb(uint64(kb["MemTotal"]) << 10), "available_gb": gb(uint64(kb["MemAvailable"]) << 10)}
	if kb["MemTotal"] > 0 {
		out["available_percent"] = int(kb["MemAvailable"] * 100 / kb["MemTotal"])
	}
	return out
}

// --- the audit itself ---

const auditWindow = 6 * time.Hour

func hostAudit(ctx context.Context, env *hostEnv, labdPort int, hosts map[string]string, now time.Time) map[string]any {
	var fs findings
	out := map[string]any{"at": now.UTC().Format(time.RFC3339), "window_hours": int(auditWindow.Hours())}

	// Listening sockets.
	var unexpected []listener
	if ss, err := env.run(ctx, 15*time.Second, "ss", "-Hltnup"); err != nil {
		out["listening_error"] = err.Error()
		fs.add("warning", "listeners-unreadable", "could not list listening sockets: "+err.Error())
	} else {
		ls := parseListeners(ss)
		for _, l := range ls {
			if !expectedListener(l, labdPort) {
				unexpected = append(unexpected, l)
				sev := "warning"
				if l.Address == "any" || !strings.HasPrefix(l.Address, "127.") && l.Address != "::1" {
					sev = "critical"
				}
				fs.add(sev, "unexpected-listener", l.Proto+" "+l.Address+":"+strconv.Itoa(l.Port)+" ("+l.Process+") is listening and is not part of the lab")
			}
		}
		if unexpected == nil {
			unexpected = []listener{}
		}
		out["listening"] = ls
		out["unexpected_listening"] = unexpected
	}

	// Firewall.
	if rules, err := env.run(ctx, 15*time.Second, "nft", "list", "ruleset"); err != nil {
		out["firewall"] = firewallAudit{Error: err.Error()}
		fs.add("critical", "firewall-unreadable", "could not read the firewall rules: "+err.Error())
	} else {
		fw := auditFirewall(rules)
		out["firewall"] = fw
		if !fw.Present || !fw.InputDrop {
			fs.add("critical", "firewall-open", "the firewall does not drop incoming traffic by default")
		}
		if fw.SSHAccepted {
			fs.add("critical", "firewall-ssh", "the firewall lets port 22 in; no SSH is a design rule here")
		}
		if !fw.LabApps {
			fs.add("critical", "firewall-apps", "the apps' firewall table (lab_apps) is missing: apps could reach the server and private networks")
		}
	}

	// Certificates.
	var certs []certAudit
	for host, path := range hosts {
		c := auditCert(host, path, now)
		certs = append(certs, c)
		switch {
		case c.Error != "":
			fs.add("warning", "cert-unreadable", host+": certificate "+c.Error)
		case c.DaysLeft < 0:
			fs.add("critical", "cert-expired", host+": certificate expired")
		case c.DaysLeft < 14:
			fs.add("warning", "cert-expiring", host+": certificate expires in "+strconv.Itoa(c.DaysLeft)+" days (renewal is not working)")
		}
	}
	sort.Slice(certs, func(a, b int) bool { return certs[a].Host < certs[b].Host })
	if certs == nil {
		certs = []certAudit{}
	}
	out["certificates"] = certs

	// Sign-ins.
	fails := recentSignInFailures(now.Add(-24 * time.Hour))
	out["sign_in_failures_24h"] = fails
	if len(fails) >= 10 {
		fs.add("critical", "sign-in-attempts", strconv.Itoa(len(fails))+" wrong passwords at the lab's sign-in in 24 hours")
	} else if len(fails) > 0 {
		fs.add("info", "sign-in-attempts", strconv.Itoa(len(fails))+" wrong password(s) at the lab's sign-in in 24 hours")
	}

	// Journal errors.
	if j, err := env.run(ctx, 20*time.Second, "journalctl", "--priority=err", "--since=-"+strconv.Itoa(int(auditWindow.Hours()))+"h", "--no-pager", "--output=short-iso", "--lines=100"); err == nil {
		lines := 0
		for _, l := range strings.Split(strings.TrimSpace(j), "\n") {
			if l != "" && !strings.HasPrefix(l, "-- ") {
				lines++
			}
		}
		out["journal_errors"] = map[string]any{"count": lines, "tail": tailLines(j, 20)}
		if lines >= 20 {
			fs.add("warning", "journal-errors", strconv.Itoa(lines)+" errors in the system journal in the last "+strconv.Itoa(int(auditWindow.Hours()))+" hours")
		}
	}

	// nginx.
	ng := auditNginx(tailBytes("/var/log/nginx/access.log", 4<<20), now, auditWindow)
	out["nginx"] = ng
	if ng.Status["5xx"] >= 50 {
		fs.add("warning", "nginx-5xx", strconv.Itoa(ng.Status["5xx"])+" server errors (5xx) from nginx in the last "+strconv.Itoa(int(auditWindow.Hours()))+" hours")
	}
	for _, ip := range ng.TopErrIPs {
		if ip.Count >= 300 {
			fs.add("warning", "nginx-scanner", ip.IP+" caused "+strconv.Itoa(ip.Count)+" refused or failed requests: probably a scanner")
		}
	}

	// App units.
	if u, err := env.run(ctx, 15*time.Second, "systemctl", "list-units", "lab-app-*", "--all", "--plain", "--no-legend"); err == nil {
		units := parseUnits(u)
		if units == nil {
			units = []unitState{}
		}
		out["units"] = units
		for _, x := range units {
			if x.Active == "failed" {
				sev := "warning"
				if strings.HasSuffix(x.Unit, "--prod.service") {
					sev = "critical"
				}
				fs.add(sev, "unit-failed", x.Unit+" has failed")
			}
		}
	}

	// Memory and disk.
	if b, err := os.ReadFile("/proc/meminfo"); err == nil {
		m := memoryStatus(string(b))
		out["memory"] = m
		if p, ok := m["available_percent"].(int); ok && p < 10 {
			fs.add("warning", "memory-low", "only "+strconv.Itoa(p)+" % of memory is available")
		}
	}
	disk := diskSpace("/")
	out["disk"] = disk
	if free, ok := disk["free_gb"].(float64); ok && free < 1 {
		fs.add("critical", "disk-full", "less than 1 GB of disk left")
	}

	// Patch state (reuses GET /api/host).
	host := hostStatus(ctx, env)
	out["host"] = host
	if up, ok := host["updates"].(map[string]any); ok {
		if sec, ok := up["security"].(int); ok && sec > 0 {
			fs.add("warning", "security-updates", strconv.Itoa(sec)+" security update(s) waiting")
		}
	}
	if rr, ok := host["reboot_required"].(bool); ok && rr {
		fs.add("warning", "reboot-required", "an update is waiting for a restart")
	}
	if h, ok := host["hardening"].(map[string]any); ok {
		if ssh, ok := h["ssh"].(string); ok && ssh != "" && ssh != "off" && !strings.HasPrefix(ssh, "off") {
			fs.add("critical", "ssh-on", "the SSH server is not off: "+ssh)
		}
	}

	if fs == nil {
		fs = findings{}
	}
	sort.SliceStable(fs, func(a, b int) bool { return sevRank(fs[a].Severity) < sevRank(fs[b].Severity) })
	out["findings"] = fs
	worst := "ok"
	if len(fs) > 0 {
		worst = fs[0].Severity
	}
	out["worst"] = worst
	return out
}

func sevRank(s string) int {
	switch s {
	case "critical":
		return 0
	case "warning":
		return 1
	}
	return 2
}

func tailLines(s string, n int) string {
	lines := strings.Split(strings.TrimRight(s, "\n"), "\n")
	if len(lines) > n {
		lines = lines[len(lines)-n:]
	}
	return strings.Join(lines, "\n")
}

// certHosts lists every host with a certificate and its chain file.
func (s *Server) certHosts() map[string]string {
	out := map[string]string{}
	set := s.store.Settings()
	if set.Domain == "" {
		return out
	}
	if cert, _, ok := s.mgr.certs.Paths(set.Domain); ok {
		out[set.Domain] = cert
	}
	for _, inst := range s.store.List() {
		host := hostFor(inst.App, inst.Env, set)
		if host == "" {
			continue
		}
		if cert, _, ok := s.mgr.certs.Paths(host); ok {
			out[host] = cert
		}
	}
	return out
}
