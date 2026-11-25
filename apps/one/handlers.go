package main

import (
	"bytes"
	"context"
	"embed"
	"encoding/json"
	"errors"
	"fmt"
	"html/template"
	"io/fs"
	"log/slog"
	"net/http"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"
	_ "time/tzdata"

	"lab/apps/one/repo"
	"lab/platform/appkit"
)

//go:embed web
var webFS embed.FS

// pageFuncs are the small helpers the pages use.
var pageFuncs = template.FuncMap{
	"dict2":  func(v ...any) []any { return v },
	"plural": func(n int, unit string) string { return plural(int64(n), unit) },
}

type app struct {
	uploads        uploadGrants // one-time upload addresses for agents' files (SPEC 29)
	store          *store
	dataDir        string
	secretsDir     string
	envText        string
	version        string
	loc            *time.Location
	now            func() time.Time
	gmail          gmailConfig
	graph          graphConfig
	flow           deviceFlow
	tokMu          sync.Mutex   // the Microsoft sign-in file
	opMu           sync.Mutex   // one test or send at a time
	icloudMu       sync.Mutex   // one read of the iCloud calendars at a time
	icloudBase     string       // the CalDAV address; tests point it at a fake
	outlookCalMu   sync.Mutex   // one read of the Office calendar at a time
	googleMu       sync.Mutex   // one read of the Google calendars at a time
	googleCfgMu    sync.Mutex   // changes of the list of Google calendars, one at a time
	googleHTTP     *http.Client // nil: the default; tests put a fake Google in front
	pages          *template.Template
	static         http.Handler
	icons          map[string][]byte
	imp            *importer
	gimp           *graphImporter // the Office mailbox
	refr           refresher      // the Refresh button (SPEC 15)
	pics           srcPics        // the owner's pictures of the sources (SPEC 5, 23)
	agentSeen      atomic.Int64   // when an agent was last noted
	agentMu        sync.RWMutex   // the agents' token
	refusedLogAt   atomic.Int64   // when a refused token was last logged
	agentTok       string
	bridgeMu       sync.RWMutex // the chat bridge's token
	bridgeTok      string
	bridge         *http.ServeMux // the addresses under /api/bridge/
	api            *http.ServeMux
	apiPatterns    []string // the addresses of a.api and of the bridge, for the test that every one refuses the Work tool token
	bridgePatterns []string
	proMu          sync.RWMutex // the Work tool token (SPEC 32)
	proTok         string
	// notify tells the owner's phone; the platform drops what a preview sends.
	notify func(title, body, url string) error
	// async runs what must not hold a page back (importing a dropped message
	// again); tests make it run at once.
	async func(func())
	// The review by the app's own model and the night check (SPEC 31).
	claudeHTTP *http.Client // nil: the default; tests put a fake Claude in front
	reviewMu   sync.Mutex   // one pass of the review at a time
	idleLogged time.Time    // when the "review idle" line was last written (under reviewMu)
	reviewWake chan struct{}
	nightMu    sync.Mutex // one night check at a time
	// fails is the ledger of records that failed on their own (softfail.go);
	// log is where the app's own error lines go (nil: the importer's logger).
	fails failLedger
	// dbBad is set by the daily database check when it fails; /healthz then fails.
	dbBad atomic.Bool
	// quickCheck is the database check; tests replace it.
	quickCheck func(context.Context) error
	// beforeStore is a test hook: called with the subject before a mail is stored.
	beforeStore func(subject string)
	log         *slog.Logger
	summaryMu   sync.Mutex // one pass over the summary of what Claude closed at a time
}

func newApp(st *store, dataDir, appEnv, appVersion string) *app {
	envText := "development"
	switch {
	case appEnv == "prod":
		envText = "production"
	case appEnv != "":
		envText = "preview: " + appEnv
	}
	version := "dev"
	if appVersion != "" {
		version = appVersion
		if len(version) > 7 {
			version = version[:7]
		}
	}
	loc, err := time.LoadLocation("Europe/Prague")
	if err != nil {
		loc = time.UTC
	}
	sub, err := fs.Sub(webFS, "web")
	if err != nil {
		panic(err)
	}
	a := &app{
		store:      st,
		dataDir:    dataDir,
		secretsDir: filepath.Join(dataDir, "secrets"),
		envText:    envText,
		version:    version,
		loc:        loc,
		now:        time.Now,
		gmail:      defaultGmailConfig(),
		graph:      defaultGraphConfig(),
		icloudBase: icloudCalDAV,
		pages:      template.Must(template.New("").Funcs(pageFuncs).ParseFS(webFS, "web/*.html")),
		static:     http.StripPrefix("/static/", http.FileServer(http.FS(sub))),
		icons:      makeIcons(),
		notify:     appkit.Notify,

		reviewWake: make(chan struct{}, 1),
	}
	a.quickCheck = st.QuickCheck
	a.async = func(f func()) { go a.runUnit("background job", f) }
	a.imp = newImporter(a)
	a.gimp = newGraphImporter(a, a.imp.core)
	if err := a.loadAgentToken(); err != nil {
		appkit.Error("preparing the agents' token", err)
	}
	if err := a.loadBridgeToken(); err != nil {
		appkit.Error("preparing the chat bridge's token", err)
	}
	a.loadProToken()
	a.bridge = a.bridgeRoutes()
	return a
}

