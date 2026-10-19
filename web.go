package main

import (
	"bytes"
	"cmp"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"embed"
	"encoding/hex"
	"fmt"
	"html/template"
	"io/fs"
	"maps"
	"net/http"
	"net/url"
	"path"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/opencontainers/go-digest"
	v1 "github.com/opencontainers/image-spec/specs-go/v1"
	"go.uber.org/zap"
	"golang.org/x/crypto/bcrypt"
)

//go:embed web
var webFS embed.FS

var usernameRe = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]{0,38}$`)

type server struct {
	cfg       Config
	log       *zap.Logger
	store     *store
	reg       *registry
	backup    *backup
	pages     map[string]*template.Template
	version   string
	setupCode string
}

type head struct {
	Title   string
	Version string
	Refresh bool
}

func newServer(cfg Config, log *zap.Logger, st *store, reg *registry, b *backup) (*server, error) {
	s := &server{cfg: cfg, log: log.Named("web"), store: st, reg: reg, backup: b, pages: map[string]*template.Template{}}
	if err := s.loadTemplates(); err != nil {
		return nil, err
	}
	if !st.hasAccount() {
		s.setupCode = rand.Text()[:8]
		s.log.Warn("no account yet, open /setup and enter the setup code", zap.String("setup_code", s.setupCode))
	}
	return s, nil
}

func (s *server) loadTemplates() error {
	icons := map[string]string{}
	entries, err := fs.ReadDir(webFS, "web/icons")
	if err != nil {
		return err
	}
	for _, e := range entries {
		name, ok := strings.CutSuffix(e.Name(), ".svg")
		if !ok {
			continue
		}
		b, err := webFS.ReadFile("web/icons/" + e.Name())
		if err != nil {
			return err
		}
		svg := string(b)
		start := strings.Index(svg, "<svg")
		open := strings.Index(svg[start:], ">") + start + 1
		icons[name] = strings.TrimSpace(svg[open:strings.LastIndex(svg, "</svg>")])
	}
	funcs := template.FuncMap{
		"icon": func(name string, size int, class ...string) template.HTML {
			return template.HTML(fmt.Sprintf(`<svg class="i %s" width="%d" height="%d" viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2" stroke-linecap="round" stroke-linejoin="round" aria-hidden="true">%s</svg>`,
				strings.Join(class, " "), size, size, icons[name]))
		},
		"join": strings.Join,
	}
	h := sha256.New()
	for _, f := range []string{"web/static/app.css", "web/static/app.js"} {
		b, err := webFS.ReadFile(f)
		if err != nil {
			return err
		}
		h.Write(b)
	}
	s.version = hex.EncodeToString(h.Sum(nil))[:10]
	for _, page := range []string{"dashboard.html", "login.html", "setup.html"} {
		t, err := template.New(page).Funcs(funcs).ParseFS(webFS, "web/templates/base.html", "web/templates/"+page)
		if err != nil {
			return err
		}
		s.pages[page] = t
	}
	return nil
}

func (s *server) render(w http.ResponseWriter, status int, page string, data any) {
	var buf bytes.Buffer
	if err := s.pages[page].ExecuteTemplate(&buf, "base", data); err != nil {
		s.log.Error("render", zap.String("page", page), zap.Error(err))
		http.Error(w, "render error", http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.WriteHeader(status)
	_, _ = buf.WriteTo(w)
}

func (s *server) routes() http.Handler {
	mux := http.NewServeMux()
	mux.Handle("/v2/", s.guard(s.reg.app))
	mux.HandleFunc("GET /token", s.handleToken)
	mux.HandleFunc("POST /token", s.handleToken)
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte("ok"))
	})
	static, _ := fs.Sub(webFS, "web")
	files := http.FileServerFS(static)
	mux.Handle("GET /static/", http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Cache-Control", "public, max-age=86400")
		files.ServeHTTP(w, r)
	}))

	ui := http.NewServeMux()
	ui.HandleFunc("GET /setup", s.setupPage)
	ui.HandleFunc("POST /setup", s.setup)
	ui.HandleFunc("GET /login", s.loginPage)
	ui.HandleFunc("POST /login", s.login)
	ui.HandleFunc("POST /logout", s.logout)
	ui.HandleFunc("GET /{$}", s.authed(s.dashboard))
	ui.HandleFunc("GET /r/{name...}", s.authed(s.dashboard))
	ui.HandleFunc("POST /visibility", s.authed(s.setVisibility))
	ui.HandleFunc("POST /tags/delete", s.authed(s.deleteTag))
	ui.HandleFunc("POST /repos/delete", s.authed(s.deleteRepo))
	ui.HandleFunc("POST /gc", s.authed(s.runGC))
	ui.HandleFunc("POST /backup", s.authed(s.runBackup))
	ui.HandleFunc("POST /tokens", s.authed(s.createToken))
	ui.HandleFunc("POST /tokens/delete", s.authed(s.deleteToken))
	mux.Handle("/", http.NewCrossOriginProtection().Handler(ui))
	return mux
}

