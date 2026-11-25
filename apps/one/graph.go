package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"
)

// Trial 2: can an app registered in the Office Microsoft 365 tenant read
// and send mail and read the calendar, with the sign-in kept by the server?
// Microsoft Graph, device-code sign-in, public client (no client secret).

const (
	graphConn       = "graph"
	graphScopes     = "offline_access User.Read Mail.ReadWrite Mail.Send Calendars.Read"
	graphLoginBase  = "https://login.microsoftonline.com"
	graphAPIBase    = "https://graph.microsoft.com/v1.0"
	keyGraphTenant  = "graph_tenant"
	keyGraphClient  = "graph_client"
	refreshEarly    = 2 * time.Minute
	graphCallLimit  = 25 * time.Second
	graphTestMailTo = "ONE test mail"
)

var (
	tenantRE = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9.-]{0,98}$`)
	clientRE = regexp.MustCompile(`^[0-9A-Fa-f]{8}-[0-9A-Fa-f]{4}-[0-9A-Fa-f]{4}-[0-9A-Fa-f]{4}-[0-9A-Fa-f]{12}$`)

	errNeedsLogin = errors.New("sign-in needed")
)

// graphSecret is what the server keeps after a sign-in (graph.json, 0600).
type graphSecret struct {
	Address      string `json:"address"`
	AccessToken  string `json:"access_token"`
	RefreshToken string `json:"refresh_token"`
	ExpiresAt    int64  `json:"expires_at"`
}

// graphConfig holds the addresses so tests can point at a fake Microsoft.
type graphConfig struct {
	LoginBase string
	APIBase   string
	HTTP      *http.Client
	// PollEvery, when set, replaces the interval Microsoft names (tests only).
	PollEvery time.Duration
	// RetryWait, when set, replaces the wait Microsoft asks for before a call
	// is repeated (tests only).
	RetryWait time.Duration
}

func defaultGraphConfig() graphConfig {
	return graphConfig{LoginBase: graphLoginBase, APIBase: graphAPIBase, HTTP: &http.Client{Timeout: graphCallLimit}}
}

// msError is a refusal from Microsoft in its own words.
type msError struct {
	Code   string
	Text   string
	Status int // the HTTP status, when the refusal came from Graph itself
	// RetryAfter is the wait Graph asked for with a 429 or 503.
	RetryAfter time.Duration
}

func (e *msError) Error() string {
	if e.Text != "" {
		return e.Text
	}
	return e.Code
}

// tokenReply is Microsoft's answer from the token and device-code addresses.
type tokenReply struct {
	AccessToken  string `json:"access_token"`
	RefreshToken string `json:"refresh_token"`
	ExpiresIn    int    `json:"expires_in"`
	Error        string `json:"error"`
	ErrorDesc    string `json:"error_description"`
	// device-code answer
	DeviceCode      string `json:"device_code"`
	UserCode        string `json:"user_code"`
	VerificationURI string `json:"verification_uri"`
	Interval        int    `json:"interval"`
}

func firstLine(s string) string {
	s, _, _ = strings.Cut(s, "\n")
	s, _, _ = strings.Cut(s, "\r")
	return oneLine(s, 300)
}

func (g graphConfig) postForm(ctx context.Context, tenant, path string, form url.Values) (tokenReply, error) {
	u := g.LoginBase + "/" + url.PathEscape(tenant) + "/oauth2/v2.0/" + path
	req, err := http.NewRequestWithContext(ctx, "POST", u, strings.NewReader(form.Encode()))
	if err != nil {
		return tokenReply{}, err
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	resp, err := g.HTTP.Do(req)
	if err != nil {
		return tokenReply{}, errors.New("Microsoft could not be reached")
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	var tr tokenReply
	if err := json.Unmarshal(body, &tr); err != nil {
		return tokenReply{}, errors.New("Microsoft answered something that could not be read")
	}
	if tr.Error != "" {
		return tr, &msError{Code: tr.Error, Text: firstLine(tr.ErrorDesc)}
	}
	if resp.StatusCode/100 != 2 {
		return tr, &msError{Code: "http_" + fmt.Sprint(resp.StatusCode)}
	}
	return tr, nil
}

// startDeviceCode asks Microsoft for a code the person types at the address.
func (g graphConfig) startDeviceCode(ctx context.Context, tenant, client string) (tokenReply, error) {
	return g.postForm(ctx, tenant, "devicecode", url.Values{"client_id": {client}, "scope": {graphScopes}})
}

// pollToken asks once whether the person has finished signing in.
func (g graphConfig) pollToken(ctx context.Context, tenant, client, deviceCode string) (tokenReply, error) {
	return g.postForm(ctx, tenant, "token", url.Values{
		"grant_type":  {"urn:ietf:params:oauth:grant-type:device_code"},
		"client_id":   {client},
		"device_code": {deviceCode},
	})
}

func (g graphConfig) refresh(ctx context.Context, tenant, client, refreshToken string) (tokenReply, error) {
	return g.postForm(ctx, tenant, "token", url.Values{
		"grant_type":    {"refresh_token"},
		"client_id":     {client},
		"refresh_token": {refreshToken},
		"scope":         {graphScopes},
	})
}

// api calls Microsoft Graph with the access token. out may be nil.
func (g graphConfig) api(ctx context.Context, token, method, path string, body any, out any) error {
	return g.apiPrefer(ctx, token, method, path, nil, body, out)
}

// graphError turns an error answer from Graph into an msError, without
// quoting anything from the mail.
func graphError(status int, data []byte, hdr http.Header) *msError {
	var e struct {
		Error struct {
			Code    string `json:"code"`
			Message string `json:"message"`
		} `json:"error"`
	}
	json.Unmarshal(data, &e)
	me := &msError{Code: e.Error.Code, Status: status, Text: fmt.Sprintf("%d %s %s", status, e.Error.Code, firstLine(e.Error.Message))}
	if n, err := strconv.Atoi(strings.TrimSpace(hdr.Get("Retry-After"))); err == nil && n > 0 {
		me.RetryAfter = time.Duration(n) * time.Second
	}
	return me
}

// preferHeader is the one Prefer header: the time zone and whatever else a call asks for.
func preferHeader(extra []string) string {
	return strings.Join(append([]string{`outlook.timezone="UTC"`}, extra...), ", ")
}

// apiPrefer is api with more Prefer values (immutable ids, page size).
func (g graphConfig) apiPrefer(ctx context.Context, token, method, path string, prefer []string, body any, out any) error {
	var rd io.Reader
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			return err
		}
		rd = strings.NewReader(string(b))
	}
	req, err := http.NewRequestWithContext(ctx, method, g.APIBase+path, rd)
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Accept", "application/json")
	req.Header.Set("Prefer", preferHeader(prefer))
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := g.HTTP.Do(req)
	if err != nil {
		return errors.New("Microsoft Graph could not be reached")
	}
	defer resp.Body.Close()
	data, _ := io.ReadAll(io.LimitReader(resp.Body, 4<<20))
	if resp.StatusCode/100 != 2 {
		return graphError(resp.StatusCode, data, resp.Header)
	}
	if out != nil {
		if err := json.Unmarshal(data, out); err != nil {
			return errors.New("Microsoft Graph answered something that could not be read")
		}
	}
	return nil
}

// apiStream is a GET whose answer is copied into w a buffer at a time, for
// an original message that can be large. The whole-call timeout of the
// client is lifted for it (a slow big download is not a failure); the
// context carries the limit instead.
func (g graphConfig) apiStream(ctx context.Context, token, path string, prefer []string, w io.Writer) (int64, error) {
	req, err := http.NewRequestWithContext(ctx, "GET", g.APIBase+path, nil)
	if err != nil {
		return 0, err
	}
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Prefer", preferHeader(prefer))
	client := *g.HTTP
	client.Timeout = 0
	resp, err := client.Do(req)
	if err != nil {
		return 0, errors.New("Microsoft Graph could not be reached")
	}
	defer resp.Body.Close()
	if resp.StatusCode/100 != 2 {
		data, _ := io.ReadAll(io.LimitReader(resp.Body, 64<<10))
		return 0, graphError(resp.StatusCode, data, resp.Header)
	}
	n, err := io.Copy(w, resp.Body)
	if err != nil {
		return n, errors.New("Microsoft Graph stopped sending the message")
	}
	return n, nil
}

// pathOfLink turns a next or delta link Graph gave into a path under the
// API address. A link that points anywhere else is refused, so the access
// token can never be sent to another address.
func (g graphConfig) pathOfLink(link string) (string, error) {
	if !strings.HasPrefix(link, g.APIBase+"/") {
		return "", errors.New("Microsoft Graph gave a link to another address; it was not followed")
	}
	return strings.TrimPrefix(link, g.APIBase), nil
}

// whoAmI returns the signed-in address.
func (g graphConfig) whoAmI(ctx context.Context, token string) (string, error) {
	var me struct {
		Mail string `json:"mail"`
		UPN  string `json:"userPrincipalName"`
	}
	if err := g.api(ctx, token, "GET", "/me?$select=mail,userPrincipalName", nil, &me); err != nil {
		return "", err
	}
	addr := me.Mail
	if addr == "" {
		addr = me.UPN
	}
	if a, ok := cleanAddress(addr); ok {
		return a, nil
	}
	return "", errors.New("Microsoft did not say which address is signed in")
}

// --- the device-code sign-in, as the app runs it ---

// deviceFlow is one sign-in in progress (kept in memory only).
type deviceFlow struct {
	mu       sync.Mutex
	state    string // "", "pending", "done", "error"
	userCode string
	verify   string
	message  string // the failure, when state is "error"
	until    time.Time
	cancel   context.CancelFunc
}

type flowView struct {
	State    string `json:"state"`
	UserCode string `json:"code,omitempty"`
	Verify   string `json:"url,omitempty"`
	Message  string `json:"message,omitempty"`
}

func (d *deviceFlow) view() flowView {
	d.mu.Lock()
	defer d.mu.Unlock()
	v := flowView{State: d.state, Message: d.message}
	if d.state == "pending" {
		v.UserCode, v.Verify = d.userCode, d.verify
	}
	return v
}

// reset forgets any sign-in in progress and stops its polling.
func (d *deviceFlow) reset() {
	d.mu.Lock()
	if d.cancel != nil {
		d.cancel()
		d.cancel = nil
	}
	d.state, d.message, d.userCode, d.verify = "", "", "", ""
	d.mu.Unlock()
}

func (d *deviceFlow) set(state, msg string) {
	d.mu.Lock()
	d.state, d.message = state, msg
	d.mu.Unlock()
}

// startSignIn begins a device-code sign-in; polling goes on in the
// background until the person finishes, refuses or the code runs out.
func (a *app) startSignIn(ctx context.Context) error {
	tenant, client := a.store.Setting(ctx, keyGraphTenant), a.store.Setting(ctx, keyGraphClient)
	if !tenantRE.MatchString(tenant) || !clientRE.MatchString(client) {
		return errors.New("Save the directory and application ids first.")
	}
	dc, err := a.graph.startDeviceCode(ctx, tenant, client)
	if err != nil {
		return errors.New(plainMS("Microsoft refused to start the sign-in", err))
	}
	if dc.DeviceCode == "" || dc.UserCode == "" {
		return errors.New("Microsoft did not give a sign-in code.")
	}
	verify := dc.VerificationURI
	if verify == "" {
		verify = "https://microsoft.com/devicelogin"
	}
	life := time.Duration(dc.ExpiresIn) * time.Second
	if life <= 0 || life > 30*time.Minute {
		life = 15 * time.Minute
	}
	f := &a.flow
	f.mu.Lock()
	if f.cancel != nil {
		f.cancel()
	}
	pctx, cancel := context.WithTimeout(context.Background(), life)
	f.cancel, f.state, f.message = cancel, "pending", ""
	f.userCode, f.verify, f.until = dc.UserCode, verify, time.Now().Add(life)
	f.mu.Unlock()
	interval := time.Duration(dc.Interval) * time.Second
	if interval < time.Second {
		interval = 5 * time.Second
	}
	go a.runUnit("microsoft sign-in", func() { a.pollSignIn(pctx, cancel, tenant, client, dc.DeviceCode, interval) })
	return nil
}

func (a *app) pollSignIn(ctx context.Context, cancel context.CancelFunc, tenant, client, deviceCode string, interval time.Duration) {
	defer cancel()
	f := &a.flow
	for {
		wait := interval
		if a.graph.PollEvery > 0 {
			wait = a.graph.PollEvery
		}
		select {
		case <-ctx.Done():
			if ctx.Err() == context.DeadlineExceeded {
				f.set("error", "The code ran out before the sign-in was finished. Press Sign in to get a new one.")
			}
			return
		case <-time.After(wait):
		}
		cctx, ccancel := context.WithTimeout(ctx, graphCallLimit)
		tr, err := a.graph.pollToken(cctx, tenant, client, deviceCode)
		ccancel()
		if ctx.Err() != nil {
			if ctx.Err() == context.DeadlineExceeded {
				f.set("error", "The code ran out before the sign-in was finished. Press Sign in to get a new one.")
			}
			return
		}
		if err != nil {
			var me *msError
			if errors.As(err, &me) {
				switch me.Code {
				case "authorization_pending":
					continue
				case "slow_down":
					interval += 5 * time.Second
					continue
				case "authorization_declined":
					f.set("error", "The sign-in was declined.")
					return
				case "expired_token", "bad_verification_code":
					f.set("error", "The code ran out before the sign-in was finished. Press Sign in to get a new one.")
					return
				}
			}
			f.set("error", plainMS("The sign-in failed", err))
			return
		}
		if tr.AccessToken == "" || tr.RefreshToken == "" {
			f.set("error", "Microsoft signed in but gave no long-lived sign-in. Check that offline_access is allowed for the application.")
			return
		}
		actx, acancel := context.WithTimeout(ctx, graphCallLimit)
		addr, err := a.graph.whoAmI(actx, tr.AccessToken)
		acancel()
		if err != nil {
			f.set("error", plainMS("Signed in, but the address could not be read", err))
			return
		}
		sec := graphSecret{Address: addr, AccessToken: tr.AccessToken, RefreshToken: tr.RefreshToken, ExpiresAt: time.Now().Unix() + int64(tr.ExpiresIn)}
		a.tokMu.Lock()
		err = writeSecret(a.secretsDir, graphSecretFile, sec)
		a.tokMu.Unlock()
		if err != nil {
			f.set("error", "The sign-in could not be saved on the server.")
			return
		}
		a.store.SetSettingBy(context.Background(), keyGraphLogin, "", "system", a.now()) // fetching resumes by itself
		f.set("done", "")
		return
	}
}

// plainMS words a Microsoft refusal for the page; it never holds a token.
func plainMS(what string, err error) string {
	var me *msError
	if errors.As(err, &me) {
		return oneLine(what+": "+me.Error(), 400)
	}
	return oneLine(what+": "+err.Error(), 400)
}

// graphAccess returns a valid access token and the stored sign-in, refreshing
// it when it is about to run out.
func (a *app) graphAccess(ctx context.Context) (string, graphSecret, error) {
	return a.graphToken(ctx, false)
}

// graphToken is graphAccess; force makes a new access token even when the
// stored one has time left (Graph said it no longer works).
func (a *app) graphToken(ctx context.Context, force bool) (string, graphSecret, error) {
	a.tokMu.Lock()
	defer a.tokMu.Unlock()
	var sec graphSecret
	if !readSecret(a.secretsDir, graphSecretFile, &sec) || sec.RefreshToken == "" {
		return "", sec, errNeedsLogin
	}
	if !force && sec.AccessToken != "" && time.Until(time.Unix(sec.ExpiresAt, 0)) > refreshEarly {
		return sec.AccessToken, sec, nil
	}
	tenant, client := a.store.Setting(ctx, keyGraphTenant), a.store.Setting(ctx, keyGraphClient)
	tr, err := a.graph.refresh(ctx, tenant, client, sec.RefreshToken)
	if err != nil {
		var me *msError
		if errors.As(err, &me) && (me.Code == "invalid_grant" || me.Code == "interaction_required" || me.Code == "consent_required" || me.Code == "login_required") {
			return "", sec, errNeedsLogin
		}
		return "", sec, err
	}
	sec.AccessToken = tr.AccessToken
	if tr.RefreshToken != "" {
		sec.RefreshToken = tr.RefreshToken
	}
	sec.ExpiresAt = time.Now().Unix() + int64(tr.ExpiresIn)
	if err := writeSecret(a.secretsDir, graphSecretFile, sec); err != nil {
		return "", sec, errors.New("the refreshed sign-in could not be saved")
	}
	return sec.AccessToken, sec, nil
}

// --- Test now and the test mail ---

type graphMsg struct {
	Received string `json:"receivedDateTime"`
	Subject  string `json:"subject"`
	ConvID   string `json:"conversationId"`
	From     struct {
		E struct {
			Name    string `json:"name"`
			Address string `json:"address"`
		} `json:"emailAddress"`
	} `json:"from"`
}

type graphEvent struct {
	Subject string `json:"subject"`
	AllDay  bool   `json:"isAllDay"`
	Start   struct {
		DateTime string `json:"dateTime"`
	} `json:"start"`
}

func parseGraphTime(s string) (time.Time, bool) {
	if len(s) < 19 {
		return time.Time{}, false
	}
	t, err := time.ParseInLocation("2006-01-02T15:04:05", s[:19], time.UTC)
	return t, err == nil
}

// graphTest shows who is signed in, the 5 newest messages and the next 5
// calendar events.
func (a *app) graphTest(ctx context.Context, now time.Time) (string, bool) {
	token, _, err := a.graphAccess(ctx)
	if err != nil {
		if errors.Is(err, errNeedsLogin) {
			return "Needs login: press Sign in.", false
		}
		return plainMS("Could not refresh the sign-in", err), false
	}
	g := a.graph
	var b strings.Builder
	addr, err := g.whoAmI(ctx, token)
	if err != nil {
		return plainMS("Microsoft refused to say who is signed in", err), false
	}
	fmt.Fprintf(&b, "Signed in as %s\n", addr)

	var msgs struct {
		Value []graphMsg `json:"value"`
	}
	if err := g.api(ctx, token, "GET", "/me/messages?$top=5&$orderby=receivedDateTime%20desc&$select=receivedDateTime,from,subject,conversationId", nil, &msgs); err != nil {
		b.WriteString(plainMS("Mail could not be read", err))
		return b.String(), false
	}
	b.WriteString("Newest messages:\n")
	if len(msgs.Value) == 0 {
		b.WriteString("  none\n")
	}
	for _, m := range msgs.Value {
		date := "date unknown"
		if t, ok := parseGraphTime(m.Received); ok {
			date = t.In(a.loc).Format("2 Jan 2006 15:04")
		}
		from := m.From.E.Address
		if m.From.E.Name != "" {
			from = m.From.E.Name + " <" + m.From.E.Address + ">"
		}
		fmt.Fprintf(&b, "  %s | %s | %s | conversation %s\n", date, oneLine(from, 80), oneLine(m.Subject, 100), oneLine(m.ConvID, 24))
	}

	var evs struct {
		Value []graphEvent `json:"value"`
	}
	q := url.Values{}
	q.Set("startDateTime", now.UTC().Format("2006-01-02T15:04:05"))
	q.Set("endDateTime", now.UTC().AddDate(0, 0, 90).Format("2006-01-02T15:04:05"))
	q.Set("$orderby", "start/dateTime")
	q.Set("$top", "5")
	q.Set("$select", "subject,start,isAllDay")
	if err := g.api(ctx, token, "GET", "/me/calendarView?"+strings.ReplaceAll(q.Encode(), "+", "%20"), nil, &evs); err != nil {
		b.WriteString(plainMS("The calendar could not be read", err))
		return b.String(), false
	}
	b.WriteString("Next events:\n")
	if len(evs.Value) == 0 {
		b.WriteString("  none in the next 90 days\n")
	}
	for _, e := range evs.Value {
		when := "time unknown"
		if t, ok := parseGraphTime(e.Start.DateTime); ok {
			if e.AllDay {
				when = t.Format("2 Jan 2006") + " (all day)"
			} else {
				when = t.In(a.loc).Format("2 Jan 2006 15:04")
			}
		}
		fmt.Fprintf(&b, "  %s | %s\n", when, oneLine(e.Subject, 100))
	}
	return strings.TrimRight(b.String(), "\n"), true
}

// graphMail is the body of Graph's sendMail.
type graphMail struct {
	Message struct {
		Subject string `json:"subject"`
		Body    struct {
			ContentType string `json:"contentType"`
			Content     string `json:"content"`
		} `json:"body"`
		ToRecipients []struct {
			E struct {
				Address string `json:"address"`
			} `json:"emailAddress"`
		} `json:"toRecipients"`
	} `json:"message"`
	SaveToSentItems bool `json:"saveToSentItems"`
}

// buildGraphTestMail makes the one test mail; its only recipient is the
// signed-in address. There is no parameter for another recipient.
func buildGraphTestMail(self string) graphMail {
	var m graphMail
	m.Message.Subject = graphTestMailTo
	m.Message.Body.ContentType = "Text"
	m.Message.Body.Content = "This is a test mail from ONE, sent to yourself to check that the server can send."
	m.Message.ToRecipients = []struct {
		E struct {
			Address string `json:"address"`
		} `json:"emailAddress"`
	}{{}}
	m.Message.ToRecipients[0].E.Address = self
	m.SaveToSentItems = true
	return m
}

func (a *app) graphSend(ctx context.Context) error {
	token, sec, err := a.graphAccess(ctx)
	if err != nil {
		return err
	}
	return a.graph.api(ctx, token, "POST", "/me/sendMail", buildGraphTestMail(sec.Address), nil)
}