func (a *app) routes() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /{$}", withConn(phoneList(a.index)))
	mux.HandleFunc("GET /list/state", withConn(phoneList(a.listState)))
	mux.HandleFunc("GET /list/connections", withConn(a.listConnections))
	mux.HandleFunc("GET /list/{stage}", withConn(phoneList(a.listGroup)))
	mux.HandleFunc("GET /items/{id}/row", a.itemRow)
	mux.HandleFunc("GET /items/{id}/messages", a.itemMessages)
	mux.HandleFunc("GET /items/{id}/pulse", a.itemPulse)
	mux.HandleFunc("POST /items/{id}/notes", a.addNote)
	mux.HandleFunc("POST /refresh", a.refreshNow)
	mux.HandleFunc("GET /sources/{key}/picture", a.serveSourcePicture)
	mux.HandleFunc("POST /sources/{key}/picture", a.setSourcePicture)
	mux.HandleFunc("POST /sources/{key}/picture/remove", a.removeSourcePicture)
	mux.HandleFunc("POST /connectors/notify/test", a.notifyTest)
	mux.HandleFunc("POST /connectors/{key}/remove", a.removeSource)
	mux.HandleFunc("GET /refresh", a.refreshStatus)
	mux.HandleFunc("POST /items/{id}/todo", a.itemTodo)
	mux.HandleFunc("POST /items/{id}/write", a.chatWrite)
	mux.HandleFunc("POST /writes/{group}/withdraw", a.withdrawWrite)
	mux.HandleFunc("POST /items/{id}/{action}", a.ownerActs)
	mux.HandleFunc("GET /agenda", a.agendaGet)
	mux.HandleFunc("POST /agenda/todos", a.agendaAddTodo)
	mux.HandleFunc("POST /agenda/{id}/done", a.agendaDone)
	mux.HandleFunc("POST /agenda/{id}/thread", a.agendaOpenThread)
	mux.HandleFunc("POST /agenda/proposals/{id}/{decision}", a.agendaProposalDecision)
	mux.HandleFunc("POST /proposals/{id}/withdraw", a.withdrawChatReply)
	mux.HandleFunc("POST /proposals/{id}/{decision}", a.decideProposal)
	mux.HandleFunc("GET /files/{id}", a.poolFile)
	mux.HandleFunc("GET /files/{id}/view", a.poolFile)
	mux.HandleFunc("GET /items/{id}", a.itemPage)
	mux.HandleFunc("POST /items/{id}/bring-back", a.bringBack)
	mux.HandleFunc("POST /settings/setting-proposals/{id}/{decision}", a.decideSettingProposalHandler)
	mux.HandleFunc("GET /closed", a.closedPageHandler)
	mux.HandleFunc("POST /items/{id}/closed-seen", a.markSeenHandler)
	mux.HandleFunc("POST /closed/seen-all", a.markAllSeenHandler)
	mux.HandleFunc("GET /settings", a.settingsPage)
	mux.HandleFunc("POST /settings", a.saveSettings)
	mux.HandleFunc("POST /settings/guards/{key}/{state}", a.switchGuard)
	mux.HandleFunc("POST /settings/blocked-targets", a.saveBlockedTargets)
	mux.HandleFunc("POST /settings/rules", a.addRule)
	mux.HandleFunc("POST /settings/matter-patterns", a.addMatterPattern)
	mux.HandleFunc("POST /settings/matter-patterns/{n}/remove", a.removeMatterPattern)
	mux.HandleFunc("POST /settings/matter-proposals/{id}/decision/{decision}", a.decideMatterProposalHandler)
	mux.HandleFunc("POST /settings/rules/test", a.testRuleForm)
	mux.HandleFunc("POST /settings/rules/{id}", a.saveRule)
	mux.HandleFunc("POST /settings/rules/{id}/test", a.testRuleOf)
	mux.HandleFunc("POST /settings/rules/{id}/enabled/{state}", a.switchRule)
	mux.HandleFunc("POST /settings/rules/{id}/decision/{decision}", a.decideRuleHandler)
	mux.HandleFunc("POST /settings/skip-rules/{id}/decision/{decision}", a.decideSkipRuleHandler)
	mux.HandleFunc("POST /settings/skip-rules/{id}/enabled/{state}", a.switchSkipRule)
	mux.HandleFunc("POST /settings/skipped/{id}/again", a.importAgainHandler)
	mux.HandleFunc("POST /settings/test-data/{run}/remove", a.removeTestData)
	mux.HandleFunc("GET /people", a.peoplePage)
	mux.HandleFunc("GET /people/{id}", a.personPage)
	mux.HandleFunc("POST /people-proposals/{id}/{decision}", a.decidePersonHandler)
	mux.HandleFunc("POST /people/{id}", a.savePersonHandler)
	mux.HandleFunc("GET /people/{id}/picture", a.servePicture)
	mux.HandleFunc("POST /people/{id}/picture", a.setPicture)
	mux.HandleFunc("POST /people/{id}/picture/remove", a.removePicture)
	mux.HandleFunc("GET /notes", a.notesPageHandler)
	mux.HandleFunc("GET /notes/new", a.newNoteForm)
	mux.HandleFunc("GET /notes/{id}", a.editNoteForm)
	mux.HandleFunc("POST /notes", a.postNote)
	mux.HandleFunc("POST /notes/{id}", a.postNote)
	mux.HandleFunc("GET /search", a.searchPageHandler)
	mux.HandleFunc("GET /search/live", a.searchLive)
	mux.HandleFunc("GET /connectors", a.connectors)
	mux.HandleFunc("POST /connectors/gmail", a.gmailConnect)
	mux.HandleFunc("POST /connectors/gmail/test", a.gmailTestNow)
	mux.HandleFunc("POST /connectors/gmail/send", a.gmailSendTest)
	mux.HandleFunc("POST /connectors/gmail/import", a.gmailImport)
	mux.HandleFunc("POST /connectors/gmail/history", a.gmailHistory)
	mux.HandleFunc("POST /connectors/graph/history", a.graphHistory)
	mux.HandleFunc("POST /connectors/gmail/disconnect", a.gmailDisconnect)
	mux.HandleFunc("POST /connectors/graph/save", a.graphSave)
	mux.HandleFunc("POST /connectors/graph/signin", a.graphSignIn)
	mux.HandleFunc("GET /connectors/graph/status", a.graphStatus)
	mux.HandleFunc("POST /connectors/graph/test", a.graphTestNow)
	mux.HandleFunc("POST /connectors/graph/send", a.graphSendTest)
	mux.HandleFunc("POST /connectors/graph/import", a.graphImport)
	mux.HandleFunc("POST /connectors/graph/disconnect", a.graphDisconnect)
	mux.HandleFunc("POST /connectors/icloud", a.icloudConnect)
	mux.HandleFunc("POST /connectors/icloud/calendars", a.icloudSave)
	mux.HandleFunc("POST /connectors/icloud/test", a.icloudTest)
	mux.HandleFunc("POST /connectors/icloud/disconnect", a.icloudDisconnect)
	mux.HandleFunc("POST /connectors/google", a.googleConnect)
	mux.HandleFunc("POST /connectors/google/calendars", a.googleSave)
	mux.HandleFunc("POST /connectors/google/test", a.googleTest)
	mux.HandleFunc("POST /connectors/google/disconnect", a.googleDisconnect)
	mux.HandleFunc("POST /connectors/outlookcal/test", a.outlookCalTest)
	mux.HandleFunc("POST /connectors/server/test", a.serverTest)
	mux.HandleFunc("POST /connectors/claude-api", a.claudeKeySave)
	mux.HandleFunc("POST /connectors/claude-api/remove", a.claudeKeyRemove)
	mux.HandleFunc("POST /night/dismiss", a.nightDismiss)
	mux.HandleFunc("POST /settings/prices", a.savePrices)
	mux.HandleFunc("POST /settings/claude", a.saveClaudeSettings)
	mux.HandleFunc("POST /connectors/agent-token/new", a.agentTokenNew)
	mux.HandleFunc("POST /connectors/bridge-token/new", a.bridgeTokenNew)
	mux.HandleFunc("POST /connectors/worktool-token/new", a.proTokenNew)
	mux.HandleFunc("POST /connectors/worktool-token/revoke", a.proTokenRevoke)
	mux.Handle("/api/", a.apiHandler())
	mux.HandleFunc("GET /healthz", a.healthz)
	mux.HandleFunc("GET /manifest.webmanifest", a.manifest)
	mux.HandleFunc("GET /apple-touch-icon.png", a.icon("apple-touch-icon.png"))
	mux.HandleFunc("GET /icon-192.png", a.icon("icon-192.png"))
	mux.HandleFunc("GET /icon-512.png", a.icon("icon-512.png"))
	mux.Handle("GET /static/style.css", a.static)
	mux.Handle("GET /static/app.js", a.static)
	mux.Handle("GET /static/phone.js", a.static)
	return appkit.LogRequests(securityHeaders(http.NewCrossOriginProtection().Handler(mux)))
}