func (s *server) authed(next func(http.ResponseWriter, *http.Request, string)) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if !s.store.hasAccount() {
			http.Redirect(w, r, "/setup", http.StatusSeeOther)
			return
		}
		u := s.user(r)
		if u == "" {
			http.Redirect(w, r, "/login", http.StatusSeeOther)
			return
		}
		next(w, r, u)
	}
}

func setFlash(w http.ResponseWriter, name, value string) {
	http.SetCookie(w, &http.Cookie{Name: "flash_" + name, Value: url.QueryEscape(value), Path: "/", MaxAge: 60, HttpOnly: true, SameSite: http.SameSiteLaxMode})
}

func takeFlash(w http.ResponseWriter, r *http.Request, name string) string {
	c, err := r.Cookie("flash_" + name)
	if err != nil {
		return ""
	}
	http.SetCookie(w, &http.Cookie{Name: "flash_" + name, Path: "/", MaxAge: -1})
	v, _ := url.QueryUnescape(c.Value)
	return v
}

func back(w http.ResponseWriter, r *http.Request, toast string) {
	if toast != "" {
		setFlash(w, "toast", toast)
	}
	to := r.FormValue("back")
	if !strings.HasPrefix(to, "/") || strings.HasPrefix(to, "//") {
		to = "/"
	}
	http.Redirect(w, r, to, http.StatusSeeOther)
}

type authView struct {
	head
	Host     string
	Error    string
	Username string
}

func (s *server) setupPage(w http.ResponseWriter, r *http.Request) {
	if s.store.hasAccount() {
		http.Redirect(w, r, "/login", http.StatusSeeOther)
		return
	}
	s.render(w, http.StatusOK, "setup.html", authView{head: head{Title: "Set up Registry", Version: s.version}, Host: s.host(r)})
}

func (s *server) setup(w http.ResponseWriter, r *http.Request) {
	if s.store.hasAccount() {
		http.Redirect(w, r, "/login", http.StatusSeeOther)
		return
	}
	u := strings.TrimSpace(r.FormValue("username"))
	p := r.FormValue("password")
	v := authView{head: head{Title: "Set up Registry", Version: s.version}, Host: s.host(r), Username: u}
	switch {
	case subtle.ConstantTimeCompare([]byte(strings.TrimSpace(r.FormValue("code"))), []byte(s.setupCode)) != 1:
		v.Error = "The setup code is not correct. You find it in the server log."
	case !usernameRe.MatchString(u):
		v.Error = "Use 1 to 39 letters, digits, dots, dashes, or underscores for the username."
	case len(p) < 10:
		v.Error = "Use at least 10 characters for the password."
	case p != r.FormValue("confirm"):
		v.Error = "The two passwords are not the same."
	}
	if v.Error != "" {
		s.render(w, http.StatusBadRequest, "setup.html", v)
		return
	}
	hash, err := bcrypt.GenerateFromPassword([]byte(p), bcrypt.DefaultCost)
	if err == nil {
		err = s.store.createAccount(u, hash)
	}
	if err != nil {
		v.Error = err.Error()
		s.render(w, http.StatusInternalServerError, "setup.html", v)
		return
	}
	s.log.Info("account created", zap.String("username", u))
	s.startSession(w, r, u)
	http.Redirect(w, r, "/", http.StatusSeeOther)
}

