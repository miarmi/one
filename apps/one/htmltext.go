package main

import (
	"strings"

	"golang.org/x/net/html"
)

// htmlToText turns an HTML mail into plain text. It only reads the markup:
// scripts, styles and the head are dropped, pictures become their alt text,
// and no address in the mail is ever opened.
func htmlToText(src string) string {
	z := html.NewTokenizer(strings.NewReader(src))
	var b strings.Builder
	skip := 0
	pre := 0
	for {
		tt := z.Next()
		switch tt {
		case html.ErrorToken:
			return tidyText(b.String())
		case html.TextToken:
			if skip > 0 {
				continue
			}
			t := string(z.Text())
			if pre == 0 {
				t = collapseSpace(t)
			}
			b.WriteString(t)
		case html.StartTagToken, html.SelfClosingTagToken:
			name, hasAttr := z.TagName()
			n := string(name)
			switch n {
			case "script", "style", "head", "title", "noscript", "template", "xml", "svg", "iframe", "object":
				if tt == html.StartTagToken {
					skip++
				}
			case "br":
				b.WriteString("\n")
			case "pre":
				pre++
				b.WriteString("\n")
			case "li":
				b.WriteString("\n- ")
			case "td", "th":
				b.WriteString(" ")
			case "img":
				if skip == 0 && hasAttr {
					for {
						k, v, more := z.TagAttr()
						if string(k) == "alt" && len(strings.TrimSpace(string(v))) > 0 {
							b.WriteString("[image: " + strings.TrimSpace(string(v)) + "]")
						}
						if !more {
							break
						}
					}
				}
			default:
				if isBlock(n) {
					b.WriteString("\n")
				}
			}
		case html.EndTagToken:
			name, _ := z.TagName()
			n := string(name)
			switch n {
			case "script", "style", "head", "title", "noscript", "template", "xml", "svg", "iframe", "object":
				if skip > 0 {
					skip--
				}
			case "pre":
				if pre > 0 {
					pre--
				}
				b.WriteString("\n")
			default:
				if isBlock(n) {
					b.WriteString("\n")
				}
			}
		}
	}
}

func isBlock(n string) bool {
	switch n {
	case "p", "div", "tr", "table", "ul", "ol", "blockquote", "h1", "h2", "h3", "h4", "h5", "h6", "hr", "section", "article", "header", "footer":
		return true
	}
	return false
}

// collapseSpace folds every run of white space into one space.
func collapseSpace(s string) string {
	var b strings.Builder
	space := false
	for _, r := range s {
		switch r {
		case ' ', '\t', '\n', '\r', '\f', ' ':
			if !space {
				b.WriteByte(' ')
			}
			space = true
		default:
			b.WriteRune(r)
			space = false
		}
	}
	return b.String()
}

// tidyText trims each line and keeps at most one empty line between blocks.
func tidyText(s string) string {
	lines := strings.Split(strings.ReplaceAll(s, "\r\n", "\n"), "\n")
	var out []string
	blank := 0
	for _, l := range lines {
		l = strings.TrimSpace(l)
		if l == "" {
			blank++
			if blank > 1 || len(out) == 0 {
				continue
			}
		} else {
			blank = 0
		}
		out = append(out, l)
	}
	return strings.TrimSpace(strings.Join(out, "\n"))
}
