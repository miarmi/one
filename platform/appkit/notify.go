// Package appkit holds the little the lab's apps share: a way to reach
// The owner's phone, and (next to it) a common way to log. It is standard
// library only, so importing it costs an app nothing.
package appkit

import (
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"strings"
	"time"
	"unicode/utf8"
)

// Notify sends a message to the owner's phone. The app writes it into its
// outbox folder ($DATA_DIR/notify/outbox); labd picks it up within seconds
// and sends it through the Notify app with the app's name in front of the
// title. Apps need no key and no network for this, and nothing an app does
// can block on it. Messages from a preview never reach the phone (labd
// drops them), so an app can call this freely in every environment. Run
// locally (no DATA_DIR) the message is only logged. An app may send at most
// 30 messages an hour; beyond that labd drops them.
func Notify(title, body, url string) error {
	return NotifyTopic("bots", title, body, url)
}

// NotifyTopic is Notify with a topic: "bots" (the default for apps),
// "lab" or "security". The owner picks per device which topics he gets.
func NotifyTopic(topic, title, body, url string) error {
	title = strings.TrimSpace(title)
	if title == "" || utf8.RuneCountInString(title) > 100 {
		return errors.New("appkit.Notify: the title must be 1 to 100 characters")
	}
	if utf8.RuneCountInString(body) > 1000 {
		return errors.New("appkit.Notify: the body must be at most 1000 characters")
	}
	dir := os.Getenv("DATA_DIR")
	if dir == "" {
		log.Printf("notify (no DATA_DIR, not sent): [%s] %s — %s", topic, title, body)
		return nil
	}
	outbox := filepath.Join(dir, "notify", "outbox")
	if err := os.MkdirAll(outbox, 0o700); err != nil {
		return fmt.Errorf("appkit.Notify: %w", err)
	}
	raw, err := json.Marshal(map[string]string{"topic": topic, "title": title, "body": body, "url": url})
	if err != nil {
		return fmt.Errorf("appkit.Notify: %w", err)
	}
	// Written under a name labd ignores, then renamed: labd never sees a
	// half-written file.
	name := fmt.Sprintf("%d-%d", time.Now().UnixNano(), os.Getpid())
	tmp := filepath.Join(outbox, name+".tmp")
	if err := os.WriteFile(tmp, raw, 0o600); err != nil {
		return fmt.Errorf("appkit.Notify: %w", err)
	}
	if err := os.Rename(tmp, filepath.Join(outbox, name+".json")); err != nil {
		os.Remove(tmp)
		return fmt.Errorf("appkit.Notify: %w", err)
	}
	return nil
}