func (s *server) loginPage(w http.ResponseWriter, r *http.Request) {
	if !s.store.hasAccount() {
		http.Redirect(w, r, "/setup", http.StatusSeeOther)
		return
	}
	if s.user(r) != "" {
		http.Redirect(w, r, "/", http.StatusSeeOther)
		return
	}
	s.render(w, http.StatusOK, "login.html", authView{head: head{Title: "Sign in · Registry", Version: s.version}, Host: s.host(r)})
}

func (s *server) login(w http.ResponseWriter, r *http.Request) {
	acc := s.store.account()
	u := strings.TrimSpace(r.FormValue("username"))
	okPass := bcrypt.CompareHashAndPassword(acc.Hash, []byte(r.FormValue("password"))) == nil
	okUser := subtle.ConstantTimeCompare([]byte(u), []byte(acc.Username)) == 1
	if acc.Username == "" || !okPass || !okUser {
		s.log.Warn("sign-in failed", zap.String("username", u), zap.String("remote", r.RemoteAddr), zap.String("forwarded_for", r.Header.Get("X-Forwarded-For")))
		s.render(w, http.StatusUnauthorized, "login.html", authView{head: head{Title: "Sign in · Registry", Version: s.version}, Host: s.host(r), Username: u, Error: "The username or password is not correct."})
		return
	}
	s.startSession(w, r, acc.Username)
	http.Redirect(w, r, "/", http.StatusSeeOther)
}

func (s *server) logout(w http.ResponseWriter, r *http.Request) {
	http.SetCookie(w, &http.Cookie{Name: "session", Path: "/", MaxAge: -1})
	http.Redirect(w, r, "/login", http.StatusSeeOther)
}

func (s *server) setVisibility(w http.ResponseWriter, r *http.Request, user string) {
	name := r.FormValue("repo")
	public := r.FormValue("public") == "1"
	if err := s.store.setPublic(name, public); err != nil {
		back(w, r, "Could not save: "+err.Error())
		return
	}
	s.log.Info("visibility changed", zap.String("repo", name), zap.Bool("public", public), zap.String("by", user))
	back(w, r, name+" is now "+map[bool]string{true: "public", false: "private"}[public])
}

func (s *server) deleteTag(w http.ResponseWriter, r *http.Request, user string) {
	name := r.FormValue("repo")
	d, err := digest.Parse(r.FormValue("digest"))
	if err != nil {
		back(w, r, "Unknown digest")
		return
	}
	tags, err := s.reg.deleteManifest(r.Context(), name, d)
	if err != nil {
		back(w, r, "Could not delete: "+err.Error())
		return
	}
	s.store.record(event{Type: "delete", Repo: name, Ref: strings.Join(tags, ", "), Actor: user})
	back(w, r, "Deleted "+strings.Join(tags, ", "))
}

func (s *server) deleteRepo(w http.ResponseWriter, r *http.Request, user string) {
	name := r.FormValue("repo")
	if err := s.reg.deleteRepo(r.Context(), name); err != nil {
		back(w, r, "Could not delete: "+err.Error())
		return
	}
	_ = s.store.setPublic(name, false)
	s.store.record(event{Type: "delete", Repo: name, Actor: user})
	setFlash(w, "toast", "Deleted "+name)
	http.Redirect(w, r, "/", http.StatusSeeOther)
}

func (s *server) runGC(w http.ResponseWriter, r *http.Request, user string) {
	if !s.reg.gcBusy.CompareAndSwap(false, true) {
		back(w, r, "Garbage collection is already running")
		return
	}
	go func() {
		defer s.reg.gcBusy.Store(false)
		freed, blobs, err := s.reg.collect(context.Background())
		if err != nil {
			s.log.Error("garbage collection", zap.Error(err))
			return
		}
		s.store.addGC(gcRun{Time: time.Now().UTC(), Freed: freed, Blobs: blobs, By: user})
		s.store.record(event{Type: "gc", Size: freed, Actor: user})
	}()
	back(w, r, "Garbage collection started")
}

func (s *server) runBackup(w http.ResponseWriter, r *http.Request, user string) {
	if !s.backup.enabled {
		back(w, r, "Backup is not set up")
		return
	}
	go s.backup.enqueueAll(context.Background())
	s.log.Info("backup requested", zap.String("by", user))
	back(w, r, "Backup started")
}

