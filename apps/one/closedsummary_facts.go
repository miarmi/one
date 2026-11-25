package main

import (
	"context"
	"fmt"
	"net/textproto"
	"regexp"
	"sort"
	"strings"
	"time"
	"unicode"

	"lab/apps/one/repo"
)

// What makes an item one the owner should look at again (SPEC 27, "the ones Claude
// was least sure of"). Never Claude's own reason, which may say "no deadline,
// no payment" and would match words it denies: only facts found in the item
// and in what ONE knows. Each fact is one plain statement that is shown as
// the doubt line; an item with no strong fact gets no doubt line and stands
// last. The weak fact (closed with no note of the owner's covering it) is true of
// most things Claude closes, so it is only added to an item that has a strong
// fact already.
type fact struct {
	w    int
	text string
}

const strongFact = 2

var (
	// A money amount is a number next to a currency sign or word. Signs and
	// "Kč"/"CZK"/"EUR"/"USD"/"GBP" may touch the number; the plain-letter "kc"
	// needs a space (so a token like "9kC" is no amount). Nothing letter-like
	// may stand directly before the number or after the currency.
	moneyAfter  = regexp.MustCompile(`(?i)(?:^|[^\p{L}\d.,])(\d(?:[\d \x{00a0}.,]*\d)?\s?(?:kč|czk|eur|usd|gbp)(?:[^\p{L}]|$)|\d(?:[\d \x{00a0}.,]*\d)?\s(?:kc)(?:[^\p{L}]|$)|\d[\d \x{00a0}.,]*\d?\s?[€$£])`)
	moneyBefore = regexp.MustCompile(`[€$£]\s?\d[\d.,]*`)
	dateDots    = regexp.MustCompile(`\b(\d{1,2})\.\s?(\d{1,2})\.(?:\s?(\d{4}))?`)
	dateISO     = regexp.MustCompile(`\b(\d{4})-(\d{2})-(\d{2})\b`)
	monthWords  = `(?:january|february|march|april|may|june|july|august|september|october|november|december|jan|feb|mar|apr|jun|jul|aug|sep|sept|oct|nov|dec|ledna|února|března|dubna|května|června|července|srpna|září|října|listopadu|prosince)`
	dayMonth    = regexp.MustCompile(`(?i)\b(\d{1,2})\.?\s(` + monthWords + `)\b`)
	monthDay    = regexp.MustCompile(`(?i)\b(` + monthWords + `)\s(\d{1,2})\b`)
)

var monthNumber = map[string]int{"jan": 1, "ledna": 1, "feb": 2, "února": 2, "mar": 3, "března": 3, "apr": 4, "dubna": 4, "may": 5, "května": 5,
	"jun": 6, "června": 6, "jul": 7, "července": 7, "aug": 8, "srpna": 8, "sep": 9, "sept": 9, "září": 9, "oct": 10, "října": 10, "nov": 11, "listopadu": 11, "dec": 12, "prosince": 12}

func monthOf(w string) int {
	w = strings.ToLower(w)
	if n, ok := monthNumber[w]; ok {
		return n
	}
	if len(w) >= 3 {
		return monthNumber[w[:3]] // january, february ... by their first three letters
	}
	return 0
}

func firstAmount(s string) string {
	for _, ix := range moneyAfter.FindAllStringSubmatchIndex(s, -1) {
		m := strings.TrimSpace(s[ix[2]:ix[3]])
		// the match may have taken one character after the currency
		m = strings.TrimRightFunc(m, func(r rune) bool { return !unicode.IsLetter(r) && !strings.ContainsRune("€$£", r) })
		if m != "" {
			return clean(m, 30)
		}
	}
	if m := moneyBefore.FindString(s); m != "" {
		return clean(strings.TrimSpace(m), 30)
	}
	return ""
}

