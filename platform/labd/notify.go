package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"
	"unicode/utf8"
)

// The notification bus: everything that wants to reach the owner's phone goes
// through here, and from here through the Notify app's production instance
// on this server (POST /send with the token from its secrets folder). No
// token ever passes through GitHub: labd reads it from disk, where Notify
// itself put it.
//
// Senders are labd itself (a failed production deploy, an app that is down,
// a critical audit finding, the daily heartbeat), the pipeline through
// POST /api/notify, and apps through their outbox folder
// ($DATA_DIR/notify/outbox/*.json), which labd empties every few seconds.
// Sending never blocks or fails the caller: with Notify not deployed or
// refusing, the message is logged and dropped.

const (
	notifyApp      = "notify"
	notifyMaxTitle = 120
	notifyMaxBody  = 1000
	outboxFolder   = "notify/outbox"
	outboxMaxFile  = 16 << 10
	outboxMaxBatch = 20 // files per instance per scan
	outboxMaxHour  = 30 // messages per instance per hour
)

var notifyTopics = map[string]bool{"security": true, "lab": true, "bots": true}

// message is what Notify's POST /send takes.
type message struct {
	Topic string `json:"topic"`
	Title string `json:"title"`
	Body  string `json:"body,omitempty"`
	URL   string `json:"url,omitempty"`
}

// validMessage checks a message the way Notify will, so a refusal is caught
// here with a clear reason.
func validMessage(m message) error {
	switch {
	case !notifyTopics[m.Topic]:
		return errors.New("unknown topic: must be security, lab or bots")
	case m.Title == "" || !utf8.ValidString(m.Title) || utf8.RuneCountInString(m.Title) > notifyMaxTitle:
		return errors.New("title must be 1 to 120 characters")
	case !utf8.ValidString(m.Body) || utf8.RuneCountInString(m.Body) > notifyMaxBody:
		return errors.New("body must be at most 1000 characters")
	case m.URL != "" && !strings.HasPrefix(m.URL, "https://") && !strings.HasPrefix(m.URL, "http://"):
		return errors.New("url must be a web address")
	}
	return nil
}

// clip shortens s to at most n runes, marking the cut.
func clip(s string, n int) string {
	if utf8.RuneCountInString(s) <= n {
		return s
	}
	r := []rune(s)
	return string(r[:n-1]) + "…"
}

type notifier struct {
	store     *Store
	runner    Runner
	client    *http.Client
	health    *http.Client
	statePath string // <state_dir>/notify-sent.json: dedupe keys and when they were last sent
	now       func() time.Time
	sleep     func(time.Duration) // the wait before the one retry
	version   string

	mu        sync.Mutex
	sent      map[string]time.Time   // dedupe key → last sent
	complaint time.Time              // last "Notify is not reachable" log line
	counts    map[string][]time.Time // outbox messages per instance in the last hour
	down      map[string]bool        // production instances reported down
	lastAudit string                 // worst of the last audit, for the heartbeat
}

func newNotifier(cfg *Config, store *Store, runner Runner, version string) *notifier {
	n := &notifier{
		store:     store,
		runner:    runner,
		client:    &http.Client{Timeout: 10 * time.Second},
		health:    &http.Client{Timeout: 3 * time.Second, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }},
		statePath: filepath.Join(cfg.StateDir, "notify-sent.json"),
		now:       time.Now,
		sleep:     time.Sleep,
		version:   version,
		sent:      map[string]time.Time{},
		counts:    map[string][]time.Time{},
		down:      map[string]bool{},
	}
	var saved map[string]time.Time
	if err := readJSON(n.statePath, &saved); err == nil && saved != nil {
		n.sent = saved
	}
	return n
}

