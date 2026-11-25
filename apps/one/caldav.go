package main

// A read-only CalDAV client for iCloud calendars, carried over from queue v1
// and changed to hand back the calendar text itself, so ONE expands
// repeating events and keeps versions. Every request goes through the one
// private do method, which allows only PROPFIND, REPORT and OPTIONS: there is
// no way to create, change or delete anything in a calendar from here.

import (
	"bytes"
	"context"
	"encoding/xml"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"
)

// errCalAuth: the calendar server refused the sign-in (HTTP 401 or 403).
var errCalAuth = errors.New("calendar sign-in refused")

// errCalUnreadable: the server answered with something that is not a
// calendar answer.
var errCalUnreadable = errors.New("calendar answer unreadable")

// caldavAllowedSuffixes lists the host suffixes (besides the base address
// itself) that may receive credentials after a redirect.
var caldavAllowedSuffixes = []string{".icloud.com"}

const (
	icloudCalDAV       = "https://caldav.icloud.com"
	caldavMaxBody      = 8 << 20
	caldavMaxRedirects = 5
)

type calDAVClient struct {
	BaseURL string       // e.g. "https://caldav.icloud.com"
	User    string       // Apple ID
	Pass    string       // app-specific password; never logged, never in an error text
	HTTP    *http.Client // nil = a default client with a 20 s timeout
}

// calInfo is one calendar collection found in the account.
type calInfo struct {
	Href  string // absolute address of the calendar collection
	Name  string
	Color string // "#RRGGBB" or ""
}

// calResource is one calendar object as the server holds it.
type calResource struct {
	Href string
	ETag string
	Data string
}

type davHref struct {
	Href string `xml:"href"`
}

type davProp struct {
	DisplayName  string  `xml:"displayname"`
	ETag         string  `xml:"getetag"`
	Principal    davHref `xml:"current-user-principal"`
	Home         davHref `xml:"calendar-home-set"`
	Color        string  `xml:"calendar-color"`
	CalendarData string  `xml:"calendar-data"`
	ResourceType struct {
		Any []struct {
			XMLName xml.Name
		} `xml:",any"`
	} `xml:"resourcetype"`
	Comps *struct {
		Comp []struct {
			Name string `xml:"name,attr"`
		} `xml:"comp"`
	} `xml:"supported-calendar-component-set"`
}

type davResponse struct {
	Href     string `xml:"href"`
	Propstat []struct {
		Status string  `xml:"status"`
		Prop   davProp `xml:"prop"`
	} `xml:"propstat"`
}

type davMultistatus struct {
	Responses []davResponse `xml:"response"`
}

// props returns the properties of the successful (or status-less) propstats.
func (r davResponse) props() []davProp {
	var out []davProp
	for _, ps := range r.Propstat {
		if ps.Status == "" || strings.Contains(ps.Status, " 2") {
			out = append(out, ps.Prop)
		}
	}
	return out
}

func parseMultistatus(data []byte) (*davMultistatus, error) {
	var ms davMultistatus
	if err := xml.Unmarshal(data, &ms); err != nil {
		return nil, errCalUnreadable
	}
	return &ms, nil
}

const propfindPrincipal = `<?xml version="1.0" encoding="utf-8"?>
<d:propfind xmlns:d="DAV:"><d:prop><d:current-user-principal/></d:prop></d:propfind>`

const propfindHome = `<?xml version="1.0" encoding="utf-8"?>
<d:propfind xmlns:d="DAV:" xmlns:c="urn:ietf:params:xml:ns:caldav"><d:prop><c:calendar-home-set/></d:prop></d:propfind>`

const propfindCalendars = `<?xml version="1.0" encoding="utf-8"?>
<d:propfind xmlns:d="DAV:" xmlns:c="urn:ietf:params:xml:ns:caldav" xmlns:a="http://apple.com/ns/ical/"><d:prop><d:displayname/><d:resourcetype/><c:supported-calendar-component-set/><a:calendar-color/></d:prop></d:propfind>`