func securityHeaders(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		h := w.Header()
		h.Set("Content-Security-Policy", "default-src 'none'; script-src 'self'; style-src 'self'; img-src 'self'; connect-src 'self'; manifest-src 'self'; form-action 'self'; base-uri 'none'; frame-ancestors 'none'")
		h.Set("X-Content-Type-Options", "nosniff")
		h.Set("Referrer-Policy", "same-origin")
		next.ServeHTTP(w, r)
	})
}

// --- views ---

type lastView struct {
	When   string
	OK     bool
	Result []string
}

type sectionView struct {
	Status    string
	StatusCls string
	Connected bool
	Address   string
	Notice    string
	Error     string
	Test      *lastView // the last Test now
	Send      *lastView // the last test mail
	Import    importView
	// ImportAction is where the import buttons post.
	ImportAction string
	// History is the state of the history of the whole mailbox (SPEC 21).
	History       historyView
	HistoryAction string
	// SPEC 23, 24: when it was last read and, when it has failed, the latest error with its time.
	LastRead      string
	LatestError   string
	LatestErrorAt string
}

// applyState puts a source's health onto its section: the status words of
// SPEC 23 and the last read and error of SPEC 24.
func (v *sectionView) applyState(st sourceState, test *lastView) {
	v.LastRead = st.LastRead
	switch st.Status {
	case statusLogin, statusError, statusStale:
		v.Status, v.StatusCls = st.Status, st.Cls
		v.LatestError, v.LatestErrorAt = st.Error, st.ErrorAt
		if st.Status == statusStale && st.Error == "" {
			v.LatestError = fmt.Sprintf("Nothing has been read for %s.", silentText(st.Silent))
		}
	case statusOK:
		v.Status, v.StatusCls = statusOK, "st-ok"
		if test != nil && !test.OK {
			v.Status, v.StatusCls = statusError, "st-error"
		}
	}
}

type graphView struct {
	sectionView
	Tenant, Client string
	HasIDs         bool
	Flow           flowView
}

type serverView struct {
	Info  serverInfo
	Error string
	Test  *lastView
}

type agentView struct {
	Token  string
	Notice string
}

