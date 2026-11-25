package main

import (
	"fmt"
	"regexp"
	"strings"
	"unicode"
	"unicode/utf8"

	"lab/apps/one/repo"
)

// What an agent reads of a message (SPEC 11, 29). The owner's page shows the
// stored text as it is; an agent gets text with no markup, with quoted
// earlier messages left out when the thread already holds them, and capped.

const (
	agentTextCap = 4000 // characters of one message an agent gets in get_item
	previewChars = 120  // the preview of a queue line
	// quoteMarker takes the place of a quote that was cut.
	quoteMarker = "[a quoted earlier message was left out; the thread holds it]"
)

var (
	// Tags that make a text an HTML document (layout), and tags and entities
	// that may be left in a text that is otherwise plain.
	blockTagRe  = regexp.MustCompile(`(?i)</?(html|head|body|div|p|br|table|tr|td|ul|ol|li|h[1-6]|blockquote|hr|style|script|center|xml)(\s[^<>]*)?/?>|<!doctype|<!--`)
	inlineTagRe = regexp.MustCompile(`(?i)</?(span|a|b|i|u|strong|em|font|img|small|big|sup|sub|o:p|meta|link|title|pre|code)(\s[^<>]*)?/?>|&(nbsp|amp|lt|gt|quot|apos|#[0-9]+|#x[0-9a-f]+);`)
)

// markupFree makes sure a text holds no markup: a text that is HTML (a mail
// stored before the conversion was fixed, a plain part that holds HTML, or
// HTML that was escaped twice) is turned into text the way new mails are.
func markupFree(s string) string {
	switch {
	case blockTagRe.MatchString(s):
		return htmlToText(s)
	case inlineTagRe.MatchString(s):
		// Mostly plain: keep its lines.
		return htmlToText("<pre>" + s + "</pre>")
	}
	return s
}

// startsWithMarkup says a text begins with a layout tag or a document type:
// the whole text is a page, not a plain text that mentions a tag.
func startsWithMarkup(s string) bool {
	loc := blockTagRe.FindStringIndex(strings.TrimSpace(s))
	return loc != nil && loc[0] == 0
}

// --- cutting quotes ---

var (
	replyHeadRe = regexp.MustCompile(`(?i)^(on|dne|am|le)\s.{4,}\s(wrote|napsal|napsala|napsal\(a\)|schrieb|a écrit)\s*:?\s*$`)
	replyStart  = regexp.MustCompile(`(?i)^(on|dne|am|le)\s`)
	headLineRe  = regexp.MustCompile(`(?i)^(from|sent|date|to|cc|bcc|subject|importance|od|odesláno|datum|komu|kopie|předmět|důležitost)\s*:`)
	fromLineRe  = regexp.MustCompile(`(?i)^(from|od)\s*:`)
	oneLineHead = regexp.MustCompile(`(?i)^(from|od)\s*:.*\b(sent|date|odesláno|datum)\s*:.*\b(to|komu)\s*:`)
	origMsgRe   = regexp.MustCompile(`(?i)^-{2,}\s*(original message|původní zpráva|ursprüngliche nachricht)\s*-{2,}$`)
	rulerRe     = regexp.MustCompile(`^[_\-=]{5,}$`)
)

// alnum folds a text to its letters and digits, lower case, so that the same
// words match whatever markup or line breaks came between them.
func alnum(s string) string {
	var b strings.Builder
	for _, r := range s {
		if unicode.IsLetter(r) || unicode.IsDigit(r) {
			b.WriteRune(unicode.ToLower(r))
		}
	}
	return b.String()
}

// replyHeaderLines is how many lines from i make a reply header like
// "On <date> <name> wrote:" (some programs wrap it), or 0.
func replyHeaderLines(lines []string, i int) int {
	if !replyStart.MatchString(strings.TrimSpace(lines[i])) {
		return 0
	}
	s := ""
	for k := 0; k < 3 && i+k < len(lines); k++ {
		s = strings.TrimSpace(s + " " + strings.TrimSpace(lines[i+k]))
		if replyHeadRe.MatchString(s) {
			return k + 1
		}
	}
	return 0
}

// outlookHeaderLines is how many lines from i make the "From: / Sent: / To: /
// Subject:" block of a mail client, or 0.
func outlookHeaderLines(lines []string, i int) int {
	first := strings.TrimSpace(lines[i])
	if !fromLineRe.MatchString(first) {
		return 0
	}
	if oneLineHead.MatchString(first) {
		return 1
	}
	n, keys := 0, 0
	for j := i; j < len(lines) && j < i+10; j++ {
		l := strings.TrimSpace(lines[j])
		if headLineRe.MatchString(l) {
			keys++
			n = j - i + 1
		} else if l != "" {
			break
		}
	}
	if keys >= 3 {
		return n
	}
	return 0
}

// quoteSample is the start of the words a quote holds, folded, without the
// header lines and quote marks that come first.
func quoteSample(lines []string) string {
	var words []string
	for _, l := range lines {
		l = strings.TrimSpace(strings.TrimLeft(strings.TrimSpace(l), "> \t"))
		if l == "" || (len(words) == 0 && (headLineRe.MatchString(l) || rulerRe.MatchString(l))) {
			continue
		}
		words = append(words, l)
		if utf8.RuneCountInString(alnum(strings.Join(words, " "))) >= 60 {
			break
		}
	}
	s := []rune(alnum(strings.Join(words, " ")))
	if len(s) > 60 {
		s = s[:60]
	}
	return string(s)
}