// Calendars discovers the principal and the calendar home and lists the
// collections that hold events (VEVENT), shared calendars included.
func (c *calDAVClient) Calendars(ctx context.Context) ([]calInfo, error) {
	start := strings.TrimRight(c.BaseURL, "/") + "/"
	final, data, err := c.do(ctx, "PROPFIND", start, "0", []byte(propfindPrincipal))
	if err != nil {
		return nil, err
	}
	principal, err := firstHref(final, data, func(p davProp) string { return p.Principal.Href })
	if err != nil {
		return nil, fmt.Errorf("%w: no user principal found", errCalUnreadable)
	}
	final, data, err = c.do(ctx, "PROPFIND", principal, "0", []byte(propfindHome))
	if err != nil {
		return nil, err
	}
	home, err := firstHref(final, data, func(p davProp) string { return p.Home.Href })
	if err != nil {
		return nil, fmt.Errorf("%w: no calendar home found", errCalUnreadable)
	}
	final, data, err = c.do(ctx, "PROPFIND", home, "1", []byte(propfindCalendars))
	if err != nil {
		return nil, err
	}
	ms, err := parseMultistatus(data)
	if err != nil {
		return nil, err
	}
	var cals []calInfo
	for _, r := range ms.Responses {
		var cal calInfo
		isCal, hasEvents := false, true
		for _, p := range r.props() {
			for _, rt := range p.ResourceType.Any {
				if strings.EqualFold(rt.XMLName.Local, "calendar") {
					isCal = true
				}
			}
			if p.Comps != nil && len(p.Comps.Comp) > 0 {
				hasEvents = false
				for _, comp := range p.Comps.Comp {
					if strings.EqualFold(strings.TrimSpace(comp.Name), "VEVENT") {
						hasEvents = true
					}
				}
			}
			if n := strings.TrimSpace(p.DisplayName); n != "" {
				cal.Name = n
			}
			if col := normalizeCalColor(p.Color); col != "" {
				cal.Color = col
			}
		}
		if !isCal || !hasEvents {
			continue
		}
		href, err := resolveHref(final, r.Href)
		if err != nil {
			continue
		}
		cal.Href = href
		cals = append(cals, cal)
	}
	return cals, nil
}

// Resources returns the calendar objects of one calendar that have an event
// overlapping [from, to), as the server holds them (repeating events not
// expanded: ONE does that itself).
func (c *calDAVClient) Resources(ctx context.Context, calendarHref string, from, to time.Time) ([]calResource, error) {
	const layout = "20060102T150405Z"
	f, t := from.UTC().Format(layout), to.UTC().Format(layout)
	body := `<?xml version="1.0" encoding="utf-8"?>
<c:calendar-query xmlns:d="DAV:" xmlns:c="urn:ietf:params:xml:ns:caldav">
<d:prop><d:getetag/><c:calendar-data/></d:prop>
<c:filter><c:comp-filter name="VCALENDAR"><c:comp-filter name="VEVENT"><c:time-range start="` + f + `" end="` + t + `"/></c:comp-filter></c:comp-filter></c:filter>
</c:calendar-query>`
	final, data, err := c.do(ctx, "REPORT", calendarHref, "1", []byte(body))
	if err != nil {
		return nil, err
	}
	ms, err := parseMultistatus(data)
	if err != nil {
		return nil, err
	}
	var out []calResource
	for _, r := range ms.Responses {
		res := calResource{}
		if h, err := resolveHref(final, r.Href); err == nil {
			res.Href = h
		}
		for _, p := range r.props() {
			if strings.TrimSpace(p.CalendarData) != "" {
				res.Data = p.CalendarData
			}
			if p.ETag != "" {
				res.ETag = p.ETag
			}
		}
		if res.Data != "" {
			out = append(out, res)
		}
	}
	return out, nil
}

func firstHref(base *url.URL, data []byte, pick func(davProp) string) (string, error) {
	ms, err := parseMultistatus(data)
	if err != nil {
		return "", err
	}
	for _, r := range ms.Responses {
		for _, p := range r.props() {
			if h := strings.TrimSpace(pick(p)); h != "" {
				return resolveHref(base, h)
			}
		}
	}
	return "", errors.New("not in the server answer")
}

func resolveHref(base *url.URL, href string) (string, error) {
	u, err := url.Parse(strings.TrimSpace(href))
	if err != nil {
		return "", errors.New("bad address from the server")
	}
	return base.ResolveReference(u).String(), nil
}