// bridgeView is the Chat bridge: the row (state, when it was last heard, what it
// has brought) and the token section (SPEC 16, 23, 24).
type bridgeView struct {
	Token       string
	Notice      string
	Status      string
	StatusCls   string
	Heard       bool
	LastRead    string
	LatestError string
	ErrorAt     string
	Chats       int
	Messages    int
	Items24     int
	Messages24  int
	Version     string
	Networks    []bridgeNetView
	Limits      []string
}

type bridgeNetView struct {
	Name   string
	State  string
	Cls    string
	Detail string
	Logins string // how often it needed a new login, dated (the owner watches this)
}

type pageData struct {
	EnvText    string
	Version    string
	Agent      agentView
	Bridge     bridgeView
	Pro        proView
	List       listView
	Gmail      sectionView
	Graph      graphView
	ICloud     icloudView
	OutCal     outlookCalView
	Google     googleView
	Server     serverView
	Conn       connExtra
	ClaudeAPI  claudeAPIView
	Dot        dotView
	Alerts     []string
	ClosedN    int    // the count in the link "Closed (n)"
	ClosedText string // the link: `Closed (n)` or `Closed (n, m not seen)`
	Closed     closedView
	Settings   settingsView
	People     peopleView
	Notes      notesView
	Search     searchView
	Item       itemPageView
	Agenda     agendaView
	Only       *onlyView // the bar `Only <connection>` above a narrowed list
	Conns      connBlock // the block `Connections` in the left pane on a laptop
	// ShowAgenda puts the Agenda button into the top bar (the list page only).
	ShowAgenda bool
	// Top is what the shared top line says on a page of its own (filled by render).
	Top topInfo
}

// topInfo describes the shared top line of a page that is not the list: the
// page's title and its own main button, which sits in the right-hand block
// in the same place on every page (the owner, 2026-11-19: controls on one side).
type topInfo struct {
	Sub     bool
	Title   string
	Primary *topButton
}

// topButton is a page's main button: a link (Href) or a submit button of a
// form further down the page (Form is the form's id).
type topButton struct {
	Label string
	Href  string
	Form  string
}

var pageTitles = map[string]string{
	"closed.html":     "Closed",
	"connectors.html": "Connectors",
	"settings.html":   "Settings",
	"people.html":     "People",
	"notes.html":      "Notes",
	"note.html":       "Notes",
	"search.html":     "Search",
}

// topFor fills the top line of every page but the list.
func (a *app) topFor(name string, data pageData) topInfo {
	t := topInfo{Sub: true, Title: pageTitles[name]}
	switch name {
	case "item.html":
		t.Title = data.Item.Title
	case "notes.html":
		t.Primary = &topButton{Label: "Add note", Href: "/notes/new"}
	case "note.html":
		t.Primary = &topButton{Label: "Save", Form: "note-form"}
	case "settings.html":
		t.Primary = &topButton{Label: "Save", Form: "set-form"}
	case "people.html":
		if data.People.Open != nil {
			t.Primary = &topButton{Label: "Save", Form: "person-form"}
		}
	}
	return t
}

var notices = map[string]string{
	"claudekey":  "The key is saved. ONE now asks Claude to read new items.",
	"claudeoff":  "The key is removed from the server.",
	"gmail":      "Connected.",
	"gmailoff":   "Disconnected. The password is removed from the server.",
	"gmailimp":   "The import has started. This page shows how it goes.",
	"gmailbusy":  "An import is already running. Wait until it has finished.",
	"gmailhist":  "The history is being read. This page shows how it goes.",
	"graphhist":  "The history is being read. This page shows how it goes.",
	"graphids":   "Saved.",
	"graphoff":   "Disconnected. The sign-in is removed from the server.",
	"graphimp":   "The import has started. This page shows how it goes.",
	"graphbusy":  "An import is already running. Wait until it has finished.",
	"agentnew":   "A new token is made. The old one no longer works; put the new one into every connector.",
	"bridgenew":  "A new token is made. The old one no longer works; put the new one into the bridge on the Mac.",
	"pronew":     "A new token is made. The old one no longer works; put the new one into Work tool.",
	"prorevoked": "The token is revoked. Work tool can no longer read the list until you make a new token.",
}

func (a *app) lastView(ctx context.Context, key string) *lastView {
	t, ok := a.store.LastTest(ctx, key)
	if !ok {
		return nil
	}
	return &lastView{
		When:   t.At.In(a.loc).Format("2 Jan 2006 15:04"),
		OK:     t.OK,
		Result: strings.Split(t.Result, "\n"),
	}
}

func status(connected bool, last *lastView) (string, string) {
	switch {
	case !connected:
		return "Not configured", "st-stale"
	case last == nil:
		return "Not tested yet", "st-stale"
	case last.OK:
		return "OK", "st-ok"
	}
	return "Error", "st-error"
}

func (a *app) gmailSecret() (gmailSecret, bool) {
	var s gmailSecret
	ok := readSecret(a.secretsDir, gmailSecretFile, &s) && s.Address != "" && s.Password != ""
	return s, ok
}

func (a *app) gmailView(ctx context.Context, notice, errText string) sectionView {
	v := sectionView{Notice: notices[notice], Error: errText}
	if s, ok := a.gmailSecret(); ok {
		v.Connected, v.Address = true, s.Address
	}
	v.Test = a.lastView(ctx, gmailConn)
	v.Send = a.lastView(ctx, gmailConn+"-send")
	v.Status, v.StatusCls = status(v.Connected, v.Test)
	if v.Connected {
		v.Import = a.importViewFor(ctx, gmailSrc)
		v.ImportAction = "/connectors/gmail/import"
		v.History, v.HistoryAction = a.historyViewFor(ctx, gmailSrc, historyKeyG), "/connectors/gmail/history"
		v.applyState(a.sourceStateOf(ctx, gmailSrc), v.Test)
	}
	return v
}