// target is Notify's production instance: where to post and with what token.
// ok is false when it is not deployed, not running or has no token yet.
func (n *notifier) target() (url, token string, ok bool) {
	inst, found := n.store.Get(instanceID(notifyApp, prodEnv))
	if !found || inst.Status != "running" {
		return "", "", false
	}
	root, _ := n.runner.DataDir(inst.ID())
	f, err := os.OpenFile(filepath.Join(root, secretsFolder, "token"), os.O_RDONLY|syscall.O_NOFOLLOW, 0)
	if err != nil {
		return "", "", false
	}
	defer f.Close()
	b, err := io.ReadAll(io.LimitReader(f, 4096))
	if err != nil || len(bytes.TrimSpace(b)) == 0 {
		return "", "", false
	}
	return "http://127.0.0.1:" + strconv.Itoa(inst.Port) + "/send", string(bytes.TrimSpace(b)), true
}

// complain logs why a message was dropped, at most once an hour.
func (n *notifier) complain(why string) {
	n.mu.Lock()
	defer n.mu.Unlock()
	if n.now().Sub(n.complaint) < time.Hour {
		return
	}
	n.complaint = n.now()
	log.Printf("notify: %s (saying this at most once an hour)", why)
}

// send posts a message to Notify and reports whether Notify accepted it.
// One retry after a short wait covers a Notify that is just restarting.
func (n *notifier) send(ctx context.Context, m message) bool {
	if err := validMessage(m); err != nil {
		log.Printf("notify: dropping %q: %v", m.Title, err)
		return false
	}
	url, token, ok := n.target()
	if !ok {
		n.complain("Notify is not running in production or has no token yet; dropped " + strconv.Quote(m.Title))
		return false
	}
	payload, _ := json.Marshal(m)
	var lastErr error
	for attempt := 0; attempt < 2; attempt++ {
		if attempt > 0 {
			n.sleep(5 * time.Second)
		}
		req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(payload))
		if err != nil {
			return false
		}
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("Authorization", "Bearer "+token)
		resp, err := n.client.Do(req)
		if err != nil {
			lastErr = err
			continue
		}
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
		resp.Body.Close()
		if resp.StatusCode == http.StatusOK {
			return true
		}
		lastErr = fmt.Errorf("Notify answered %d: %s", resp.StatusCode, strings.TrimSpace(string(body)))
		if resp.StatusCode >= 400 && resp.StatusCode < 500 {
			break // a refusal does not get better by retrying
		}
	}
	n.complain(fmt.Sprintf("could not deliver %q: %v", m.Title, lastErr))
	return false
}

// sendOnce sends a message unless the same key was sent within the window.
// Keys live in the state file, so a restart does not repeat a message.
func (n *notifier) sendOnce(ctx context.Context, key string, within time.Duration, m message) bool {
	n.mu.Lock()
	last, seen := n.sent[key]
	if seen && n.now().Sub(last) < within {
		n.mu.Unlock()
		return false
	}
	n.mu.Unlock()
	if !n.send(ctx, m) {
		return false
	}
	n.mu.Lock()
	n.sent[key] = n.now()
	n.saveLocked()
	n.mu.Unlock()
	return true
}

// forget drops a dedupe key, so the next sendOnce with it sends again.
func (n *notifier) forget(key string) {
	n.mu.Lock()
	defer n.mu.Unlock()
	delete(n.sent, key)
	n.saveLocked()
}

func (n *notifier) saveLocked() {
	for k, t := range n.sent {
		if n.now().Sub(t) > 7*24*time.Hour {
			delete(n.sent, k)
		}
	}
	b, _ := json.MarshalIndent(n.sent, "", "  ")
	if err := writeFileAtomic(n.statePath, b, 0o600); err != nil {
		log.Printf("notify: saving state: %v", err)
	}
}

// --- labd's own events ---

// deployFailed tells the owner a production deploy did not pass its health check.
func (n *notifier) deployFailed(ctx context.Context, app, env string, derr *DeployError, set Settings) {
	if env != prodEnv {
		return
	}
	body := "The new release did not pass its health check. "
	if derr.RolledBack {
		body += "The previous release is serving again."
	} else {
		body += "The previous release did not come back either: the app is down."
	}
	n.send(ctx, message{Topic: "lab", Title: "Deploy of " + app + " failed", Body: body, URL: urlFor(app, env, set)})
}

