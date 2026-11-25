package main

import (
	"bytes"
	"embed"
	"html/template"
	"image"
	"image/color"
	"image/png"
	"io"
	"log"
	"math"
	"net"
	"net/http"
	"net/url"
	"sort"
	"strconv"
	"strings"
	"time"
)

// labd's own pages in domain mode (CONTRACT.md, "Login gate"): sign-in,
// setup, sign-out and the list of apps. They answer only on the lab's own
// host over HTTPS, never on an app's host, so no app's code can ever run on
// the page where the owner types his password.

//go:embed pages
var pagesFS embed.FS

var pageTmpl = func() map[string]*template.Template {
	base := template.Must(template.New("").ParseFS(pagesFS, "pages/layout.html"))
	out := map[string]*template.Template{}
	for _, name := range []string{"login", "setup", "home", "errors"} {
		out[name] = template.Must(template.Must(base.Clone()).ParseFS(pagesFS, "pages/"+name+".html"))
	}
	return out
}()

var labCSS = func() []byte {
	b, err := pagesFS.ReadFile("pages/lab.css")
	if err != nil {
		panic(err)
	}
	return b
}()

type texts struct {
	SignInTitle, Password, SignIn, SignInHelp, WrongPassword, TooMany, NoPassword, SignedOut                      string
	SetupTitle, SetupIntro, SetupCode, NewPassword, PasswordHint, RepeatPassword, SavePassword, BadCode, Mismatch string
	TooShort, HomeTitle, Apps, Previews, NoApps, NotRunning, SignOut, PasswordSaved                               string
	AlreadySet, SignInLink, PreviewsHint, PasswordIsSet                                                           string
	ErrorsLink, ErrorsTitle, ErrorsHint, ErrorsWord, WarningsWord, InfoWord, NoErrors, JournalFailed, Back        string
	PreviewWord                                                                                                   string
}

var textsEN = texts{
	SignInTitle:    "Sign in to your lab",
	Password:       "Password",
	SignIn:         "Sign in",
	SignInHelp:     "At home the lab opens without a password.",
	WrongPassword:  "That password isn't right. Try again.",
	TooMany:        "Too many tries. Wait a minute, then try again.",
	NoPassword:     "No password yet. You set it with a one-time setup link: ask Claude for one.",
	SignedOut:      "You're signed out. This device will need the password now.",
	SetupTitle:     "Choose your lab password",
	SetupIntro:     "You'll need it to open your apps away from home. This device stays signed in for six months.",
	SetupCode:      "Setup code",
	NewPassword:    "New password",
	PasswordHint:   "At least 12 characters. A short sentence works well.",
	RepeatPassword: "Repeat the password",
	SavePassword:   "Save password",
	BadCode:        "That code doesn't match. Check it, or ask Claude for a new link: each one works once, for an hour.",
	Mismatch:       "The passwords don't match. Type both again.",
	TooShort:       "That password is too short: it needs at least 12 characters.",
	HomeTitle:      "Your lab",
	Apps:           "Apps",
	Previews:       "Previews",
	NoApps:         "No apps yet.",
	NotRunning:     "not running",
	SignOut:        "Sign out",
	PasswordSaved:  "Password saved. This device is signed in.",
	AlreadySet:     "Your password is already set.",
	PasswordIsSet:  "Password is set",
	SignInLink:     "Sign in",
	PreviewsHint:   "Work in progress. Their data is a copy of the real app's.",
	ErrorsLink:     "Errors in the apps",
	ErrorsTitle:    "Errors",
	ErrorsHint:     "What the apps logged as an error in the last 24 hours. Real apps first, then work in progress.",
	ErrorsWord:     "errors",
	WarningsWord:   "warnings",
	InfoWord:       "other lines",
	NoErrors:       "No errors.",
	JournalFailed:  "The log could not be read:",
	Back:           "Back to the lab",
	PreviewWord:    "preview",
}