func (a *app) graphViewFor(ctx context.Context, notice, errText string) graphView {
	v := graphView{sectionView: sectionView{Notice: notices[notice], Error: errText}}
	v.Tenant, v.Client = a.store.Setting(ctx, keyGraphTenant), a.store.Setting(ctx, keyGraphClient)
	v.HasIDs = tenantRE.MatchString(v.Tenant) && clientRE.MatchString(v.Client)
	var sec graphSecret
	if readSecret(a.secretsDir, graphSecretFile, &sec) && sec.RefreshToken != "" {
		v.Connected, v.Address = true, sec.Address
	}
	v.Flow = a.flow.view()
	if v.Flow.State == "error" && v.Error == "" {
		v.Error = v.Flow.Message
	}
	v.Test = a.lastView(ctx, graphConn)
	v.Send = a.lastView(ctx, graphConn+"-send")
	if v.Connected {
		v.Import = a.importViewFor(ctx, graphSrc)
		v.ImportAction = "/connectors/graph/import"
		v.History, v.HistoryAction = a.historyViewFor(ctx, graphSrc, historyKeyO), "/connectors/graph/history"
	}
	switch {
	case !v.HasIDs:
		v.Status, v.StatusCls = "Not configured", "st-stale"
	case !v.Connected:
		v.Status, v.StatusCls = "Needs login", "st-stale"
	case a.graphNeedsLogin(ctx):
		// SPEC 24: a sign-in that has run out is said so, in plain words.
		v.Status, v.StatusCls = "Needs login", "st-error"
		v.applyState(a.sourceStateOf(ctx, graphSrc), v.Test)
		if v.LatestError == "" && v.Error == "" {
			v.Error = needsLoginText
		}
	default:
		v.Status, v.StatusCls = status(true, v.Test)
		v.applyState(a.sourceStateOf(ctx, graphSrc), v.Test)
	}
	return v
}

func (a *app) serverViewFor(ctx context.Context) serverView {
	v := serverView{Test: a.lastView(ctx, "server")}
	info, err := readServerInfo(a.dataDir)
	if err != nil {
		v.Error = "The disk could not be read."
		return v
	}
	v.Info = info
	return v
}