// auditDone sends the critical findings of an audit, once a day per set of
// findings, and remembers the verdict for the heartbeat.
func (n *notifier) auditDone(ctx context.Context, worst string, fs findings) {
	n.mu.Lock()
	n.lastAudit = worst
	n.mu.Unlock()
	if worst != "critical" {
		return
	}
	var codes, texts []string
	for _, f := range fs {
		if f.Severity != "critical" {
			continue
		}
		codes = append(codes, f.Code)
		if len(texts) < 4 {
			texts = append(texts, f.Text)
		}
	}
	sort.Strings(codes)
	n.sendOnce(ctx, "audit:"+strings.Join(codes, ","), 24*time.Hour, message{
		Topic: "security",
		Title: fmt.Sprintf("Server: %d critical finding(s)", len(codes)),
		Body:  clip(strings.Join(texts, "\n"), notifyMaxBody),
	})
}

// watchHealth checks every running production app and reports one that does
// not answer its health check (once per 6 hours while it stays down) and its
// return.
func (n *notifier) watchHealth(ctx context.Context) {
	set := n.store.Settings()
	for _, inst := range n.store.List() {
		if inst.Env != prodEnv || inst.Status != "running" {
			continue
		}
		url := "http://127.0.0.1:" + strconv.Itoa(inst.Port) + inst.Health
		healthy := n.healthy(ctx, url)
		n.mu.Lock()
		wasDown := n.down[inst.ID()]
		n.down[inst.ID()] = !healthy
		n.mu.Unlock()
		switch {
		case !healthy:
			n.sendOnce(ctx, "down:"+inst.ID(), 6*time.Hour, message{
				Topic: "lab", Title: inst.App + " is down",
				Body: "Its health check (" + inst.Health + ") did not answer OK. labd restarts it if its process stopped; if this keeps coming, look at its logs.",
				URL:  urlFor(inst.App, inst.Env, set),
			})
		case wasDown:
			n.forget("down:" + inst.ID())
			n.send(ctx, message{Topic: "lab", Title: inst.App + " is back", Body: "Its health check answers OK again.", URL: urlFor(inst.App, inst.Env, set)})
		}
	}
}

func (n *notifier) healthy(ctx context.Context, url string) bool {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return false
	}
	resp, err := n.health.Do(req)
	if err != nil {
		return false
	}
	io.Copy(io.Discard, io.LimitReader(resp.Body, 1<<16))
	resp.Body.Close()
	return resp.StatusCode == http.StatusOK
}

// heartbeat sends "Lab is alive" once a day, at or after 08:00 Prague time, so a
// quiet phone means something is wrong rather than that nothing happened.
func (n *notifier) heartbeat(ctx context.Context) {
	loc, err := time.LoadLocation("Europe/Prague")
	if err != nil {
		loc = time.UTC
	}
	now := n.now().In(loc)
	if now.Hour() < 8 {
		return
	}
	running := 0
	for _, inst := range n.store.List() {
		if inst.Env == prodEnv && inst.Status == "running" {
			running++
		}
	}
	n.mu.Lock()
	audit := n.lastAudit
	n.mu.Unlock()
	if audit == "" {
		audit = "zatím žádný"
	}
	n.sendOnce(ctx, "heartbeat:"+now.Format("2006-01-02"), 48*time.Hour, message{
		Topic: "lab", Title: "Lab is alive",
		Body: fmt.Sprintf("%d aplikací běží, poslední audit: %s, labd %s", running, audit, n.version),
	})
}

// --- the apps' outbox ---

type outboxMessage struct {
	Topic string `json:"topic"`
	Title string `json:"title"`
	Body  string `json:"body"`
	URL   string `json:"url"`
}

