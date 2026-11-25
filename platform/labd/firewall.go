package main

import (
	"context"
	"fmt"
	"log"
	"net"
	"net/netip"
	"os"
	"os/exec"
	"sort"
	"strings"
	"sync"
	"time"
)

// sandboxState is what the app sandbox could enforce on this server, for
// /api/status: whether binding is limited, whether the hardened sandbox
// works, and how the firewall went.
var sandboxState struct {
	sync.Mutex
	bindRules   bool
	bindErr     string
	hardened    bool
	hardenedErr string
	firewall    string // "", "applied", or "failed: ..."
	settled     bool   // the startup work (hardening, firewall, sandbox refresh) is done
}

func setSettled() {
	sandboxState.Lock()
	sandboxState.settled = true
	sandboxState.Unlock()
}

func setFirewallState(s string) {
	sandboxState.Lock()
	sandboxState.firewall = s
	sandboxState.Unlock()
}

func sandboxHardened() bool {
	sandboxState.Lock()
	defer sandboxState.Unlock()
	return sandboxState.hardened
}

// sandboxStatus is reported by /api/status on a systemd server.
func sandboxStatus() map[string]any {
	sandboxState.Lock()
	defer sandboxState.Unlock()
	out := map[string]any{"version": sandboxVersion, "bind_rules": sandboxState.bindRules,
		"hardened": sandboxState.hardened, "firewall": sandboxState.firewall, "settled": sandboxState.settled}
	if sandboxState.hardenedErr != "" {
		out["hardened_error"] = sandboxState.hardenedErr
	}
	if sandboxState.bindErr != "" {
		out["bind_rules_error"] = sandboxState.bindErr
	}
	return out
}

// appUsers are the IDs systemd gives dynamic users; nginx, labd and the
// system are outside that range.
const appUsers = "meta skuid 61184-65519"

// Addresses apps may never send to: private networks, link-local (with the
// cloud's metadata service at 169.254.169.254), carrier-grade NAT, multicast
// and the other special ranges.
const (
	blockedV4 = "0.0.0.0/8, 10.0.0.0/8, 100.64.0.0/10, 169.254.0.0/16, 172.16.0.0/12, 192.0.0.0/24, 192.168.0.0/16, 198.18.0.0/15, 224.0.0.0/3"
	blockedV6 = "fc00::/7, fe80::/10, ff00::/8"
)

// appFirewall keeps apps from opening connections to the server itself (on
// any of its addresses) and to private, link-local and metadata addresses,
// so a preview can't reach production's port, labd, another app behind
// nginx's back, or the cloud's metadata service. DNS to the resolvers in
// resolv.conf stays open, and so does the internet. Replies to nginx's
// requests are not new connections and pass. The first two lines make the
// text safe to apply again and again.
func appFirewall(resolvers, locals []netip.Addr) string {
	var b strings.Builder
	b.WriteString("table inet lab_apps\ndelete table inet lab_apps\ntable inet lab_apps {\n")
	b.WriteString("\tchain output {\n\t\ttype filter hook output priority filter; policy accept;\n")
	rule := func(format string, args ...any) {
		fmt.Fprintf(&b, "\t\t%s "+format+"\n", append([]any{appUsers}, args...)...)
	}
	rule("ip daddr 127.0.0.0/8 udp dport 53 accept")
	rule("ip daddr 127.0.0.0/8 tcp dport 53 accept")
	for _, a := range resolvers {
		fam := "ip"
		if a.Is6() {
			fam = "ip6"
		}
		rule("%s daddr %s udp dport 53 accept", fam, a)
		rule("%s daddr %s tcp dport 53 accept", fam, a)
	}
	rule("ip daddr 127.0.0.0/8 ct state new drop")
	rule("ip6 daddr ::1 ct state new drop")
	// An app answers nginx only from the apps' port range, so one that took
	// labd's port while labd restarted could never answer in its place.
	rule("ip saddr 127.0.0.0/8 tcp sport != %d-%d drop", portMin, portMax)
	rule("ip6 saddr ::1 tcp sport != %d-%d drop", portMin, portMax)
	var v4, v6 []string
	for _, a := range locals {
		if a.Is4() {
			v4 = append(v4, a.String())
		} else {
			v6 = append(v6, a.String())
		}
	}
	if len(v4) > 0 {
		rule("ip daddr { %s } ct state new drop", strings.Join(v4, ", "))
	}
	if len(v6) > 0 {
		rule("ip6 daddr { %s } ct state new drop", strings.Join(v6, ", "))
	}
	rule("ip daddr { %s } drop", blockedV4)
	rule("ip6 daddr { %s } drop", blockedV6)
	b.WriteString("\t}\n}\n")
	return b.String()
}