// normalizeCalColor turns #RRGGBB or #RRGGBBAA into #RRGGBB ("" if unusable).
func normalizeCalColor(s string) string {
	s = strings.TrimSpace(s)
	if len(s) != 7 && len(s) != 9 || s[0] != '#' {
		return ""
	}
	for _, r := range s[1:] {
		if !(r >= '0' && r <= '9' || r >= 'a' && r <= 'f' || r >= 'A' && r <= 'F') {
			return ""
		}
	}
	return strings.ToUpper(s[:7])
}

// --- transport ---

func isLoopbackHost(h string) bool {
	return h == "127.0.0.1" || h == "localhost" || h == "::1"
}

// credsAllowed says whether credentials may be sent to u: https (or plain
// http to loopback only, for tests), on the base address or an allowed suffix.
func credsAllowed(u, base *url.URL) bool {
	host := strings.ToLower(u.Hostname())
	switch u.Scheme {
	case "https":
	case "http":
		if !isLoopbackHost(host) {
			return false
		}
	default:
		return false
	}
	if strings.EqualFold(u.Host, base.Host) {
		return true
	}
	for _, suf := range caldavAllowedSuffixes {
		if strings.HasSuffix(host, suf) {
			return true
		}
	}
	return false
}

func (c *calDAVClient) httpClient() *http.Client {
	var cl http.Client
	if c.HTTP != nil {
		cl = *c.HTTP
	} else {
		cl.Timeout = 20 * time.Second
	}
	cl.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	return &cl
}

// do is the only way a request leaves this client. Read-only methods only.
// It returns the address the answer finally came from and the (size-limited)
// body. Nothing in an error text holds the password.
func (c *calDAVClient) do(ctx context.Context, method, rawURL, depth string, body []byte) (*url.URL, []byte, error) {
	switch method {
	case "PROPFIND", "REPORT", "OPTIONS":
	default:
		return nil, nil, fmt.Errorf("method %q is not allowed: this client only reads", method)
	}
	base, err := url.Parse(strings.TrimSpace(c.BaseURL))
	if err != nil || base.Host == "" {
		return nil, nil, errors.New("bad calendar address")
	}
	cur, err := url.Parse(rawURL)
	if err != nil || cur.Host == "" {
		return nil, nil, errors.New("bad calendar address")
	}
	cl := c.httpClient()
	for hop := 0; ; hop++ {
		if !credsAllowed(cur, base) {
			return nil, nil, errors.New("refusing to send the sign-in to that address")
		}
		req, err := http.NewRequestWithContext(ctx, method, cur.String(), bytes.NewReader(body))
		if err != nil {
			return nil, nil, errors.New("bad request")
		}
		req.SetBasicAuth(c.User, c.Pass)
		req.Header.Set("Content-Type", "application/xml; charset=utf-8")
		if depth != "" {
			req.Header.Set("Depth", depth)
		}
		resp, err := cl.Do(req)
		if err != nil {
			var ue *url.Error
			if errors.As(err, &ue) {
				err = ue.Err
			}
			return nil, nil, fmt.Errorf("the calendar server could not be reached: %w", err)
		}
		data, rerr := io.ReadAll(io.LimitReader(resp.Body, caldavMaxBody+1))
		resp.Body.Close()
		status := resp.StatusCode
		switch {
		case status == 301 || status == 302 || status == 303 || status == 307 || status == 308:
			if hop >= caldavMaxRedirects {
				return nil, nil, errors.New("too many redirects")
			}
			loc, err := url.Parse(resp.Header.Get("Location"))
			if resp.Header.Get("Location") == "" || err != nil {
				return nil, nil, errors.New("bad redirect from the server")
			}
			cur = cur.ResolveReference(loc)
			continue
		case status == 401 || status == 403:
			return nil, nil, fmt.Errorf("%w (HTTP %d)", errCalAuth, status)
		case status != 207 && (status < 200 || status > 299):
			return nil, nil, fmt.Errorf("%s failed with HTTP %d", method, status)
		}
		if rerr != nil {
			return nil, nil, errors.New("reading the answer failed")
		}
		if len(data) > caldavMaxBody {
			return nil, nil, errors.New("the answer is too large")
		}
		return cur, data, nil
	}
}