func (s *server) createToken(w http.ResponseWriter, r *http.Request, user string) {
	name := strings.TrimSpace(r.FormValue("name"))
	if name == "" {
		back(w, r, "Give the token a name")
		return
	}
	var repos []string
	for p := range strings.FieldsFuncSeq(r.FormValue("repos"), func(c rune) bool { return c == ',' || c == ' ' || c == '\n' }) {
		if _, err := path.Match(p, ""); err != nil {
			back(w, r, "Bad repository pattern: "+p)
			return
		}
		repos = append(repos, p)
	}
	secret := "reg_" + rand.Text()
	t := &token{
		ID:      rand.Text()[:12],
		Name:    name,
		Hash:    hashSecret(secret),
		Hint:    secret[len(secret)-4:],
		Push:    r.FormValue("access") == "push",
		Repos:   repos,
		Created: time.Now().UTC(),
	}
	if days, _ := strconv.Atoi(r.FormValue("expires")); days > 0 {
		t.Expires = t.Created.AddDate(0, 0, days)
	}
	if err := s.store.addToken(t); err != nil {
		back(w, r, "Could not save: "+err.Error())
		return
	}
	s.log.Info("token created", zap.String("token", t.Name), zap.Bool("push", t.Push), zap.Strings("repos", t.Repos), zap.String("by", user))
	setFlash(w, "token", secret)
	back(w, r, "")
}

func (s *server) deleteToken(w http.ResponseWriter, r *http.Request, user string) {
	if err := s.store.deleteToken(r.FormValue("id")); err != nil {
		back(w, r, "Could not save: "+err.Error())
		return
	}
	s.log.Info("token revoked", zap.String("id", r.FormValue("id")), zap.String("by", user))
	setFlash(w, "tokens", "open")
	back(w, r, "Token revoked")
}

type part struct {
	label, color string
	n            int64
}

type statView struct{ Label, Value, Sub string }
type segView struct{ Label, Color, Value, Width string }
type barView struct{ Height, Label string }
type eventView struct{ Type, Label, Target, Ago, Title string }
type layerView struct{ Index, Cmd, Size, Width string }
type platView struct {
	Arch, Total string
	Layers      []layerView
}

type seriesView struct {
	Key, Idle string
	Pulls     string
	Pushes    string
	Egress    string
	Bars      []barView
	Week      bool
}

type repoRow struct {
	Name, NS, Base, Size string
	Tags                 int
	Public, On           bool
}

type tagView struct {
	Name, Digest, Short, Arch, Size, Ago string
	Type, Created, Revision              string
	Pull, PullDigest                     string
	Others                               []string
	Plats                                []platView
	Latest                               bool
}

type repoView struct {
	Name, NS, Base, Meta, Pull, Size string
	Public                           bool
	Tags                             []tagView
}

type tokenView struct {
	ID, Name, Access, Repos, Created, Used, Expires string
	Expired                                         bool
}

type dashView struct {
	head
	Host, Username, Login, Back, Query string
	Stats                              []statView
	Used, Capacity, Pct                string
	Warn, Disk                         bool
	Segs, Legend                       []segView
	Line, Area, Growth                 string
	Location                           string
	Backup, BackupFailed               bool
	BackupText, BackupTitle            string
	Reclaim, LastGC, LastFreed         string
	Loose                              int
	GCBusy                             bool
	Day, Week                          seriesView
	Events                             []eventView
	Repos                              []repoRow
	Sort                               string
	Repo                               *repoView
	Missing                            string
	Tokens                             []tokenView
	NewToken, Toast                    string
	OpenTokens                         bool
	RepoNames                          []string
	Access, Expires                    selectView
}

type option struct {
	Value, Label string
	Selected     bool
}

type selectView struct {
	ID, Name, Label string
	Options         []option
}

func (s selectView) Current() option {
	for _, o := range s.Options {
		if o.Selected {
			return o
		}
	}
	return s.Options[0]
}

var (
	accessSelect = selectView{ID: "token-access", Name: "access", Label: "Access", Options: []option{
		{Value: "pull", Label: "Pull", Selected: true},
		{Value: "push", Label: "Pull and push"},
	}}
	expiresSelect = selectView{ID: "token-expires", Name: "expires", Label: "Expires", Options: []option{
		{Value: "30", Label: "In 30 days", Selected: true},
		{Value: "90", Label: "In 90 days"},
		{Value: "365", Label: "In 1 year"},
		{Value: "0", Label: "Never"},
	}}
)

