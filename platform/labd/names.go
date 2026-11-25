package main

import (
	"errors"
	"net"
	"regexp"
	"strings"
)

var (
	appNameRe     = regexp.MustCompile(`^[a-z][a-z0-9]{1,19}$`)
	previewNameRe = regexp.MustCompile(`^[a-z0-9]([a-z0-9-]{0,28}[a-z0-9])?$`)
	reservedApps  = map[string]bool{"lab": true, "www": true, "api": true, "mail": true}
)

const prodEnv = "prod"

func validAppName(name string) bool {
	return appNameRe.MatchString(name) && !reservedApps[name]
}

// validEnv accepts "prod" or a well-formed preview name.
func validEnv(env string) bool {
	return env == prodEnv || (previewNameRe.MatchString(env) && env != "main" && !strings.Contains(env, "--"))
}

// previewName is the preview name of a branch (see CONTRACT.md): the branch
// name itself, when it is one. Branches named otherwise get no preview, so
// two branches can never share one.
func previewName(branch string) (string, error) {
	if !previewNameRe.MatchString(branch) || strings.Contains(branch, "--") || branch == prodEnv || branch == "main" {
		return "", errors.New("branch name is not a preview name: use lowercase letters, digits and single hyphens, at most 30 characters")
	}
	return branch, nil
}

// branchOf returns the branch for a refs/heads/ ref, or "" for anything else.
func branchOf(ref string) string {
	if !strings.HasPrefix(ref, "refs/heads/") {
		return ""
	}
	return strings.TrimPrefix(ref, "refs/heads/")
}

func instanceID(app, env string) string { return app + "--" + env }

func unitName(id string) string { return "lab-app-" + id + ".service" }

// baseDomain is the configured domain, or <ip-with-dashes>.sslip.io in
// bootstrap mode.
func baseDomain(s Settings) string {
	if s.Domain != "" {
		return s.Domain
	}
	if s.PublicIP == "" {
		return ""
	}
	return strings.ReplaceAll(s.PublicIP, ".", "-") + ".sslip.io"
}

func hostFor(app, env string, s Settings) string {
	base := baseDomain(s)
	if base == "" {
		return ""
	}
	if env == prodEnv {
		return app + "." + base
	}
	return env + "--" + app + "." + base
}

func urlFor(app, env string, s Settings) string {
	host := hostFor(app, env, s)
	if host == "" {
		return ""
	}
	if s.Domain != "" {
		return "https://" + host
	}
	return "http://" + host
}

// validDomain accepts the lab's own domain for domain mode, such as
// lab.example.com: a lowercase DNS name with room for the longest app
// host below it. The lab itself answers on that name; apps on names below.
func validDomain(d string) bool {
	if !safeHost(d) || !strings.Contains(d, ".") || len(d) > 200 || strings.HasSuffix(d, ".sslip.io") {
		return false
	}
	tld := d[strings.LastIndex(d, ".")+1:]
	return strings.Trim(tld, "0123456789") != "" // not an IP address
}

func validPublicIPv4(s string) bool {
	ip := net.ParseIP(s)
	return ip != nil && ip.To4() != nil && strings.Count(s, ".") == 3 && !strings.Contains(s, ":")
}

// normalizeAllowIP accepts an IP address or a CIDR range and returns it in
// canonical form, so it can be written safely into nginx's allow directive.
func normalizeAllowIP(s string) (string, bool) {
	if ip := net.ParseIP(s); ip != nil {
		if v4 := ip.To4(); v4 != nil && !strings.Contains(s, ":") {
			return v4.String(), true
		}
		return ip.String(), true
	}
	if _, n, err := net.ParseCIDR(s); err == nil {
		ones, _ := n.Mask.Size()
		if ones == 0 {
			return "", false // 0.0.0.0/0 would open everything
		}
		return n.String(), true
	}
	return "", false
}