func (a *app) render(w http.ResponseWriter, name string, status int, data pageData) {
	if name != "index.html" {
		// Every other page has the list's top line, with the connections dot
		// and the strip for a failed source.
		data.Top = a.topFor(name, data)
		if data.Dot.Label == "" {
			ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
			states := a.sourceStates(ctx)
			data.Dot = dotFor(states)
			data.Alerts = a.allAlerts(ctx, states)
			cancel()
		}
	}
	var buf bytes.Buffer
	if err := a.pages.ExecuteTemplate(&buf, name, data); err != nil {
		appkit.Error("rendering a page", err)
		http.Error(w, "template error", http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(status)
	w.Write(buf.Bytes())
}

func (a *app) index(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	lv, err := a.listViewFor(ctx)
	if err != nil {
		appkit.Error("building the list", err)
		http.Error(w, "The list could not be loaded. Reload the page to try again.", http.StatusInternalServerError)
		return
	}
	states := a.sourceStates(ctx)
	// every strip is sent, in the order of DESIGN 10.1: failed sources, review
	// paused, proposals, the night check, waiting for Claude; the page fits
	// them to one line and counts what does not fit (`+n more`)
	alerts := a.allAlerts(ctx, states)
	ag, err := a.agendaViewFor(ctx, false)
	if err != nil {
		appkit.Error("building the agenda", err)
	}
	a.render(w, "index.html", http.StatusOK, pageData{EnvText: a.envText, Version: a.version, List: lv, Agenda: ag, ShowAgenda: true,
		Dot: dotFor(states), Alerts: alerts, ClosedN: a.store.ClosedCount(ctx), ClosedText: closedLinkText(a.store.ClosedCount(ctx), a.store.CountUnseenClosed(ctx)), Conns: a.connBlockFor(ctx, states), Only: a.onlyFor(ctx, states)})
}

type itemPageView struct {
	Title string
	Row   rowView
	Since int64
}

// itemPage is one item on a page of its own, open: where a search result, a
// closed item or a link lands. It is the same row the list shows.
func (a *app) itemPage(w http.ResponseWriter, r *http.Request) {
	id, ok := itemID(r)
	if !ok {
		http.NotFound(w, r)
		return
	}
	ctx := r.Context()
	// SPEC 10: the number of a row that was joined into another leads to the row that stays.
	if to := a.store.MergedInto(ctx, id); to != 0 {
		http.Redirect(w, r, "/items/"+strconv.FormatInt(to, 10), http.StatusSeeOther)
		return
	}
	lr, found := a.store.ListRowByID(ctx, id)
	if !found || lr.Hidden {
		http.NotFound(w, r)
		return
	}
	v := a.rowFor(lr, a.now())
	views := []rowView{v}
	a.dress(ctx, views, []repo.ListRow{lr})
	v = views[0]
	body, err := a.bodyFor(ctx, lr, false)
	if err != nil {
		appkit.Error("building an item page", err)
		http.Error(w, "The item could not be loaded. Reload the page to try again.", http.StatusInternalServerError)
		return
	}
	v.Open, v.Body = true, body
	a.render(w, "item.html", http.StatusOK, pageData{EnvText: a.envText, Version: a.version, Item: itemPageView{Title: v.Number, Row: v, Since: a.store.LastEventID(r.Context())}})
}

// renderConnectors draws the page; the notice or error belongs to one
// section ("gmail" or "graph").
func (a *app) renderConnectors(w http.ResponseWriter, r *http.Request, code int, section, notice, errText string) {
	ctx := r.Context()
	agentNotice, bridgeNotice, proNotice := "", "", ""
	if section == "worktool" {
		proNotice = notice
	}
	if section == "agent" {
		agentNotice = notice
	}
	if section == "bridge" {
		bridgeNotice = notice
	}
	gn, gerr, mn, merr := "", "", "", ""
	in, ierr, on, oerr, goon, goerr := "", "", "", "", "", ""
	switch section {
	case "gmail":
		gn, gerr = notice, errText
	case "graph":
		mn, merr = notice, errText
	case "icloud":
		in, ierr = notice, errText
	case "outlookcal":
		on, oerr = notice, errText
	case "google":
		goon, goerr = notice, errText
	}
	data := pageData{
		EnvText: a.envText,
		Version: a.version,
		Gmail:   a.gmailView(ctx, gn, gerr),
		Graph:   a.graphViewFor(ctx, mn, merr),
		ICloud:  a.icloudViewFor(ctx, in, ierr),
		OutCal:  a.outlookCalViewFor(ctx, on, oerr),
		Google:  a.googleViewFor(ctx, goon, goerr),
		Server:  a.serverViewFor(ctx),
		Agent:   agentView{Token: a.agentTokenValue(), Notice: notices[agentNotice]},
		Bridge:  a.bridgeViewFor(ctx, notices[bridgeNotice]),
		Pro:     a.proViewFor(notices[proNotice]),
	}
	data.Conn = a.connExtraFor(ctx, &data)
	data.ClaudeAPI = a.claudeAPIViewFor(ctx, "", "")
	if section == "claude" {
		data.ClaudeAPI = a.claudeAPIViewFor(ctx, notices[notice], errText)
	}
	switch {
	case section == "picture-error":
		data.Conn.PicError = errText
	case notice == "picture":
		data.Conn.PicNotice = "The picture is set."
	case notice == "nopicture":
		data.Conn.PicNotice = "The picture is removed."
	case section == "notify":
		if errText != "" {
			data.Conn.Notify.Error = errText
		} else {
			data.Conn.PicNotice = "A test notice was handed over."
		}
	}
	a.render(w, "connectors.html", code, data)
}

func (a *app) connectors(w http.ResponseWriter, r *http.Request) {
	n := r.URL.Query().Get("n")
	section := ""
	switch {
	case strings.HasPrefix(n, "gmail"):
		section = "gmail"
	case strings.HasPrefix(n, "graph"):
		section = "graph"
	case strings.HasPrefix(n, "agent"):
		section = "agent"
	case strings.HasPrefix(n, "bridge"):
		section = "bridge"
	case strings.HasPrefix(n, "icloud"):
		section = "icloud"
	case strings.HasPrefix(n, "google"):
		section = "google"
	case strings.HasPrefix(n, "notify"):
		section = "notify"
	case strings.HasPrefix(n, "claude"):
		section = "claude"
	case strings.HasPrefix(n, "pro"):
		section = "worktool"
	}
	a.renderConnectors(w, r, http.StatusOK, section, n, "")
}

func (a *app) healthz(w http.ResponseWriter, r *http.Request) {
	if err := a.store.Ping(r.Context()); err != nil {
		http.Error(w, "database unavailable", http.StatusServiceUnavailable)
		return
	}
	if a.dbBad.Load() {
		http.Error(w, "database check failed", http.StatusServiceUnavailable)
		return
	}
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	w.Write([]byte("ok"))
}

func (a *app) manifest(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/manifest+json")
	json.NewEncoder(w).Encode(map[string]any{
		"name":             "ONE",
		"short_name":       "ONE",
		"start_url":        "/",
		"scope":            "/",
		"display":          "standalone",
		"background_color": "#f4f4f1",
		"theme_color":      iconColor,
		"icons": []map[string]string{
			{"src": "/icon-192.png", "sizes": "192x192", "type": "image/png"},
			{"src": "/icon-512.png", "sizes": "512x512", "type": "image/png", "purpose": "any maskable"},
		},
	})
}

func (a *app) icon(name string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "image/png")
		w.Header().Set("Cache-Control", "public, max-age=86400")
		w.Write(a.icons[name])
	}
}

// --- Gmail ---

func (a *app) record(ctx context.Context, key string, ok bool, text string) {
	if err := a.store.SaveTest(context.WithoutCancel(ctx), key, ok, text, a.now()); err != nil {
		appkit.Error("saving a test result", err)
	}
}

func (a *app) gmailConnect(w http.ResponseWriter, r *http.Request) {
	r.Body = http.MaxBytesReader(w, r.Body, 8<<10)
	addr, okAddr := cleanAddress(r.FormValue("address"))
	pass := cleanAppPassword(r.FormValue("password"))
	if !okAddr || pass == "" || len(pass) > 100 {
		a.renderConnectors(w, r, http.StatusBadRequest, "gmail", "", "Enter the Gmail address and an app password.")
		return
	}
	a.opMu.Lock()
	defer a.opMu.Unlock()
	if _, err := readGmail(a.gmail, addr, pass, 1); err != nil {
		a.renderConnectors(w, r, http.StatusBadRequest, "gmail", "", plainError("Gmail", err, pass))
		return
	}
	if err := writeSecret(a.secretsDir, gmailSecretFile, gmailSecret{Address: addr, Password: pass}); err != nil {
		appkit.Error("saving the Gmail secret", err)
		a.renderConnectors(w, r, http.StatusInternalServerError, "gmail", "", "The password could not be saved. Try again.")
		return
	}
	a.store.ForgetTest(r.Context(), gmailConn)
	a.store.ForgetTest(r.Context(), gmailConn+"-send")
	a.setGmailNeedsLogin(r.Context(), false) // SPEC 24: fetching resumes by itself
	http.Redirect(w, r, "/connectors?n=gmail#gmail", http.StatusSeeOther)
}