func fmtBytes(n int64) string {
	mb := float64(n) / (1 << 20)
	switch {
	case n <= 0:
		return "0 B"
	case n < 1<<10:
		return fmt.Sprintf("%d B", n)
	case mb < 1:
		return fmt.Sprintf("%d KB", max(1, n>>10))
	case mb >= 1024:
		return fmt.Sprintf("%.1f GB", mb/1024)
	case mb < 100:
		return fmt.Sprintf("%.1f MB", mb)
	default:
		return fmt.Sprintf("%.0f MB", mb)
	}
}

func fmtNum(n int) string {
	s := strconv.Itoa(n)
	for i := len(s) - 3; i > 0; i -= 3 {
		s = s[:i] + "," + s[i:]
	}
	return s
}

func ago(t time.Time) string {
	if t.IsZero() {
		return "never"
	}
	m := int(time.Since(t).Minutes())
	switch {
	case m < 1:
		return "just now"
	case m < 60:
		return fmt.Sprintf("%dm ago", m)
	case m < 48*60:
		return fmt.Sprintf("%dh ago", (m+30)/60)
	case m < 60*24*60:
		return fmt.Sprintf("%dd ago", (m+720)/1440)
	default:
		return fmt.Sprintf("%dmo ago", m/43200)
	}
}

func fmtDate(t time.Time) string {
	if t.IsZero() {
		return "—"
	}
	return t.UTC().Format("Jan 2, 15:04")
}

func splitName(name string) (string, string) {
	if i := strings.LastIndex(name, "/"); i >= 0 {
		return name[:i+1], name[i+1:]
	}
	return "", name
}

func pct(part, whole int64) string {
	if whole <= 0 || part <= 0 {
		return "0%"
	}
	return strconv.FormatFloat(float64(part)/float64(whole)*100, 'f', 2, 64) + "%"
}

func diskSpace(dir string) (int64, int64) {
	var st syscall.Statfs_t
	if syscall.Statfs(dir, &st) != nil {
		return 0, 0
	}
	return int64(st.Blocks) * int64(st.Bsize), int64(st.Bavail) * int64(st.Bsize)
}

func sparkline(vals []int64) (string, string) {
	if slices.Min(vals) == slices.Max(vals) {
		return "M0 80 L300 80", "M0 80 L300 80 L300 100 L0 100 Z"
	}
	lo, hi := float64(slices.Min(vals))*0.985, float64(slices.Max(vals))*1.01
	var b strings.Builder
	for i, v := range vals {
		_, _ = fmt.Fprintf(&b, "%s%.2f %.2f ", map[bool]string{true: "M", false: "L"}[i == 0], float64(i)/float64(len(vals)-1)*300, 94-(float64(v)-lo)/(hi-lo)*88)
	}
	line := strings.TrimSpace(b.String())
	return line, line + " L300 100 L0 100 Z"
}

func series(hours []hour, week bool) seriesView {
	byHour := map[int64]hour{}
	for _, h := range hours {
		byHour[h.Hour] = h
	}
	now := time.Now().UTC()
	type slot struct {
		label        string
		pulls, pushs int
		egress       int64
	}
	var slots []slot
	if week {
		today := now.Truncate(24 * time.Hour)
		for d := 6; d >= 0; d-- {
			day := today.AddDate(0, 0, -d)
			sl := slot{label: day.Format("Mon Jan 2")}
			for h := range 24 {
				b := byHour[day.Add(time.Duration(h)*time.Hour).Unix()/3600]
				sl.pulls, sl.pushs, sl.egress = sl.pulls+b.Pulls, sl.pushs+b.Pushes, sl.egress+b.Egress
			}
			slots = append(slots, sl)
		}
	} else {
		cur := now.Unix() / 3600
		for i := cur - 23; i <= cur; i++ {
			b := byHour[i]
			start := time.Unix(i*3600, 0).UTC()
			slots = append(slots, slot{label: start.Format("15:04") + "–" + start.Add(time.Hour).Format("15:04") + " UTC", pulls: b.Pulls, pushs: b.Pushes, egress: b.Egress})
		}
	}
	v := seriesView{Key: "24h", Idle: "Last 24 hours, by hour", Week: week}
	if week {
		v.Key, v.Idle = "7d", "Last 7 days, by day"
	}
	top, pulls, pushes, egress := 0, 0, 0, int64(0)
	for _, sl := range slots {
		top = max(top, sl.pulls)
		pulls, pushes, egress = pulls+sl.pulls, pushes+sl.pushs, egress+sl.egress
	}
	for _, sl := range slots {
		h := 3.0
		if top > 0 {
			h = max(3, float64(sl.pulls)/float64(top)*100)
		}
		v.Bars = append(v.Bars, barView{
			Height: strconv.FormatFloat(h, 'f', 1, 64) + "%",
			Label:  fmt.Sprintf("%s · %s pulls · %s pushes · %s", sl.label, fmtNum(sl.pulls), fmtNum(sl.pushs), fmtBytes(sl.egress)),
		})
	}
	v.Pulls, v.Pushes, v.Egress = fmtNum(pulls), fmtNum(pushes), fmtBytes(egress)
	return v
}

