package main

import (
	"errors"
	"fmt"
	"net"
	"net/url"
	"os"
	"strconv"
	"strings"
	"time"
)

// Config is everything the bridge needs. It comes from the environment and
// from owner-only files, never from the command line, so no token shows up in
// a process list or a shell history.
type Config struct {
	OneURL       string        // the one ONE address, https
	BeeperURL    string        // Beeper Desktop on this Mac, localhost only
	StatePath    string        // where the bridge keeps its place
	BeeperToken  string        // Beeper access token
	OneToken     string        // ONE bridge token
	Poll         time.Duration // how often to look for news
	BackfillDays int           // how far back a chat is read the first time, until ONE says otherwise
}

func isLoopbackHost(h string) bool {
	if h == "localhost" {
		return true
	}
	ip := net.ParseIP(strings.Trim(h, "[]"))
	return ip != nil && ip.IsLoopback()
}

// Validate refuses anything but Beeper on this Mac and the one ONE address.
func (c *Config) Validate() error {
	bu, err := url.Parse(c.BeeperURL)
	if err != nil || bu.Host == "" || (bu.Scheme != "http" && bu.Scheme != "https") {
		return errors.New("BEEPER_URL is not an address")
	}
	if !isLoopbackHost(bu.Hostname()) {
		return errors.New("BEEPER_URL must be this Mac (localhost); the bridge talks to nothing else for Beeper")
	}
	if bu.User != nil || bu.RawQuery != "" || (bu.Path != "" && bu.Path != "/") {
		return errors.New("BEEPER_URL must be only a host and a port")
	}
	ou, err := url.Parse(c.OneURL)
	if err != nil || ou.Host == "" {
		return errors.New("ONE_URL is missing or is not an address")
	}
	if ou.User != nil || ou.RawQuery != "" || ou.Fragment != "" || (ou.Path != "" && ou.Path != "/") {
		return errors.New("ONE_URL must be only the address of ONE")
	}
	if ou.Scheme != "https" && !(ou.Scheme == "http" && isLoopbackHost(ou.Hostname())) {
		return errors.New("ONE_URL must be https")
	}
	if c.BeeperToken == "" || c.OneToken == "" {
		return errors.New("both tokens are needed (see the README)")
	}
	if c.BeeperToken == c.OneToken {
		return errors.New("the two tokens are the same; one of them is wrong")
	}
	if c.StatePath == "" {
		return errors.New("no place to keep the state")
	}
	if c.Poll < time.Second {
		return errors.New("poll interval is too short")
	}
	return nil
}

// tokenFrom reads a token from the environment, or else from an owner-only file.
func tokenFrom(env, fileEnv, defaultFile string) (string, error) {
	if v := strings.TrimSpace(os.Getenv(env)); v != "" {
		return v, nil
	}
	path := os.Getenv(fileEnv)
	if path == "" {
		path = defaultFile
	}
	st, err := os.Stat(path)
	if err != nil {
		return "", fmt.Errorf("no %s and no readable token file (see the README)", env)
	}
	if st.Mode().Perm()&0o077 != 0 {
		return "", fmt.Errorf("the token file %s can be read by others; run chmod 600 on it", baseName(path))
	}
	b, err := os.ReadFile(path)
	if err != nil {
		return "", fmt.Errorf("the token file for %s cannot be read", env)
	}
	return strings.TrimSpace(string(b)), nil
}

func baseName(p string) string {
	if i := strings.LastIndexByte(p, '/'); i >= 0 {
		return p[i+1:]
	}
	return p
}

func envInt(name string, def int) int {
	if v, err := strconv.Atoi(os.Getenv(name)); err == nil && v > 0 {
		return v
	}
	return def
}

// ConfigFromEnv builds the configuration.
func ConfigFromEnv() (Config, error) {
	home, _ := os.UserHomeDir()
	dir := home + "/.one-bridge"
	c := Config{
		OneURL:       strings.TrimRight(strings.TrimSpace(os.Getenv("ONE_URL")), "/"),
		BeeperURL:    strings.TrimRight(strings.TrimSpace(os.Getenv("BEEPER_URL")), "/"),
		StatePath:    os.Getenv("BRIDGE_STATE"),
		Poll:         time.Duration(envInt("BRIDGE_POLL_SECONDS", 20)) * time.Second,
		BackfillDays: envInt("BRIDGE_BACKFILL_DAYS", 30),
	}
	if c.BeeperURL == "" {
		c.BeeperURL = "http://localhost:23373"
	}
	if c.StatePath == "" {
		c.StatePath = dir + "/state.json"
	}
	var err error
	if c.BeeperToken, err = tokenFrom("BEEPER_TOKEN", "BEEPER_TOKEN_FILE", dir+"/beeper-token"); err != nil {
		return c, err
	}
	if c.OneToken, err = tokenFrom("ONE_BRIDGE_TOKEN", "ONE_BRIDGE_TOKEN_FILE", dir+"/one-token"); err != nil {
		return c, err
	}
	return c, c.Validate()
}