func (a *app) gmailTestNow(w http.ResponseWriter, r *http.Request) {
	sec, ok := a.gmailSecret()
	if !ok {
		a.renderConnectors(w, r, http.StatusBadRequest, "gmail", "", "Connect Gmail first.")
		return
	}
	a.opMu.Lock()
	defer a.opMu.Unlock()
	text, good := gmailTest(a.gmail, sec, a.loc)
	a.record(r.Context(), gmailConn, good, text)
	if good {
		a.setGmailNeedsLogin(r.Context(), false) // the password works again
	}
	http.Redirect(w, r, "/connectors#gmail", http.StatusSeeOther)
}

func (a *app) gmailSendTest(w http.ResponseWriter, r *http.Request) {
	sec, ok := a.gmailSecret()
	if !ok {
		a.renderConnectors(w, r, http.StatusBadRequest, "gmail", "", "Connect Gmail first.")
		return
	}
	a.opMu.Lock()
	defer a.opMu.Unlock()
	// Nothing from the request decides who gets the mail: only the stored address.
	now := a.now()
	if err := gmailSend(a.gmail, sec, now); err != nil {
		a.record(r.Context(), gmailConn+"-send", false, "Could not send: "+plainError("Gmail", err, sec.Password))
	} else {
		a.record(r.Context(), gmailConn+"-send", true, "Sent "+now.In(a.loc).Format("15:04")+".")
	}
	http.Redirect(w, r, "/connectors#gmail", http.StatusSeeOther)
}

func (a *app) gmailDisconnect(w http.ResponseWriter, r *http.Request) {
	if err := removeSecret(a.secretsDir, gmailSecretFile); err != nil {
		appkit.Error("removing the Gmail secret", err)
		http.Error(w, "could not remove the password", http.StatusInternalServerError)
		return
	}
	a.store.ForgetTest(r.Context(), gmailConn)
	a.store.ForgetTest(r.Context(), gmailConn+"-send")
	a.setGmailNeedsLogin(r.Context(), false)
	http.Redirect(w, r, "/connectors?n=gmailoff#gmail", http.StatusSeeOther)
}

// --- Office mail (Microsoft Graph) ---

func (a *app) graphSave(w http.ResponseWriter, r *http.Request) {
	r.Body = http.MaxBytesReader(w, r.Body, 8<<10)
	tenant := strings.TrimSpace(r.FormValue("tenant"))
	client := strings.TrimSpace(r.FormValue("client"))
	if !tenantRE.MatchString(tenant) || !clientRE.MatchString(client) {
		a.renderConnectors(w, r, http.StatusBadRequest, "graph", "", "Enter the directory (tenant) ID and the application (client) ID, as Azure shows them.")
		return
	}
	ctx := r.Context()
	changed := tenant != a.store.Setting(ctx, keyGraphTenant) || client != a.store.Setting(ctx, keyGraphClient)
	if err := a.store.SetSetting(ctx, keyGraphTenant, tenant); err != nil {
		http.Error(w, "could not save", http.StatusInternalServerError)
		return
	}
	if err := a.store.SetSetting(ctx, keyGraphClient, client); err != nil {
		http.Error(w, "could not save", http.StatusInternalServerError)
		return
	}
	if changed {
		// A sign-in belongs to the application it was made for.
		a.tokMu.Lock()
		removeSecret(a.secretsDir, graphSecretFile)
		a.tokMu.Unlock()
		a.store.ForgetTest(ctx, graphConn)
		a.store.ForgetTest(ctx, graphConn+"-send")
		a.flow.reset()
		a.setGraphNeedsLogin(ctx, false)
	}
	http.Redirect(w, r, "/connectors?n=graphids#graph", http.StatusSeeOther)
}

func (a *app) graphSignIn(w http.ResponseWriter, r *http.Request) {
	if err := a.startSignIn(r.Context()); err != nil {
		a.renderConnectors(w, r, http.StatusBadRequest, "graph", "", err.Error())
		return
	}
	http.Redirect(w, r, "/connectors#graph", http.StatusSeeOther)
}

func (a *app) graphStatus(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	json.NewEncoder(w).Encode(a.flow.view())
}

func (a *app) graphTestNow(w http.ResponseWriter, r *http.Request) {
	a.opMu.Lock()
	defer a.opMu.Unlock()
	ctx, cancel := context.WithTimeout(r.Context(), 60*time.Second)
	defer cancel()
	text, good := a.graphTest(ctx, a.now())
	a.record(r.Context(), graphConn, good, text)
	http.Redirect(w, r, "/connectors#graph", http.StatusSeeOther)
}