// blockedClients are addresses that may not reach the server at all: scanners
// that hammered nginx with hundreds of refused requests in a day (the audit's
// "nginx-scanner" finding). The daily security review adds one with a pull
// request that says when and why, and removes entries that have been quiet
// for a few months. Single addresses only: blocking a whole network is
// the owner's call. The list starts empty.
var blockedClients = []string{}

// blockedFirewall drops everything from blockedClients in a table of its own,
// ahead of the system's rules (priority filter - 10), so even a connection
// that was open when the address was added ends. Like appFirewall, the first
// two lines make the text safe to apply again and again. An entry that is not
// an address is skipped with a log line rather than breaking the whole file.
func blockedFirewall(clients []string) string {
	var v4, v6 []string
	for _, c := range clients {
		a, err := netip.ParseAddr(c)
		if err != nil || a.Zone() != "" {
			log.Printf("blocked clients: %q is not an address, skipped", c)
			continue
		}
		a = a.Unmap()
		if a.Is4() {
			v4 = append(v4, a.String())
		} else {
			v6 = append(v6, a.String())
		}
	}
	var b strings.Builder
	b.WriteString("table inet lab_blocked\ndelete table inet lab_blocked\ntable inet lab_blocked {\n")
	b.WriteString("\tchain input {\n\t\ttype filter hook input priority filter - 10; policy accept;\n")
	if len(v4) > 0 {
		fmt.Fprintf(&b, "\t\tip saddr { %s } drop\n", strings.Join(v4, ", "))
	}
	if len(v6) > 0 {
		fmt.Fprintf(&b, "\t\tip6 saddr { %s } drop\n", strings.Join(v6, ", "))
	}
	b.WriteString("\t}\n}\n")
	return b.String()
}

// resolvers reads the name servers from resolv.conf (loopback ones are
// covered by the loopback rule).
func resolvers(path string) []netip.Addr {
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil
	}
	var out []netip.Addr
	for _, line := range strings.Split(string(raw), "\n") {
		f := strings.Fields(line)
		if len(f) < 2 || f[0] != "nameserver" {
			continue
		}
		if a, err := netip.ParseAddr(f[1]); err == nil && !a.IsLoopback() && a.Zone() == "" {
			out = append(out, a.Unmap())
		}
	}
	return uniqueAddrs(out)
}

// localAddrs are this server's own addresses (and the public IP from the
// settings, in case it isn't on an interface), loopback and link-local left
// out because other rules cover them.
func localAddrs(publicIP string) []netip.Addr {
	var out []netip.Addr
	if ifs, err := net.InterfaceAddrs(); err == nil {
		for _, a := range ifs {
			if n, ok := a.(*net.IPNet); ok {
				if ip, ok := netip.AddrFromSlice(n.IP); ok {
					out = append(out, ip.Unmap())
				}
			}
		}
	}
	if ip, err := netip.ParseAddr(publicIP); err == nil {
		out = append(out, ip.Unmap())
	}
	var keep []netip.Addr
	for _, a := range out {
		if !a.IsLoopback() && !a.IsLinkLocalUnicast() && !a.IsUnspecified() && a.Zone() == "" {
			keep = append(keep, a)
		}
	}
	return uniqueAddrs(keep)
}

func uniqueAddrs(in []netip.Addr) []netip.Addr {
	sort.Slice(in, func(i, j int) bool { return in[i].Less(in[j]) })
	var out []netip.Addr
	for i, a := range in {
		if i == 0 || a != in[i-1] {
			out = append(out, a)
		}
	}
	return out
}

// ensureAppFirewall applies appFirewall and blockedFirewall with nft and keeps
// them in file, from where nftables.service loads them again after the
// system's rules (host.go). A failure is logged, not fatal: labd must keep
// serving deploys to be able to receive a fix.
func ensureAppFirewall(ctx context.Context, set Settings, file string) {
	ctx, cancel := context.WithTimeout(ctx, 20*time.Second)
	defer cancel()
	text := appFirewall(resolvers("/etc/resolv.conf"), localAddrs(set.PublicIP)) + blockedFirewall(blockedClients)
	cmd := exec.CommandContext(ctx, "nft", "-f", "-")
	cmd.Stdin = strings.NewReader(text)
	if out, err := cmd.CombinedOutput(); err != nil {
		msg := strings.TrimSpace(err.Error() + ": " + string(out))
		log.Printf("app firewall not applied: %s", msg)
		setFirewallState("failed: " + msg)
		return
	}
	if _, err := writeIfChanged(file, text); err != nil {
		log.Printf("app firewall: %v", err)
	}
	setFirewallState("applied")
}