var textsCS = texts{
	SignInTitle:    "Přihlášení do labu",
	Password:       "Heslo",
	SignIn:         "Přihlásit se",
	SignInHelp:     "Doma se lab otevře i bez hesla.",
	WrongPassword:  "Heslo nesouhlasí. Zkus to znovu.",
	TooMany:        "Příliš mnoho pokusů. Počkej minutu a zkus to znovu.",
	NoPassword:     "Heslo zatím není nastavené. Nastavíš ho přes jednorázový odkaz, řekni si o něj Claudovi.",
	SignedOut:      "Odhlásil ses. Na tomhle zařízení teď budeš potřebovat heslo.",
	SetupTitle:     "Zvol si heslo do labu",
	SetupIntro:     "Budeš ho potřebovat, když budeš otevírat aplikace mimo domov. Toto zařízení zůstane přihlášené půl roku.",
	SetupCode:      "Kód pro nastavení",
	NewPassword:    "Nové heslo",
	PasswordHint:   "Alespoň 12 znaků. Dobře poslouží krátká věta.",
	RepeatPassword: "Heslo ještě jednou",
	SavePassword:   "Uložit heslo",
	BadCode:        "Kód nesedí. Zkontroluj ho, nebo si u Clauda řekni o nový odkaz. Každý platí hodinu a jen jednou.",
	Mismatch:       "Hesla se neshodují. Napiš obě znovu.",
	TooShort:       "Heslo je moc krátké, potřebuje aspoň 12 znaků.",
	HomeTitle:      "Tvůj lab",
	Apps:           "Aplikace",
	Previews:       "Náhledy",
	NoApps:         "Zatím tu nejsou žádné aplikace.",
	NotRunning:     "neběží",
	SignOut:        "Odhlásit se",
	PasswordSaved:  "Heslo je uložené. Toto zařízení je přihlášené.",
	AlreadySet:     "Heslo už je nastavené.",
	PasswordIsSet:  "Heslo je nastavené",
	SignInLink:     "Přihlásit se",
	PreviewsHint:   "Rozpracované verze. Jejich data jsou kopie ostrých.",
	ErrorsLink:     "Chyby v aplikacích",
	ErrorsTitle:    "Chyby",
	ErrorsHint:     "Co aplikace za posledních 24 hodin zapsaly jako chybu. Nejdřív ostré aplikace, pak rozpracované.",
	ErrorsWord:     "chyb",
	WarningsWord:   "varování",
	InfoWord:       "dalších řádků",
	NoErrors:       "Žádné chyby.",
	JournalFailed:  "Log se nepodařilo přečíst:",
	Back:           "Zpět do labu",
	PreviewWord:    "náhled",
}

// pageLang picks Czech when the browser prefers it (or Slovak) over
// English, else English.
func pageLang(r *http.Request) (string, texts) {
	best, bestQ := "en", -1.0
	for _, part := range strings.Split(r.Header.Get("Accept-Language"), ",") {
		tag, params, _ := strings.Cut(strings.TrimSpace(part), ";")
		q := 1.0
		if v, ok := strings.CutPrefix(strings.TrimSpace(params), "q="); ok {
			if f, err := strconv.ParseFloat(v, 64); err == nil {
				q = f
			}
		}
		lang := strings.ToLower(tag)
		if i := strings.IndexByte(lang, '-'); i > 0 {
			lang = lang[:i]
		}
		switch lang {
		case "cs", "sk":
			lang = "cs"
		case "en":
		default:
			continue
		}
		if q > bestQ {
			best, bestQ = lang, q
		}
	}
	if best == "cs" {
		return "cs", textsCS
	}
	return "en", textsEN
}

type appLink struct {
	Title, URL, Host, Status string
	Down                     bool
}

type previewGroup struct {
	Name string
	Apps []appLink
}

type pageData struct {
	T                texts
	Lang, Title      string
	Notice, Error    string
	Info             string
	Next, Code       string
	NoForm, SignedIn bool
	CodeHidden       bool
	Apps             []appLink
	Previews         []previewGroup
	Errors           []appErrorsView
}

// controlHost returns the lab's domain when r came through nginx over HTTPS
// for the lab's own host.
func (s *Server) controlHost(r *http.Request) (string, bool) {
	domain := s.store.Settings().Domain
	if domain == "" || !s.overTLS(r) {
		return "", false
	}
	host := strings.ToLower(r.Host)
	if h, _, err := net.SplitHostPort(host); err == nil {
		host = h
	}
	return domain, host == domain
}

// page serves one of labd's pages: only on the lab's own host, never framed,
// never cached, with nothing but the page's own style allowed to load.
func (s *Server) page(h func(w http.ResponseWriter, r *http.Request, base string)) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		base, ok := s.controlHost(r)
		if !ok {
			http.NotFound(w, r)
			return
		}
		hd := w.Header()
		hd.Set("Content-Security-Policy", "default-src 'none'; style-src 'self'; img-src 'self'; manifest-src 'self'; form-action 'self' https://*."+base+"; frame-ancestors 'none'; base-uri 'none'")
		hd.Set("X-Frame-Options", "DENY")
		hd.Set("X-Content-Type-Options", "nosniff")
		hd.Set("Referrer-Policy", "no-referrer")
		hd.Set("Cache-Control", "no-store")
		if r.Method == http.MethodPost {
			if err := s.cop.Check(r); err != nil {
				http.Error(w, "This form can only be sent from the lab's own page.", http.StatusForbidden)
				return
			}
			r.Body = http.MaxBytesReader(w, r.Body, 16<<10)
			if err := r.ParseForm(); err != nil {
				http.Error(w, "bad form", http.StatusBadRequest)
				return
			}
		}
		h(w, r, base)
	}
}