// scanOutboxes empties every running instance's outbox folder. Production
// messages are sent with the app's name in front; a preview's are dropped
// (a preview holds a copy of production's data and must not reach the
// phone). Only plain files are read, never through a link, and never more
// than 16 KB; a file is removed whatever happens to it, so a bad one cannot
// jam the queue.
func (n *notifier) scanOutboxes(ctx context.Context) {
	for _, inst := range n.store.List() {
		if inst.Status != "running" {
			continue
		}
		root, _ := n.runner.DataDir(inst.ID())
		dir := filepath.Join(root, outboxFolder)
		entries, err := os.ReadDir(dir)
		if err != nil {
			continue // no outbox: the usual case
		}
		seen := 0
		for _, e := range entries {
			if seen >= outboxMaxBatch {
				break
			}
			path := filepath.Join(dir, e.Name())
			if !e.Type().IsRegular() {
				// A link or anything else odd is never followed; it goes
				// away so it cannot sit there forever (a folder stays).
				if os.Remove(path) == nil {
					log.Printf("notify: %s: removed %s from the outbox: not a plain file", inst.ID(), e.Name())
				}
				continue
			}
			if !strings.HasSuffix(e.Name(), ".json") {
				continue // the helper's .tmp files on their way to being renamed
			}
			seen++
			raw, err := readOutboxFile(path)
			os.Remove(path)
			if err != nil {
				log.Printf("notify: %s: outbox file %s: %v", inst.ID(), e.Name(), err)
				continue
			}
			var m outboxMessage
			if err := json.Unmarshal(raw, &m); err != nil {
				log.Printf("notify: %s: outbox file %s is not valid JSON; removed", inst.ID(), e.Name())
				continue
			}
			if inst.Env != prodEnv {
				log.Printf("notify: %s: preview message %q dropped (previews don't reach the phone)", inst.ID(), m.Title)
				continue
			}
			if !n.allow(inst.ID()) {
				log.Printf("notify: %s: more than %d messages in an hour; dropped %q", inst.ID(), outboxMaxHour, m.Title)
				continue
			}
			topic := strings.TrimSpace(m.Topic)
			if topic == "" {
				topic = "bots"
			}
			title := strings.TrimSpace(m.Title)
			if title == "" {
				title = "message"
			}
			msg := message{
				Topic: topic,
				Title: clip(inst.App+": "+title, notifyMaxTitle),
				Body:  clip(strings.TrimSpace(m.Body), notifyMaxBody),
				URL:   strings.TrimSpace(m.URL),
			}
			n.send(ctx, msg)
		}
	}
}

func readOutboxFile(path string) ([]byte, error) {
	f, err := os.OpenFile(path, os.O_RDONLY|syscall.O_NOFOLLOW, 0)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	st, err := f.Stat()
	if err != nil {
		return nil, err
	}
	if !st.Mode().IsRegular() {
		return nil, errors.New("not a plain file")
	}
	if st.Size() > outboxMaxFile {
		return nil, fmt.Errorf("%d bytes is more than the %d allowed", st.Size(), outboxMaxFile)
	}
	return io.ReadAll(io.LimitReader(f, outboxMaxFile+1))
}

// allow counts a message against the instance's hourly budget.
func (n *notifier) allow(id string) bool {
	n.mu.Lock()
	defer n.mu.Unlock()
	now := n.now()
	kept := n.counts[id][:0]
	for _, t := range n.counts[id] {
		if now.Sub(t) < time.Hour {
			kept = append(kept, t)
		}
	}
	if len(kept) >= outboxMaxHour {
		n.counts[id] = kept
		return false
	}
	n.counts[id] = append(kept, now)
	return true
}

// --- POST /api/notify ---

// notify is POST /api/notify: the pipeline's way to the owner's phone, with its
// OIDC token instead of a secret. Only main may use it.
func (s *Server) notify(w http.ResponseWriter, r *http.Request) {
	_, b := s.authenticate(w, r, audAPI, smallBody, "", mainOnly)
	if b == nil {
		return
	}
	var m message
	if err := json.Unmarshal(b.data, &m); err != nil {
		writeErr(w, http.StatusBadRequest, "the body must be JSON with topic, title, body and url")
		return
	}
	m.Topic, m.Title, m.Body, m.URL = strings.TrimSpace(m.Topic), strings.TrimSpace(m.Title), strings.TrimSpace(m.Body), strings.TrimSpace(m.URL)
	if err := validMessage(m); err != nil {
		writeErr(w, http.StatusBadRequest, err.Error())
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 30*time.Second)
	defer cancel()
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "delivered": s.notifier.send(ctx, m)})
}
