package main

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"regexp"
	"slices"
	"sort"
	"strconv"
	"strings"
	"time"

	"lab/apps/one/repo"
	"lab/platform/appkit"
)

// The settings of the model (SPEC 31) and what the Claude row shows. They are
// written only from this signed-in page; nothing under /api/ reaches them.

type claudeField struct {
	Key, Label, Hint, Value, Kind string // Kind: text, number, money or time
	Datalist                      bool
}

type priceRow struct {
	Model   string
	In, Out string
}

type claudeSettingsView struct {
	Fields    []claudeField
	Models    []string
	Prices    []priceRow
	Confirmed bool
	Source    string
}

func fmtUSD(f float64) string { return strconv.FormatFloat(f, 'f', -1, 64) }

func (a *app) claudeSettingsFor(ctx context.Context) claudeSettingsView {
	_, _, at := a.nightAt(ctx)
	v := claudeSettingsView{Models: knownModels, Source: priceSource}
	v.Fields = []claudeField{
		{Key: keyReviewModel, Label: "Review model", Hint: "The model that reads each new item.", Value: a.reviewModel(ctx), Kind: "text", Datalist: true},
		{Key: keyDailyLimit, Label: "Daily limit", Hint: "US dollars a day for reading items. The day turns at midnight in Prague.", Value: fmtUSD(a.dailyLimit(ctx)), Kind: "money"},
		{Key: keyNightAt, Label: "Night check at", Hint: "Time of day, Prague.", Value: at, Kind: "time"},
		{Key: keyNightModel, Label: "Night check model", Hint: "The model that looks over the last 24 hours.", Value: a.nightModel(ctx), Kind: "text", Datalist: true},
		{Key: keyNightLimit, Label: "Night check limit", Hint: "US dollars a night.", Value: fmtUSD(a.nightLimit(ctx)), Kind: "money"},
		{Key: keySkipMin, Label: "Skips before a sender rule", Hint: "A rule for a sender can be proposed only after this many skips.", Value: strconv.Itoa(a.skipMin(ctx)), Kind: "number"},
	}
	t := a.priceTable(ctx)
	v.Confirmed = t.Confirmed
	var names []string
	for m := range t.Prices {
		names = append(names, m)
	}
	sort.Strings(names)
	for _, m := range names {
		p := t.Prices[m]
		v.Prices = append(v.Prices, priceRow{Model: m, In: fmtUSD(p.In), Out: fmtUSD(p.Out)})
	}
	// the names offered for a model field: the known ones and every model in the price table
	v.Models = append([]string(nil), knownModels...)
	for _, m := range names {
		if !slices.Contains(v.Models, m) {
			v.Models = append(v.Models, m)
		}
	}
	return v
}

const priceSource = "From the Anthropic price list on 20 Nov 2026. Change them here when the price list changes."