func typeLabel(mt string, plats int) string {
	switch mt {
	case v1.MediaTypeImageIndex:
		return fmt.Sprintf("OCI index · %d platforms", plats)
	case dockerList:
		return fmt.Sprintf("Docker manifest list · %d platforms", plats)
	case v1.MediaTypeImageManifest:
		return "OCI manifest"
	case dockerManifest:
		return "Docker manifest"
	}
	return mt
}

func (s *server) dashboard(w http.ResponseWriter, r *http.Request, user string) {
	sc, err := s.reg.snapshot(r.Context())
	if err != nil {
		http.Error(w, "read registry: "+err.Error(), http.StatusInternalServerError)
		return
	}
	used := sc.Total + sc.Uploads
	s.store.recordUsage(used)
	st := s.store.snapshot()
	host := s.host(r)

	v := dashView{
		head:     head{Title: "Registry", Version: s.version, Refresh: s.reg.gcBusy.Load()},
		Host:     host,
		Username: user,
		Login:    "docker login " + host + " -u " + user,
		Back:     r.URL.RequestURI(),
		Sort:     cmp.Or(r.URL.Query().Get("sort"), "recent"),
		Location: s.cfg.location(),
		Backup:   s.backup.enabled,
		Toast:    takeFlash(w, r, "toast"),
		NewToken: takeFlash(w, r, "token"),
		GCBusy:   s.reg.gcBusy.Load(),
	}
	v.OpenTokens = v.NewToken != "" || takeFlash(w, r, "tokens") != ""
	if v.Backup {
		v.BackupText, v.BackupTitle, v.BackupFailed = s.backupStatus(st)
	}
	v.Access, v.Expires = accessSelect, expiresSelect
	if v.Sort != "recent" {
		v.Query = "?sort=" + url.QueryEscape(v.Sort)
	}

	tags := 0
	for _, rp := range sc.Repos {
		tags += len(rp.Tags)
		v.RepoNames = append(v.RepoNames, rp.Name)
	}
	day, week := series(st.Hours, false), series(st.Hours, true)
	v.Day, v.Week = day, week

	reclaim := sc.Total - sc.Referenced
	v.Used, v.Reclaim, v.Loose = fmtBytes(used), fmtBytes(reclaim), sc.Loose
	parts := []part{{"Layers", "#6BBF8E", sc.Referenced}, {"Reclaimable", "#DDC38E", reclaim}}
	whole, free := max(used, 1), int64(0)
	if s.cfg.Storage.Driver == "filesystem" {
		if capacity, f := diskSpace(s.cfg.Storage.Filesystem.Root); capacity > 0 {
			whole, free = capacity, f
			full := float64(capacity-free) / float64(capacity) * 100
			v.Disk, v.Capacity = true, fmtBytes(capacity)
			v.Pct, v.Warn = fmt.Sprintf("%.0f%%", full), full > 85
			parts = append(parts, part{"Other files", "#4A4D52", max(0, capacity-free-sc.Total)})
		}
	}
	if !v.Disk {
		parts = append(parts, part{"Uploads", "#9FBCDC", sc.Uploads})
	}
	for _, p := range parts {
		sv := segView{Label: p.label, Color: p.color, Value: fmtBytes(p.n), Width: pct(p.n, whole)}
		v.Legend = append(v.Legend, sv)
		if p.n > 0 {
			v.Segs = append(v.Segs, sv)
		}
	}
	if v.Disk {
		v.Legend = append(v.Legend, segView{Label: "Free", Color: "#252629", Value: fmtBytes(free)})
	}

	days := slices.Sorted(maps.Keys(st.Usage))
	var points []int64
	for _, d := range days {
		points = append(points, st.Usage[d])
	}
	if len(points) == 0 {
		points = []int64{used}
	}
	v.Line, v.Area = sparkline(points)
	growth := used - points[0]
	sign := "+"
	if growth < 0 {
		sign, growth = "−", -growth
	}
	v.Growth = fmt.Sprintf("%s%s · %dd", sign, fmtBytes(growth), max(1, len(days)))

	v.LastGC, v.LastFreed = "Never", "—"
	if len(st.GC) > 0 {
		g := st.GC[0]
		v.LastGC = fmtDate(g.Time) + " UTC · " + g.By
		v.LastFreed = fmtBytes(g.Freed) + " · " + fmtNum(g.Blobs) + " blobs"
	}

	v.Stats = []statView{
		{Label: "Repositories", Value: fmtNum(len(sc.Repos))},
		{Label: "Tags", Value: fmtNum(tags)},
		{Label: "Pulls · 24h", Value: day.Pulls},
		{Label: "Egress · 24h", Value: day.Egress},
		{Label: "Storage", Value: v.Used},
	}
	if v.Disk {
		v.Stats[4].Sub = "of " + v.Capacity
	}

	labels := map[string]string{"push": "Push", "pull": "Pull", "delete": "Del", "gc": "GC"}
	for _, e := range st.Events[:min(len(st.Events), 60)] {
		ev := eventView{Type: e.Type, Label: labels[e.Type], Ago: ago(e.Time), Title: e.Actor + " · " + fmtDate(e.Time) + " UTC"}
		switch {
		case e.Type == "gc":
			ev.Target = fmtBytes(e.Size) + " freed"
		case e.Ref == "":
			ev.Target = e.Repo
		case strings.Contains(e.Ref, ":"):
			ev.Target = e.Repo + "@" + e.Ref[:min(len(e.Ref), 19)]
		default:
			ev.Target = e.Repo + ":" + e.Ref
		}
		v.Events = append(v.Events, ev)
	}

	list := slices.Clone(sc.Repos)
	switch v.Sort {
	case "size":
		slices.SortStableFunc(list, func(a, b *repo) int { return cmp.Compare(b.Size, a.Size) })
	case "name":
		slices.SortStableFunc(list, func(a, b *repo) int { return strings.Compare(a.Name, b.Name) })
	default:
		slices.SortStableFunc(list, func(a, b *repo) int { return b.Pushed.Compare(a.Pushed) })
	}
	selected := r.PathValue("name")
	if selected == "" && len(sc.Repos) > 0 {
		recent := slices.Clone(sc.Repos)
		slices.SortStableFunc(recent, func(a, b *repo) int { return b.Pushed.Compare(a.Pushed) })
		selected = recent[0].Name
	}
	for _, rp := range list {
		ns, base := splitName(rp.Name)
		v.Repos = append(v.Repos, repoRow{Name: rp.Name, NS: ns, Base: base, Size: fmtBytes(rp.Size), Tags: len(rp.Tags), Public: s.store.isPublic(rp.Name), On: rp.Name == selected})
	}
	if rp := sc.repo(selected); rp != nil {
		v.Repo = s.repoView(rp, host, st.Backups)
		v.Title = rp.Name + " · Registry"
	} else if selected != "" {
		v.Missing = selected
	}

	toks, usedAt := s.store.tokens()
	for _, t := range toks {
		tv := tokenView{ID: t.ID, Name: t.Name, Access: "Pull", Repos: "All repositories", Created: fmtDate(t.Created), Used: ago(usedAt[t.ID]), Expires: "Never expires", Expired: t.expired()}
		if t.Push {
			tv.Access = "Pull and push"
		}
		if len(t.Repos) > 0 {
			tv.Repos = strings.Join(t.Repos, ", ")
		}
		if !t.Expires.IsZero() {
			tv.Expires = "Expires " + t.Expires.Format("Jan 2, 2006")
			if tv.Expired {
				tv.Expires = "Expired " + t.Expires.Format("Jan 2, 2006")
			}
		}
		v.Tokens = append(v.Tokens, tv)
	}

	s.render(w, http.StatusOK, "dashboard.html", v)
}

