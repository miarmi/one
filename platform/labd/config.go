package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
)

const (
	githubIssuer = "https://token.actions.githubusercontent.com"
	githubJWKS   = githubIssuer + "/.well-known/jwks"
)

// Config is /etc/lab/labd.json. Only these keys are allowed (CONTRACT.md).
type Config struct {
	Repo        string `json:"repo"`
	Listen      string `json:"listen"`
	OIDCIssuer  string `json:"oidc_issuer,omitempty"`
	OIDCJWKSURL string `json:"oidc_jwks_url,omitempty"`
	StateDir    string `json:"state_dir,omitempty"`
	Runner      string `json:"runner,omitempty"`
	Edge        string `json:"edge,omitempty"`
	Certs       string `json:"certs,omitempty"`

	path  string // file the config was read from
	paths paths  // fixed server paths; tests override them in Go
}

// paths are the places labd touches outside its state dir.
type paths struct {
	binary       string // labd itself, replaced by POST /api/platform
	nginxSite    string
	nginxCommon  string
	webroot      string
	certLive     string // certbot's certificates, one folder per host
	dataRoot     string // root's view of systemd StateDirectory=lab/<id> with DynamicUser
	appDataRoot  string // the same directory as the app sees it (DATA_DIR)
	labdUpstream string // where nginx reaches labd
	sysctlConf   string // host hardening (host.go)
	modprobeConf string
	procSys      string
	procModules  string
	unitDir      string
	journaldConf string // journald's drop-in: how long logs are kept (host.go)
	shadow       string // root's password aging (host.go)
	appFirewall  string // the apps' firewall as applied last, for nftables.service
}

func defaultPaths(listen string) paths {
	return paths{
		binary:       "/usr/local/bin/labd",
		nginxSite:    "/etc/nginx/sites-available/lab",
		nginxCommon:  "/etc/nginx/conf.d/lab-common.conf",
		webroot:      "/var/www/acme",
		certLive:     "/etc/letsencrypt/live",
		dataRoot:     "/var/lib/private/lab",
		appDataRoot:  "/var/lib/lab",
		labdUpstream: "http://" + listen,
		sysctlConf:   "/etc/sysctl.d/90-lab-hardening.conf",
		modprobeConf: "/etc/modprobe.d/00-lab-hardening.conf",
		procSys:      "/proc/sys",
		procModules:  "/proc/modules",
		unitDir:      "/etc/systemd/system",
		journaldConf: "/etc/systemd/journald.conf.d/50-lab.conf",
		shadow:       "/etc/shadow",
		appFirewall:  "/etc/lab/apps-firewall.nft",
	}
}

func loadConfig(path string) (*Config, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.DisallowUnknownFields()
	var c Config
	if err := dec.Decode(&c); err != nil {
		return nil, fmt.Errorf("%s: %w", path, err)
	}
	c.path = path
	if err := c.finish(); err != nil {
		return nil, fmt.Errorf("%s: %w", path, err)
	}
	return &c, nil
}

// finish fills defaults and validates.
func (c *Config) finish() error {
	if c.Repo == "" {
		return errors.New("repo is required")
	}
	if c.Listen == "" {
		c.Listen = "127.0.0.1:7070"
	}
	if c.OIDCIssuer == "" {
		c.OIDCIssuer = githubIssuer
	}
	if c.OIDCJWKSURL == "" {
		c.OIDCJWKSURL = githubJWKS
	}
	if c.StateDir == "" {
		c.StateDir = "/srv/lab"
	}
	if !filepath.IsAbs(c.StateDir) {
		return errors.New("state_dir must be an absolute path")
	}
	if c.Runner == "" {
		c.Runner = "systemd"
	}
	if c.Edge == "" {
		c.Edge = "nginx"
	}
	if c.Certs == "" {
		c.Certs = "certbot"
	}
	switch {
	case c.Runner != "systemd" && c.Runner != "exec":
		return errors.New("runner must be systemd or exec")
	case c.Edge != "nginx" && c.Edge != "none":
		return errors.New("edge must be nginx or none")
	case c.Certs != "certbot" && c.Certs != "selfsigned":
		return errors.New("certs must be certbot or selfsigned")
	}
	if c.paths == (paths{}) {
		c.paths = defaultPaths(c.Listen)
	}
	return nil
}