func render(w http.ResponseWriter, name string, code int, d pageData) {
	var buf bytes.Buffer
	if err := pageTmpl[name].ExecuteTemplate(&buf, "layout", d); err != nil {
		log.Printf("page %s: %v", name, err)
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.WriteHeader(code)
	w.Write(buf.Bytes())
}

// clientIP is the visitor's address as nginx saw it.
func clientIP(r *http.Request) string {
	if fromLocalProxy(r) {
		if ip := strings.TrimSpace(r.Header.Get("X-Real-IP")); net.ParseIP(ip) != nil {
			return ip
		}
	}
	host, _, _ := net.SplitHostPort(r.RemoteAddr)
	return host
}

// allowedIP reports whether ip is on the settings' allow list (home).
func allowedIP(ip string, allow []string) bool {
	addr := net.ParseIP(ip)
	if addr == nil {
		return false
	}
	for _, a := range allow {
		if _, n, err := net.ParseCIDR(a); err == nil {
			if n.Contains(addr) {
				return true
			}
		} else if other := net.ParseIP(a); other != nil && other.Equal(addr) {
			return true
		}
	}
	return false
}

// signedIn reports whether r carries the owner's pass for the lab's domain.
func (s *Server) signedIn(r *http.Request, base string) bool {
	hasPassword, gen := s.auth.Status()
	if !hasPassword {
		return false
	}
	for _, c := range r.Cookies() {
		if c.Name == sessionCookie && s.sessions.ValidUser(c.Value, base, base, gen) {
			return true
		}
	}
	return false
}

// cleanNext returns where to go after signing in: an https address on the
// lab's domain (a path counts as one on the lab's own host), or "" for
// anything else, so the sign-in page can't send anyone off to another site.
func cleanNext(raw, base string) string {
	if raw == "" || len(raw) > 2048 || strings.ContainsAny(raw, "\\\r\n\t ") {
		return ""
	}
	if strings.HasPrefix(raw, "/") && !strings.HasPrefix(raw, "//") {
		raw = "https://" + base + raw
	}
	u, err := url.Parse(raw)
	if err != nil || u.Scheme != "https" || u.User != nil || u.Opaque != "" || u.Port() != "" || u.Host != u.Hostname() {
		return ""
	}
	host := u.Hostname()
	if !safeHost(host) || (host != base && !strings.HasSuffix(host, "."+base)) {
		return ""
	}
	if u.Path == "" {
		u.Path = "/"
	}
	if !strings.HasPrefix(u.Path, "/") || strings.HasPrefix(u.Path, "//") {
		return ""
	}
	u.Fragment, u.RawFragment = "", ""
	return u.String()
}

func (s *Server) setPass(w http.ResponseWriter, base string, gen int) error {
	value, exp, err := s.sessions.IssueUser(userSessionTTL, base, gen)
	if err != nil {
		return err
	}
	http.SetCookie(w, &http.Cookie{Name: sessionCookie, Value: value, Domain: base, Path: "/", Expires: exp,
		MaxAge: int(userSessionTTL / time.Second), Secure: true, HttpOnly: true, SameSite: http.SameSiteLaxMode})
	return nil
}

// loginRedirect is where nginx sends a visitor an app's gate turned away
// (named location @lab_login): to the sign-in page, which brings them back.
func (s *Server) loginRedirect(w http.ResponseWriter, r *http.Request) {
	domain := s.store.Settings().Domain
	if domain == "" || !fromLocalProxy(r) {
		http.NotFound(w, r)
		return
	}
	w.Header().Set("Cache-Control", "no-store")
	target := "https://" + domain + "/_lab/login"
	host := strings.ToLower(r.Header.Get("X-Lab-Host"))
	if next := cleanNext("https://"+host+r.Header.Get("X-Lab-Original-URI"), domain); next != "" && host != "" {
		target += "?next=" + url.QueryEscape(next)
	}
	http.Redirect(w, r, target, http.StatusFound)
}

func (s *Server) loginPage(w http.ResponseWriter, r *http.Request, base string) {
	lang, t := pageLang(r)
	q := r.URL.Query()
	next := cleanNext(q.Get("next"), base)
	if s.signedIn(r, base) {
		http.Redirect(w, r, orHome(next), http.StatusSeeOther)
		return
	}
	d := pageData{T: t, Lang: lang, Title: t.SignInTitle, Next: next}
	if hasPassword, _ := s.auth.Status(); !hasPassword {
		d.Info, d.NoForm = t.NoPassword, true
	} else {
		switch {
		case q.Get("out") == "1":
			d.Notice = t.SignedOut
		case q.Get("e") == "wrong":
			d.Error = t.WrongPassword
		case q.Get("e") == "busy":
			d.Error = t.TooMany
		}
	}
	render(w, "login", http.StatusOK, d)
}

func orHome(next string) string {
	if next == "" {
		return "/"
	}
	return next
}

// again sends the browser back to a form page with what went wrong in the
// address, so reloading the answer never sends the form a second time.
func again(w http.ResponseWriter, r *http.Request, path string, q url.Values) {
	if enc := q.Encode(); enc != "" {
		path += "?" + enc
	}
	http.Redirect(w, r, path, http.StatusSeeOther)
}

func (s *Server) login(w http.ResponseWriter, r *http.Request, base string) {
	next := cleanNext(r.PostForm.Get("next"), base)
	q := url.Values{}
	if next != "" {
		q.Set("next", next)
	}
	ip := clientIP(r)
	if hasPassword, _ := s.auth.Status(); !hasPassword {
		again(w, r, "/_lab/login", q)
		return
	}
	if s.limiter.blocked(ip) || !s.limiter.acquire() {
		q.Set("e", "busy")
		again(w, r, "/_lab/login", q)
		return
	}
	gen, ok := s.auth.CheckPassword(r.PostForm.Get("password"))
	s.limiter.release()
	if !ok {
		s.limiter.failed(ip)
		log.Printf("login: wrong password from %s", ip)
		recordSignInFailure(ip, time.Now())
		q.Set("e", "wrong")
		again(w, r, "/_lab/login", q)
		return
	}
	if err := s.setPass(w, base, gen); err != nil {
		log.Printf("login: %v", err)
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	log.Printf("login: signed in from %s", ip)
	http.Redirect(w, r, orHome(next), http.StatusSeeOther)
}

func (s *Server) logout(w http.ResponseWriter, r *http.Request, base string) {
	http.SetCookie(w, &http.Cookie{Name: sessionCookie, Value: "", Domain: base, Path: "/", MaxAge: -1,
		Secure: true, HttpOnly: true, SameSite: http.SameSiteLaxMode})
	http.Redirect(w, r, "/_lab/login?out=1", http.StatusSeeOther)
}

func cleanCode(code string) string {
	code = strings.TrimSpace(code)
	if len(code) > 64 {
		return ""
	}
	return code
}

// setupPage asks for a new password. A code that came in the link is
// checked at once and then kept out of the way; once a password is set and
// no new code is waiting, the page just points to signing in.
func (s *Server) setupPage(w http.ResponseWriter, r *http.Request, base string) {
	lang, t := pageLang(r)
	q := r.URL.Query()
	code := cleanCode(q.Get("code"))
	d := pageData{T: t, Lang: lang, Title: t.SetupTitle, Code: code}
	if hasPassword, _ := s.auth.Status(); hasPassword && !s.auth.SetupPending() {
		d.Info, d.NoForm, d.Title = t.AlreadySet, true, t.PasswordIsSet
		render(w, "setup", http.StatusOK, d)
		return
	}
	ip := clientIP(r)
	switch {
	case code == "":
	case s.setupLimiter.blocked(ip):
		d.Error = t.TooMany
	case s.auth.SetupCodeValid(code):
		d.CodeHidden = true
	default:
		s.setupLimiter.failed(ip)
		d.Error = t.BadCode
	}
	if d.Error == "" {
		switch q.Get("e") {
		case "short":
			d.Error = t.TooShort
		case "mismatch":
			d.Error = t.Mismatch
		case "code":
			d.Error = t.BadCode
			d.Code = cleanCode(q.Get("typed"))
		case "busy":
			d.Error = t.TooMany
		}
	}
	render(w, "setup", http.StatusOK, d)
}

func (s *Server) setup(w http.ResponseWriter, r *http.Request, base string) {
	code := cleanCode(r.PostForm.Get("code"))
	password, repeat := r.PostForm.Get("password"), r.PostForm.Get("password2")
	q := url.Values{}
	ip := clientIP(r)
	// Typos in the new password are not guesses: they don't count as tries.
	switch {
	case len(password) < minPassword || len(password) > maxPassword:
		q.Set("e", "short")
	case password != repeat:
		q.Set("e", "mismatch")
	case s.setupLimiter.blocked(ip) || !s.setupLimiter.acquire():
		q.Set("e", "busy")
	}
	if q.Get("e") != "" {
		if code != "" {
			q.Set("code", code)
		}
		again(w, r, "/_lab/setup", q)
		return
	}
	gen, err := s.auth.UseSetupCode(code, password)
	s.setupLimiter.release()
	if err != nil {
		if err == errBadCode {
			s.setupLimiter.failed(ip)
			// Back with the code as typed, so a typo is fixed, not retyped.
			again(w, r, "/_lab/setup", url.Values{"e": {"code"}, "typed": {code}})
			return
		}
		log.Printf("setup: %v", err)
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	if err := s.setPass(w, base, gen); err != nil {
		log.Printf("setup: %v", err)
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	log.Printf("login: a new password was set from %s; earlier passes no longer work", ip)
	http.Redirect(w, r, "/?saved=1", http.StatusSeeOther)
}

// home lists the apps, for the owner at home or signed in.
func (s *Server) home(w http.ResponseWriter, r *http.Request, base string) {
	lang, t := pageLang(r)
	set := s.store.Settings()
	in := s.signedIn(r, base)
	if !in && !allowedIP(clientIP(r), set.AllowIPs) {
		http.Redirect(w, r, "/_lab/login", http.StatusSeeOther)
		return
	}
	d := pageData{T: t, Lang: lang, Title: t.HomeTitle, SignedIn: in}
	if r.URL.Query().Get("saved") == "1" && in {
		d.Notice = t.PasswordSaved
	}
	groups := map[string]int{}
	for _, inst := range s.store.List() {
		title := inst.Title
		if title == "" {
			title = inst.App
		}
		link := appLink{Title: title, URL: urlFor(inst.App, inst.Env, set), Host: hostFor(inst.App, inst.Env, set)}
		if inst.Status != "running" {
			link.Down, link.Status = true, t.NotRunning
		}
		if inst.Env == prodEnv {
			d.Apps = append(d.Apps, link)
			continue
		}
		i, ok := groups[inst.Env]
		if !ok {
			i = len(d.Previews)
			groups[inst.Env] = i
			d.Previews = append(d.Previews, previewGroup{Name: inst.Env})
		}
		d.Previews[i].Apps = append(d.Previews[i].Apps, link)
	}
	sort.Slice(d.Previews, func(a, b int) bool { return d.Previews[a].Name < d.Previews[b].Name })
	render(w, "home", http.StatusOK, d)
}

func (s *Server) css(w http.ResponseWriter, r *http.Request, _ string) {
	w.Header().Set("Content-Type", "text/css; charset=utf-8")
	w.Header().Set("Cache-Control", "public, max-age=3600")
	w.Write(labCSS)
}

// labIcon is the lab's home-screen icon: a white ring on the lab's blue.
var labIcon = func() []byte {
	const size = 180
	img := image.NewRGBA(image.Rect(0, 0, size, size))
	blue := color.RGBA{0x28, 0x51, 0xd8, 0xff}
	white := color.RGBA{0xff, 0xff, 0xff, 0xff}
	for y := 0; y < size; y++ {
		for x := 0; x < size; x++ {
			dx, dy := float64(x)-89.5, float64(y)-89.5
			d := math.Sqrt(dx*dx + dy*dy)
			c := blue
			if d >= 34 && d <= 52 {
				c = white
			}
			img.SetRGBA(x, y, c)
		}
	}
	var buf bytes.Buffer
	if err := png.Encode(&buf, img); err != nil {
		panic(err)
	}
	return buf.Bytes()
}()

func (s *Server) icon(w http.ResponseWriter, r *http.Request, _ string) {
	w.Header().Set("Content-Type", "image/png")
	w.Header().Set("Cache-Control", "public, max-age=86400")
	w.Write(labIcon)
}

func (s *Server) manifest(w http.ResponseWriter, r *http.Request, _ string) {
	w.Header().Set("Content-Type", "application/manifest+json")
	w.Header().Set("Cache-Control", "public, max-age=3600")
	io.WriteString(w, `{"name":"Lab","short_name":"Lab","start_url":"/","display":"standalone","background_color":"#f4f3ef","theme_color":"#2851d8","icons":[{"src":"/_lab/icon.png","sizes":"180x180","type":"image/png"}]}`)
}