func (s *server) backupStatus(st stats) (string, string, bool) {
	last, failed, firstErr := st.Settings, 0, ""
	for name, b := range st.Backups {
		if b.Time.After(last) {
			last = b.Time
		}
		if b.Error != "" {
			failed++
			if firstErr == "" {
				firstErr = name + ": " + b.Error
			}
		}
	}
	title := "Backup to " + s.backup.target + ". The newest image of each repository and the dashboard data."
	if !last.IsZero() {
		title += " Last run " + fmtDate(last) + " UTC."
	}
	switch {
	case s.backup.running():
		return "backing up", title, false
	case failed > 0:
		return fmt.Sprintf("backup failed for %d", failed), title + " " + firstErr, true
	case last.IsZero():
		return "backup pending", title, false
	}
	return "backup " + ago(last), title, false
}

func (s *server) repoView(rp *repo, host string, backups map[string]backupState) *repoView {
	ns, base := splitName(rp.Name)
	ref := host + "/" + rp.Name
	rv := &repoView{Name: rp.Name, NS: ns, Base: base, Size: fmtBytes(rp.Size), Public: s.store.isPublic(rp.Name), Pull: "docker pull " + ref}
	meta := []string{fmtBytes(rp.Size), fmt.Sprintf("%d tags", len(rp.Tags))}
	if len(rp.Tags) == 1 {
		meta[1] = "1 tag"
	}
	if !rp.Pushed.IsZero() {
		meta = append(meta, "pushed "+ago(rp.Pushed))
	}
	if s.backup.enabled && len(rp.Tags) > 0 {
		switch b := backups[rp.Name]; {
		case b.Error != "":
			meta = append(meta, "backup failed")
		case b.Time.IsZero():
			meta = append(meta, "backup pending")
		default:
			meta = append(meta, "backed up "+ago(b.Time))
		}
	}
	rv.Meta = strings.Join(meta, " · ")
	if len(rp.Tags) > 0 {
		def := rp.Tags[0].Name
		if slices.ContainsFunc(rp.Tags, func(t *tag) bool { return t.Name == "latest" }) {
			def = "latest"
		}
		rv.Pull += ":" + def
	}
	for _, t := range rp.Tags {
		tv := tagView{
			Name:       t.Name,
			Digest:     t.Digest.String(),
			Short:      t.Digest.Encoded()[:12],
			Size:       fmtBytes(t.Size()),
			Ago:        ago(t.Pushed),
			Type:       typeLabel(t.MediaType, len(t.Platforms)),
			Created:    fmtDate(t.Created) + " UTC",
			Revision:   t.Revision,
			Pull:       "docker pull " + ref + ":" + t.Name,
			PullDigest: "docker pull " + ref + "@" + t.Digest.String(),
			Latest:     t.Name == "latest",
		}
		if t.Created.IsZero() {
			tv.Created = "—"
		}
		if len(tv.Revision) > 12 {
			tv.Revision = tv.Revision[:7]
		}
		var arches []string
		for _, p := range t.Platforms {
			arches = append(arches, p.Arch)
			top := int64(1)
			for _, l := range p.Layers {
				top = max(top, l.Size)
			}
			pv := platView{Arch: p.Arch, Total: fmtBytes(p.Size)}
			for i, l := range p.Layers {
				w := "0%"
				if l.Size > 0 {
					w = strconv.FormatFloat(max(4, float64(l.Size)/float64(top)*100), 'f', 1, 64) + "%"
				}
				pv.Layers = append(pv.Layers, layerView{Index: fmt.Sprintf("%02d", i+1), Cmd: l.Cmd, Size: fmtBytes(l.Size), Width: w})
			}
			tv.Plats = append(tv.Plats, pv)
		}
		tv.Arch = strings.Join(arches, ", ")
		for _, o := range rp.Tags {
			if o.Digest == t.Digest && o.Name != t.Name {
				tv.Others = append(tv.Others, o.Name)
			}
		}
		rv.Tags = append(rv.Tags, tv)
	}
	return rv
}