// firstDate is the first date in s that is today or still ahead (in loc), as
// it is written there. A date that has passed is no reason to look again.
func firstDate(s string, now time.Time, loc *time.Location) string {
	today := time.Date(now.In(loc).Year(), now.In(loc).Month(), now.In(loc).Day(), 0, 0, 0, 0, loc)
	ahead := func(y, mo, d int) bool {
		if mo < 1 || mo > 12 || d < 1 || d > 31 {
			return false
		}
		return !time.Date(y, time.Month(mo), d, 0, 0, 0, 0, loc).Before(today)
	}
	for _, ix := range dateDots.FindAllStringSubmatchIndex(s, -1) {
		after := byte(' ')
		if ix[1] < len(s) {
			after = s[ix[1]]
		}
		if after >= '0' && after <= '9' { // 3.5.1 is a version, not a date
			continue
		}
		var d, mo int
		fmt.Sscan(s[ix[2]:ix[3]], &d)
		fmt.Sscan(s[ix[4]:ix[5]], &mo)
		y := today.Year()
		if ix[6] >= 0 {
			fmt.Sscan(s[ix[6]:ix[7]], &y)
		}
		if ahead(y, mo, d) {
			return clean(strings.TrimSpace(s[ix[0]:ix[1]]), 30)
		}
	}
	for _, m := range dateISO.FindAllStringSubmatch(s, -1) {
		var y, mo, d int
		fmt.Sscan(m[1], &y)
		fmt.Sscan(strings.TrimLeft(m[2], "0"), &mo)
		fmt.Sscan(strings.TrimLeft(m[3], "0"), &d)
		if ahead(y, mo, d) {
			return m[0]
		}
	}
	for _, m := range dayMonth.FindAllStringSubmatch(s, -1) {
		var d int
		fmt.Sscan(m[1], &d)
		if ahead(today.Year(), monthOf(m[2]), d) {
			return clean(m[0], 30)
		}
	}
	for _, m := range monthDay.FindAllStringSubmatch(s, -1) {
		var d int
		fmt.Sscan(m[2], &d)
		if ahead(today.Year(), monthOf(m[1]), d) {
			return clean(m[0], 30)
		}
	}
	return ""
}

// automaticSender says whether mail is from a shop or service, not a person:
// list or auto-generated headers, or an address that names a function.
func automaticSender(hdr textproto.MIMEHeader, sender string) bool {
	if hdr != nil {
		if hdr.Get("List-Id") != "" || hdr.Get("List-Unsubscribe") != "" || hdr.Get("List-Post") != "" {
			return true
		}
		if v := strings.ToLower(strings.TrimSpace(hdr.Get("Auto-Submitted"))); v != "" && v != "no" {
			return true
		}
		switch strings.ToLower(strings.TrimSpace(hdr.Get("Precedence"))) {
		case "bulk", "list", "junk", "auto_reply":
			return true
		}
	}
	local := strings.ToLower(sender)
	if i := strings.Index(local, "@"); i >= 0 {
		local = local[:i]
	}
	local = strings.NewReplacer("-", "", "_", "", ".", "").Replace(local)
	for _, w := range []string{"noreply", "donotreply", "notification", "notify", "mailer", "postmaster", "newsletter", "info", "doklady", "faktury", "objednavky", "billing", "invoice", "order", "support", "service", "shop", "news", "alert", "hello", "team", "kontakt", "podpora", "automat"} {
		if strings.HasPrefix(local, w) {
			return true
		}
	}
	return false
}