var modelName = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._:-]{0,79}$`)

// validClaudeValue is the one check of a value of the model's settings: Save
// and a proposal to change one both use it, so a value is valid or refused the
// same way, never cut to fit.
func validClaudeValue(key, val string) bool {
	switch key {
	case keyReviewModel, keyNightModel:
		return modelName.MatchString(val)
	case keyDailyLimit, keyNightLimit:
		n, err := strconv.ParseFloat(val, 64)
		return err == nil && n >= 0 && n <= 1000
	case keyNightAt:
		_, err := time.Parse("15:04", val)
		return err == nil
	case keySkipMin:
		n, err := strconv.Atoi(val)
		return err == nil && n >= 1 && n <= 1000
	}
	return false
}

// writeClaudeChange is the one way a value of the model's settings is written and recorded.
func writeClaudeChange(ctx context.Context, tx *repo.Tx, key, label, from, to string, now time.Time) error {
	if err := tx.SetSettingBy(ctx, key, to, "owner", now); err != nil {
		return err
	}
	return tx.InsertEvent(ctx, now.Unix(), 0, "owner", "setting_changed", map[string]any{
		"setting": key, "label": label, "from": from, "to": to, "from_text": from, "to_text": to})
}

// saveClaudeSettings is Save on the Claude settings. Every value is checked
// first; when one is not valid nothing is saved.
func (a *app) saveClaudeSettings(w http.ResponseWriter, r *http.Request) {
	r.Body = http.MaxBytesReader(w, r.Body, 8<<10)
	ctx := r.Context()
	if err := r.ParseForm(); err != nil {
		http.Error(w, "The form could not be read.", http.StatusBadRequest)
		return
	}
	cur := a.claudeSettingsFor(ctx)
	type change struct{ key, label, from, to string }
	var changes []change
	for _, f := range cur.Fields {
		raw := r.PostForm[f.Key]
		if len(raw) == 0 {
			continue
		}
		val := strings.TrimSpace(raw[0])
		if !validClaudeValue(f.Key, val) {
			a.renderSettings(w, r, http.StatusBadRequest, "", fmt.Sprintf("%s: that is not a value this setting can have. Nothing was saved.", f.Label))
			return
		}
		if val != f.Value {
			changes = append(changes, change{f.Key, f.Label, f.Value, val})
		}
	}
	now := a.now()
	err := a.store.Update(ctx, func(tx *repo.Tx) error {
		for _, c := range changes {
			if err := writeClaudeChange(ctx, tx, c.key, c.label, c.from, c.to, now); err != nil {
				return err
			}
		}
		return nil
	})
	if err != nil {
		appkit.Error("saving the Claude settings", err)
		http.Error(w, "The settings could not be saved. Try again.", http.StatusInternalServerError)
		return
	}
	if len(changes) > 0 {
		a.wakeReview()
	}
	http.Redirect(w, r, "/settings?n=saved#h-claude", http.StatusSeeOther)
}

// savePrices is Save prices: The owner has looked at them, so they count as confirmed.
func (a *app) savePrices(w http.ResponseWriter, r *http.Request) {
	r.Body = http.MaxBytesReader(w, r.Body, 8<<10)
	ctx := r.Context()
	if err := r.ParseForm(); err != nil {
		http.Error(w, "The form could not be read.", http.StatusBadRequest)
		return
	}
	t := priceTable{Prices: map[string]modelPrice{}, Confirmed: true}
	for i, m := range r.PostForm["model"] {
		m = strings.TrimSpace(m)
		if m == "" {
			continue
		}
		if !modelName.MatchString(m) || i >= len(r.PostForm["in"]) || i >= len(r.PostForm["out"]) {
			a.renderSettings(w, r, http.StatusBadRequest, "", "Prices: a model name is not valid. Nothing was saved.")
			return
		}
		in, e1 := strconv.ParseFloat(strings.TrimSpace(r.PostForm["in"][i]), 64)
		out, e2 := strconv.ParseFloat(strings.TrimSpace(r.PostForm["out"][i]), 64)
		if e1 != nil || e2 != nil || in < 0 || out < 0 || in > 10000 || out > 10000 {
			a.renderSettings(w, r, http.StatusBadRequest, "", "Prices: write each price as a number of US dollars per million tokens. Nothing was saved.")
			return
		}
		t.Prices[m] = modelPrice{In: in, Out: out}
	}
	if len(t.Prices) == 0 {
		a.renderSettings(w, r, http.StatusBadRequest, "", "Prices: at least one model needs a price. Nothing was saved.")
		return
	}
	now := a.now()
	err := a.store.Update(ctx, func(tx *repo.Tx) error {
		if err := tx.SetSettingBy(ctx, keyPrices, string(priceJSON(t)), "owner", now); err != nil {
			return err
		}
		return tx.InsertEvent(ctx, now.Unix(), 0, "owner", "setting_changed", map[string]any{
			"setting": keyPrices, "label": "Prices", "from": "", "to": "saved", "from_text": "", "to_text": "saved"})
	})
	if err != nil {
		appkit.Error("saving prices", err)
		http.Error(w, "The prices could not be saved. Try again.", http.StatusInternalServerError)
		return
	}
	http.Redirect(w, r, "/settings?n=prices#h-claude", http.StatusSeeOther)
}

// --- the Claude row on Connections ---

func (a *app) claudeRowFor(ctx context.Context, v *claudeView) {
	if a.claudeConfigured() {
		v.Mode = "ONE calls " + a.reviewModel(ctx) + " itself"
		v.Spend = fmt.Sprintf("Today %s of %s", usd(a.spentToday(ctx)), usd(limitMicro(a.dailyLimit(ctx))))
		if t := a.store.LastUsageAt(ctx, "review"); t > 0 {
			v.LastRan = a.clock(t)
		}
		v.Status, v.Cls = statusOK, "st-ok"
		// while the review is paused the row does not say OK (UX review m7)
		if a.limitReached(ctx) {
			v.Mode += ", paused for today"
			v.Status, v.Cls = statusStale, "st-stale"
		} else if _, down := a.downStateOf(ctx); down {
			v.Mode += ", paused: cannot be reached"
			v.Status, v.Cls = statusStale, "st-stale"
		}
	} else if v.LastCall != "" {
		v.Mode = "An outside agent is at work through the connector"
	}
	if t := a.store.LastUsageAt(ctx, "night"); t > 0 {
		v.NightRan = a.clock(t)
	}
	if ns, err := a.store.RecentNightChecks(ctx, 5); err == nil {
		for _, n := range ns {
			line := fmt.Sprintf("%s: %d messages, %d brought back", n.Day, n.Checked, n.BroughtBack)
			if n.Status == "failed" {
				line = n.Day + ": could not run: " + strings.TrimSuffix(n.Error, ".")
			} else if n.Error != "" {
				line += ". " + strings.TrimSuffix(n.Error, ".") + "." // what the night could not cover
			}
			v.Nights = append(v.Nights, line)
		}
	}
}

func priceJSON(t priceTable) []byte { b, _ := json.Marshal(t); return b }