// held says whether one of the other messages of the thread holds the quote.
// A quote too short to tell is never taken to be held.
func held(quote []string, others []string) bool {
	s := quoteSample(quote)
	if utf8.RuneCountInString(s) < 20 {
		return false
	}
	for _, o := range others {
		if strings.Contains(o, s) {
			return true
		}
	}
	return false
}

// addMarker puts the line that says a quote was left out after the lines so
// far, without empty lines before it.
func addMarker(out []string) []string {
	for len(out) > 0 && strings.TrimSpace(out[len(out)-1]) == "" {
		out = out[:len(out)-1]
	}
	return append(out, quoteMarker)
}

// quotedRunEnd is the line after the run of ">" lines that starts at i.
func quotedRunEnd(lines []string, i int) int {
	j := i
	for j < len(lines) && (strings.HasPrefix(strings.TrimSpace(lines[j]), ">") || strings.TrimSpace(lines[j]) == "") {
		j++
	}
	for j > i && strings.TrimSpace(lines[j-1]) == "" {
		j--
	}
	return j
}

// cutQuotes takes the quoted earlier messages out of a text, when the thread
// holds them (others are the other messages of the thread folded with alnum).
// A line saying so takes the place of each cut. It says whether it cut.
func cutQuotes(text string, others []string) (string, bool) {
	lines := strings.Split(strings.ReplaceAll(text, "\r\n", "\n"), "\n")
	var out []string
	cut := false
	for i := 0; i < len(lines); {
		l := strings.TrimSpace(lines[i])
		if n := replyHeaderLines(lines, i); n > 0 {
			// Quoted with ">" marks: the header goes with the marked lines, and
			// anything written below them stays.
			if k := i + n; k < len(lines) && strings.HasPrefix(strings.TrimSpace(lines[k]), ">") {
				j := quotedRunEnd(lines, k)
				if held(lines[k:j], others) {
					out = addMarker(out)
					cut = true
					i = j
					continue
				}
			} else if held(lines[i+n:], others) {
				out = addMarker(out)
				cut = true
				break
			}
		}
		if n := outlookHeaderLines(lines, i); n > 0 && held(lines[i+n:], others) {
			// The separator line before the block goes with it.
			for len(out) > 0 && (strings.TrimSpace(out[len(out)-1]) == "" || rulerRe.MatchString(strings.TrimSpace(out[len(out)-1])) || origMsgRe.MatchString(strings.TrimSpace(out[len(out)-1]))) {
				out = out[:len(out)-1]
			}
			out = addMarker(out)
			cut = true
			break
		}
		if origMsgRe.MatchString(l) && i+1 < len(lines) {
			rest := lines[i+1:]
			for len(rest) > 0 && strings.TrimSpace(rest[0]) == "" {
				rest = rest[1:]
			}
			if len(rest) > 0 {
				if n := outlookHeaderLines(rest, 0); n > 0 && held(rest[n:], others) {
					out = addMarker(out)
					cut = true
					break
				}
			}
		}
		if strings.HasPrefix(l, ">") {
			j := quotedRunEnd(lines, i)
			if held(lines[i:j], others) {
				out = addMarker(out)
				cut = true
				i = j
				continue
			}
		}
		out = append(out, lines[i])
		i++
	}
	return strings.TrimSpace(strings.Join(out, "\n")), cut
}

// capText keeps the first max characters and says how many it left out.
func capText(s string, max int) (string, int) {
	r := []rune(s)
	if len(r) <= max {
		return s, 0
	}
	return strings.TrimRightFunc(string(r[:max]), unicode.IsSpace), len(r) - max
}

// --- one thread ---

// threadTexts holds what is needed to cut the quotes of one item's messages.
type threadTexts struct {
	msgs   []repo.Message
	folded []string // alnum of each message's text without markup
}

func newThreadTexts(msgs []repo.Message) threadTexts {
	t := threadTexts{msgs: msgs, folded: make([]string, len(msgs))}
	for i, m := range msgs {
		t.folded[i] = alnum(markupFree(m.Text))
	}
	return t
}

// others are the folded texts of every message but the one at index i.
func (t threadTexts) others(i int) []string {
	out := make([]string, 0, len(t.msgs))
	for j, f := range t.folded {
		if j != i {
			out = append(out, f)
		}
	}
	return out
}

// reading is the text of message i as an agent reads it: no markup, quotes
// the thread holds cut. cut says whether a quote was left out.
func (t threadTexts) reading(i int) (text string, cut bool) {
	return cutQuotes(markupFree(t.msgs[i].Text), t.others(i))
}

// newChars counts the characters in the messages of others that the thread
// holds once markup, quotes and runs of white space are gone. A short matter
// gets no summary (SPEC 11).
func (t threadTexts) newChars() int {
	n := 0
	for i, m := range t.msgs {
		if m.SentByMe {
			continue
		}
		text, _ := t.reading(i)
		n += utf8.RuneCountInString(strings.Join(strings.Fields(text), " "))
	}
	return n
}

// previewOf is the short clean text of a queue line: no markup, no quoted
// lines, one line of white space.
func previewOf(raw string) string {
	var keep []string
	for _, l := range strings.Split(markupFree(raw), "\n") {
		if strings.HasPrefix(strings.TrimSpace(l), ">") {
			continue
		}
		keep = append(keep, l)
	}
	return snippet(oneLine(strings.Join(keep, " "), 100000), previewChars)
}

func leftOutLine(n int, id int64) string {
	return fmt.Sprintf("[%d more characters left out; read this message in full with message %d]", n, id)
}