// factsOf finds the facts about one closed item. kind is the closing's kind,
// note whether a note of the owner's covers it.
func (a *app) factsOf(ctx context.Context, lr repo.ListRow, kind string, note bool) []fact {
	var out []fact
	if kind == "old" {
		out = append(out, fact{3, "it mattered once: its date has passed"})
	}
	msgs, _, _ := a.store.MessagesSince(ctx, lr.ID, 0)
	var theirs []repo.Message
	for _, m := range msgs {
		if !m.SentByMe {
			theirs = append(theirs, m)
		}
	}
	if len(theirs) > 0 {
		last := theirs[0] // newest first
		if lr.Kind == "chat" || last.Network != "mail" {
			out = append(out, fact{2, "a chat message from a person"})
		} else {
			h := a.store.SenderHistoryOf(ctx, last.Sender, last.ID, last.ReceivedAt)
			if h.OwnerWrote {
				out = append(out, fact{3, "you have written to this sender before"})
			}
			if h.Earlier == 0 {
				out = append(out, fact{2, "first message from this sender"})
			}
		}
		person := true // a chat message is from a person; mail only when it is not a shop's or a service's
		if lr.Kind != "chat" && last.Network == "mail" {
			person = !automaticSender(a.originalHeaders(ctx, a.store.Q, last.ID), last.Sender)
		}
		withFile := false
		for _, m := range theirs {
			withFile = withFile || m.HasAttachments
		}
		if withFile && person {
			out = append(out, fact{2, "carries a file"})
		}
		var amount, date string
		for _, m := range theirs {
			text := m.Subject + " " + m.Text
			if len(text) > 6000 {
				text = text[:6000]
			}
			if amount == "" {
				amount = firstAmount(text)
			}
			if date == "" {
				date = firstDate(text, a.now(), a.loc)
			}
		}
		if amount != "" {
			out = append(out, fact{2, "amount " + amount})
		}
		if date != "" {
			out = append(out, fact{2, "date " + date})
		}
	}
	// Absences ("no note of yours") are not reasons, and "sent to you
	// directly" is true of almost all private mail: neither is listed.
	sort.SliceStable(out, func(i, j int) bool { return out[i].w > out[j].w })
	if len(out) > 2 {
		out = out[:2] // the two strongest; the model gets no more than the line shows
	}
	return out
}

// doubtOfFacts is the doubt line and the score: nothing without a strong fact.
func doubtOfFacts(fs []fact) (score int, doubt string, texts []string) {
	strong := false
	for _, f := range fs {
		texts = append(texts, f.text)
		score += f.w
		strong = strong || f.w >= strongFact
	}
	if !strong {
		return 0, "", texts
	}
	var parts []string
	for i, f := range fs {
		if i == 2 {
			break
		}
		parts = append(parts, f.text)
	}
	d := strings.Join(parts, "; ")
	return score, strings.ToUpper(d[:1]) + d[1:], texts
}

// cleanSubject is what an item is: its subject without reply and forward
// marks and runs of space, cut at a word.
func cleanSubject(s string) string {
	s = strings.Join(strings.Fields(s), " ")
	for {
		low := strings.ToLower(s)
		cut := false
		for _, p := range []string{"re:", "fw:", "fwd:", "odp:", "aw:"} {
			if strings.HasPrefix(low, p) {
				s, cut = strings.TrimSpace(s[len(p):]), true
				break
			}
		}
		if !cut {
			break
		}
	}
	return cutAtWord(clean(s, 400), 80)
}

func cutAtWord(s string, n int) string {
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	cut := string(r[:n])
	if i := strings.LastIndex(cut, " "); i > n/2 {
		cut = cut[:i]
	}
	return strings.TrimRight(cut, " ,;:-") + "…"
}

var digitRun = regexp.MustCompile(`\d+`)

// doubtInventsNumbers says whether a doubt written by the model holds a
// number that is in none of the item's facts.
func doubtInventsNumbers(doubt string, facts []string) bool {
	all := ""
	for _, f := range facts {
		all += strings.Join(digitRun.FindAllString(f, -1), "")
	}
	for _, n := range digitRun.FindAllString(doubt, -1) {
		if !strings.Contains(all, n) {
			return true
		}
	}
	return false
}

// whatOf is the "what" of an item: its subject; only an item with none (a
// chat) is told by the first words of its last message.
func whatOf(lr repo.ListRow, subject string) string {
	if lr.Kind == "chat" || strings.TrimSpace(subject) == "" {
		if t := snippetOf(lr.Snippet, 60); t != "" {
			return cutAtWord(t, 50)
		}
	}
	if w := cleanSubject(subject); w != "" {
		return w
	}
	return "(no subject)"
}
