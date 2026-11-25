package main

import (
	"context"
	"encoding/json"
	"net/http"
	"slices"
	"strings"

	"lab/apps/one/repo"
)

// SPEC 29: recording the owner's feedback on how an item was handled
// (record_feedback). Feedback is what the owner said, in his words: an agent
// records it and never writes its own judgement as feedback. It is kept on the
// item as an event of type feedback, shown in its thread, and given, next to
// the notes, to every later review of the same sender or the same matter.
// It changes nothing else: no stage, no rule.

const (
	feedbackMaxChars = 2000
	feedbackScanned  = 300 // newest feedback looked at when a review asks
	feedbackShownMax = 10
)

const feedbackRule = "Feedback is what the owner said about how items like this were handled, in his own words. Follow it like a note he wrote, before your own judgement. It is his, never an agent's judgement."

var feedbackTools = []mcpTool{
	{"record_feedback", "Record the owner's feedback",
		"Record what the owner said about how an item was handled (\"that was wrong\", \"this kind I want to see\"). Record only what the owner said, in his words; never the agent's own judgement. text is his words (required, up to 2000 characters, as he said them, not summarised into your opinion). Optional about is the id of the decision it concerns, as list_recent_decisions gives it (an event id of this item). It is kept on the item, shown in its thread, and given to every later review of the same sender or the same matter, in get_item, get_context and the review prompt, next to the notes. It changes nothing else: no stage, no rule, no proposal; to change how mail is handled use propose_rule or propose_skip_rule, which the owner must approve. Text that is not the owner's words must not be recorded.",
		schema(map[string]any{"id": itemT, "text": strT, "about": map[string]any{"type": "integer", "description": "The id of the decision it concerns, as list_recent_decisions gives it."}, "agent": agentT}, "id", "text"), hints("Record the owner's feedback", false, false, false)},
}

func init() { mcpTools = append(mcpTools, feedbackTools...) }

func (a *app) callFeedbackTool(ctx context.Context, name string, args json.RawMessage) (text string, isErr, known bool) {
	if name != "record_feedback" {
		return "", false, false
	}
	var in struct {
		ID    json.RawMessage `json:"id"`
		Text  string          `json:"text"`
		About int64           `json:"about"`
		Agent string          `json:"agent"`
	}
	if err := json.Unmarshal(args, &in); err != nil {
		return "The arguments must be a JSON object.", true, true
	}
	ref, ok := itemRef(in.ID)
	if !ok {
		return "id must be an item number like Q-0012.", true, true
	}
	b, _ := json.Marshal(map[string]any{"agent": in.Agent, "text": in.Text, "about": in.About})
	text, isErr = a.callAPI(ctx, "POST", "/api/items/"+ref+"/feedback", b)
	return text, isErr, true
}

// --- feedback ---

type feedbackData struct {
	Text    string   `json:"text"`
	About   int64    `json:"about,omitempty"`
	Senders []string `json:"senders,omitempty"`
	Matter  string   `json:"matter,omitempty"`
}

type appliedFeedback struct {
	Ev      repo.Event
	Data    feedbackData
	Because string
}

// matterOfItem is the matter an item is about: the one it was joined by, or
// else the one the patterns find in its subject and newest text.
func (a *app) matterOfItem(ctx context.Context, q repo.Q, it repo.Item) string {
	if links, err := q.LinksOfItem(ctx, it.ID); err == nil {
		for _, kv := range links {
			if kv[0] == matterLinkKind {
				return kv[1]
			}
		}
	}
	if ps := a.matterPatternsFor(ctx, q); len(ps) > 0 {
		return matterKey(ps, it.Subject, q.NewestText(ctx, it.ID, matterTextScanSize))
	}
	return ""
}

// feedbackThatApplies lists the feedback the owner gave on this item, on items
// from the same sender and on items about the same matter, newest first. Only
// feedback of the item's own test run counts, so a test never reads real
// feedback and real work never reads a test's.
func (a *app) feedbackThatApplies(ctx context.Context, q repo.Q, it repo.Item) []appliedFeedback {
	keys, err := a.senderKeysOf(ctx, q, it.ID)
	if err != nil {
		return nil
	}
	matter := a.matterOfItem(ctx, q, it)
	evs, err := q.FeedbackEvents(ctx, it.TestRun, feedbackScanned)
	if err != nil {
		return nil
	}
	var out []appliedFeedback
	for _, e := range evs {
		var d feedbackData
		if json.Unmarshal([]byte(e.Data), &d) != nil || strings.TrimSpace(d.Text) == "" {
			continue
		}
		why := ""
		switch {
		case e.Item == it.ID:
			why = "recorded on this item"
		case matter != "" && d.Matter == matter:
			why = "about the same matter"
		default:
			for _, s := range d.Senders {
				if slices.Contains(keys.addrs, s) {
					why = "about the same sender " + s
					break
				}
			}
		}
		if why == "" {
			continue
		}
		out = append(out, appliedFeedback{e, d, why})
		if len(out) == feedbackShownMax {
			break
		}
	}
	return out
}

func feedbackJSON(list []appliedFeedback) []map[string]any {
	out := []map[string]any{}
	for _, f := range list {
		m := map[string]any{"feedback_id": f.Ev.ID, "item": repo.Number(f.Ev.Number), "at": isoTime(f.Ev.At), "recorded_by": actorLabel(f.Ev.Actor),
			"text": f.Data.Text, "applies_because": f.Because}
		if f.Data.About != 0 {
			m["about_decision"] = f.Data.About
		}
		out = append(out, m)
	}
	return out
}

func (a *app) apiRecordFeedback(w http.ResponseWriter, r *http.Request) {
	a.write(func(ctx context.Context, x actor, ref string, b agentBody) (result, error) {
		return a.recordFeedback(ctx, x, ref, b.Text, b.About)
	})(w, r)
}

func (a *app) recordFeedback(ctx context.Context, x actor, ref, text string, about int64) (result, error) {
	var res result
	if err := onlyAgent(x); err != nil {
		return res, a.logRefusal(ctx, x, 0, err)
	}
	text, err := checkField("text", text, feedbackMaxChars, true)
	if err != nil {
		return res, err
	}
	now := a.now().Unix()
	err = a.store.Update(ctx, func(tx *repo.Tx) error {
		it, err := a.findItemW(ctx, tx.Q, ref)
		if err != nil {
			return err
		}
		res.Item = it
		if about != 0 {
			ev, ok := tx.EventByID(ctx, about)
			if !ok || ev.Item != it.ID || !slices.Contains(repo.DecisionTypes, ev.Type) {
				return bad("about must be the id of a decision on this item, as list_recent_decisions gives it")
			}
		}
		d := feedbackData{Text: text, About: about, Matter: a.matterOfItem(ctx, tx.Q, it)}
		if keys, err := a.senderKeysOf(ctx, tx.Q, it.ID); err == nil {
			d.Senders = keys.addrs[:min(len(keys.addrs), 10)]
		}
		return tx.InsertEvent(ctx, now, it.ID, x.id(), "feedback", d)
	})
	return res, err
}