func (a *app) graphSendTest(w http.ResponseWriter, r *http.Request) {
	a.opMu.Lock()
	defer a.opMu.Unlock()
	ctx, cancel := context.WithTimeout(r.Context(), 40*time.Second)
	defer cancel()
	now := a.now()
	// Nothing from the request decides who gets the mail: only the stored address.
	if err := a.graphSend(ctx); err != nil {
		reason := "sign in again"
		if !errors.Is(err, errNeedsLogin) {
			reason = plainMS("Microsoft", err)
		}
		a.record(r.Context(), graphConn+"-send", false, "Could not send: "+reason)
	} else {
		a.record(r.Context(), graphConn+"-send", true, "Sent "+now.In(a.loc).Format("15:04")+".")
	}
	http.Redirect(w, r, "/connectors#graph", http.StatusSeeOther)
}

func (a *app) graphDisconnect(w http.ResponseWriter, r *http.Request) {
	a.tokMu.Lock()
	err := removeSecret(a.secretsDir, graphSecretFile)
	a.tokMu.Unlock()
	if err != nil {
		appkit.Error("removing the Microsoft sign-in", err)
		http.Error(w, "could not remove the sign-in", http.StatusInternalServerError)
		return
	}
	a.flow.reset()
	a.setGraphNeedsLogin(r.Context(), false)
	a.store.ForgetTest(r.Context(), graphConn)
	a.store.ForgetTest(r.Context(), graphConn+"-send")
	http.Redirect(w, r, "/connectors?n=graphoff#graph", http.StatusSeeOther)
}

// --- Server ---

func (a *app) serverTest(w http.ResponseWriter, r *http.Request) {
	info, err := readServerInfo(a.dataDir)
	if err != nil {
		a.record(r.Context(), "server", false, "The disk could not be read.")
	} else {
		a.record(r.Context(), "server", true, info.text())
	}
	http.Redirect(w, r, "/connectors#server", http.StatusSeeOther)
}

// gmailImport starts an import of the chosen depth now, whatever the settings say.
func (a *app) gmailImport(w http.ResponseWriter, r *http.Request) {
	r.Body = http.MaxBytesReader(w, r.Body, 1<<10)
	if _, ok := a.gmailSecret(); !ok {
		a.renderConnectors(w, r, http.StatusBadRequest, "gmail", "", "Connect Gmail first.")
		return
	}
	a.startImport(w, r, "gmail", a.imp.Request)
}

// graphImport is the same button for the Office mailbox.
func (a *app) graphImport(w http.ResponseWriter, r *http.Request) {
	r.Body = http.MaxBytesReader(w, r.Body, 1<<10)
	if !a.graphSigned() {
		a.renderConnectors(w, r, http.StatusBadRequest, "graph", "", "Sign in to Microsoft first.")
		return
	}
	a.startImport(w, r, "graph", a.gimp.Request)
}

// gmailHistory is the button Read history for Gmail.
func (a *app) gmailHistory(w http.ResponseWriter, r *http.Request) {
	r.Body = http.MaxBytesReader(w, r.Body, 1<<10)
	if _, ok := a.gmailSecret(); !ok {
		a.renderConnectors(w, r, http.StatusBadRequest, "gmail", "", "Connect Gmail first.")
		return
	}
	section := "gmail"
	if !a.imp.RequestHistory(r.Context()) {
		http.Redirect(w, r, "/connectors?n="+section+"busy#"+section, http.StatusSeeOther)
		return
	}
	http.Redirect(w, r, "/connectors?n="+section+"hist#"+section, http.StatusSeeOther)
}

// graphHistory is the button Read history for the Office mailbox.
func (a *app) graphHistory(w http.ResponseWriter, r *http.Request) {
	r.Body = http.MaxBytesReader(w, r.Body, 1<<10)
	if !a.graphSigned() {
		a.renderConnectors(w, r, http.StatusBadRequest, "graph", "", "Sign in to Microsoft first.")
		return
	}
	if !a.gimp.RequestHistory(r.Context()) {
		http.Redirect(w, r, "/connectors?n=graphbusy#graph", http.StatusSeeOther)
		return
	}
	http.Redirect(w, r, "/connectors?n=graphhist#graph", http.StatusSeeOther)
}

func (a *app) startImport(w http.ResponseWriter, r *http.Request, section string, request func(string, time.Duration) bool) {
	var mode string
	var depth time.Duration
	switch r.FormValue("depth") {
	case "week":
		mode, depth = "week", 7*24*time.Hour
	case "month":
		mode, depth = "month", 30*24*time.Hour
	default:
		http.Error(w, "unknown depth", http.StatusBadRequest)
		return
	}
	if !request(mode, depth) {
		http.Redirect(w, r, "/connectors?n="+section+"busy#"+section, http.StatusSeeOther)
		return
	}
	http.Redirect(w, r, "/connectors?n="+section+"imp#"+section, http.StatusSeeOther)
}

// --- Agent token ---

// agentTokenNew makes a new token: the old one stops working at once. Only
// this signed-in page can do it; nothing under /api/ can.
func (a *app) agentTokenNew(w http.ResponseWriter, r *http.Request) {
	if err := a.newAgentToken(); err != nil {
		appkit.Error("making a new agent token", err)
		http.Error(w, "The token could not be made. Try again.", http.StatusInternalServerError)
		return
	}
	http.Redirect(w, r, "/connectors?n=agentnew#agent", http.StatusSeeOther)
}

// phoneList marks the request of a phone for the list: the page tells the
// server by a cookie, set by the same media query the stylesheet uses before
// the page is drawn (web/phone.js), so the counts of the first paint are right.
func phoneList(h http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if c, err := r.Cookie("one-phone"); err == nil && c.Value == "1" {
			r = r.WithContext(repo.WithPhone(r.Context()))
		}
		h(w, r)
	}
}
