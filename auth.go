package main

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"regexp"
	"strconv"
	"strings"
	"time"
)

const (
	bearerTTL  = 10 * time.Minute
	sessionTTL = 7 * 24 * time.Hour
)

var v2Path = regexp.MustCompile(`^/v2/(.+)/(manifests|blobs|tags|referrers)/(.*)$`)

func (s *server) mac(msg string) string {
	m := hmac.New(sha256.New, s.store.secret())
	m.Write([]byte(msg))
	return base64.RawURLEncoding.EncodeToString(m.Sum(nil))
}

func (s *server) sign(kind, value string, ttl time.Duration) string {
	msg := kind + "|" + value + "|" + strconv.FormatInt(time.Now().Add(ttl).Unix(), 10)
	return base64.RawURLEncoding.EncodeToString([]byte(msg)) + "." + s.mac(msg)
}

func (s *server) verify(kind, signed string) (string, bool) {
	enc, sum, ok := strings.Cut(signed, ".")
	if !ok {
		return "", false
	}
	raw, err := base64.RawURLEncoding.DecodeString(enc)
	if err != nil || !hmac.Equal([]byte(sum), []byte(s.mac(string(raw)))) {
		return "", false
	}
	parts := strings.Split(string(raw), "|")
	if len(parts) != 3 || parts[0] != kind {
		return "", false
	}
	exp, err := strconv.ParseInt(parts[2], 10, 64)
	if err != nil || time.Now().Unix() > exp {
		return "", false
	}
	return parts[1], true
}

func (s *server) host(r *http.Request) string {
	if u, err := url.Parse(s.cfg.Server.PublicURL); err == nil && u.Host != "" {
		return u.Host
	}
	return r.Host
}

func (s *server) secure(r *http.Request) bool {
	return r.TLS != nil || r.Header.Get("X-Forwarded-Proto") == "https" || strings.HasPrefix(s.cfg.Server.PublicURL, "https://")
}

func (s *server) baseURL(r *http.Request) string {
	if s.cfg.Server.PublicURL != "" {
		return s.cfg.Server.PublicURL
	}
	if s.secure(r) {
		return "https://" + r.Host
	}
	return "http://" + r.Host
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

func registryError(w http.ResponseWriter, status int, code, message string) {
	writeJSON(w, status, map[string]any{"errors": []map[string]string{{"code": code, "message": message}}})
}

func (s *server) identify(r *http.Request) (*token, bool) {
	if v, ok := strings.CutPrefix(r.Header.Get("Authorization"), "Bearer "); ok {
		id, ok := s.verify("bearer", v)
		if !ok {
			return nil, false
		}
		if id == "" {
			return nil, true
		}
		t := s.store.tokenByID(id)
		return t, t != nil
	}
	if _, p, ok := r.BasicAuth(); ok {
		t := s.store.tokenBySecret(p)
		return t, t != nil
	}
	return nil, false
}

func (s *server) can(t *token, repo, action string) bool {
	if action == "pull" && s.store.isPublic(repo) {
		return true
	}
	return t != nil && t.covers(repo) && (action == "pull" || t.Push)
}

func (s *server) challenge(w http.ResponseWriter, r *http.Request, scope string) {
	h := fmt.Sprintf(`Bearer realm="%s/token",service="%s"`, s.baseURL(r), s.host(r))
	if scope != "" {
		h += fmt.Sprintf(`,scope="%s"`, scope)
	}
	w.Header().Set("WWW-Authenticate", h)
	registryError(w, http.StatusUnauthorized, "UNAUTHORIZED", "authentication required")
}

func (s *server) deny(w http.ResponseWriter, r *http.Request, t *token, scope string) {
	if t != nil {
		registryError(w, http.StatusForbidden, "DENIED", "requested access to the resource is denied")
		return
	}
	s.challenge(w, r, scope)
}

func (s *server) handleToken(w http.ResponseWriter, r *http.Request) {
	secret := ""
	if _, p, ok := r.BasicAuth(); ok {
		secret = p
	}
	if r.Method == http.MethodPost {
		_ = r.ParseForm()
		if r.PostForm.Get("grant_type") != "password" {
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": "unsupported_grant_type"})
			return
		}
		secret = r.PostForm.Get("password")
	}
	id := ""
	if secret != "" {
		t := s.store.tokenBySecret(secret)
		if t == nil {
			registryError(w, http.StatusUnauthorized, "UNAUTHORIZED", "invalid username or token")
			return
		}
		id = t.ID
	}
	signed := s.sign("bearer", id, bearerTTL)
	writeJSON(w, http.StatusOK, map[string]any{
		"token":        signed,
		"access_token": signed,
		"expires_in":   int(bearerTTL.Seconds()),
		"issued_at":    time.Now().UTC().Format(time.RFC3339),
	})
}

type recorder struct {
	http.ResponseWriter
	status int
	bytes  int64
	wrote  bool
}

func (r *recorder) WriteHeader(code int) {
	if !r.wrote {
		r.status, r.wrote = code, true
	}
	r.ResponseWriter.WriteHeader(code)
}

func (r *recorder) Write(b []byte) (int, error) {
	r.wrote = true
	n, err := r.ResponseWriter.Write(b)
	r.bytes += int64(n)
	return n, err
}

func (r *recorder) Flush() {
	if f, ok := r.ResponseWriter.(http.Flusher); ok {
		f.Flush()
	}
}

func (r *recorder) Unwrap() http.ResponseWriter {
	return r.ResponseWriter
}

func (s *server) guard(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Docker-Distribution-API-Version", "registry/2.0")
		t, ok := s.identify(r)
		action := "pull"
		if r.Method != http.MethodGet && r.Method != http.MethodHead {
			action = "push"
		}
		m := v2Path.FindStringSubmatch(r.URL.Path)
		switch {
		case m != nil:
			if !s.can(t, m[1], action) {
				s.deny(w, r, t, "repository:"+m[1]+":"+map[string]string{"pull": "pull", "push": "pull,push"}[action])
				return
			}
			if from := r.URL.Query().Get("from"); from != "" && !s.can(t, from, "pull") {
				q := r.URL.Query()
				q.Del("from")
				q.Del("mount")
				r.URL.RawQuery = q.Encode()
			}
		case r.URL.Path == "/v2/_catalog":
			if t == nil || len(t.Repos) > 0 {
				s.deny(w, r, t, "registry:catalog:*")
				return
			}
		case !ok:
			s.challenge(w, r, "")
			return
		}
		if action == "push" {
			if !s.reg.gcLock.TryRLock() {
				registryError(w, http.StatusServiceUnavailable, "UNAVAILABLE", "garbage collection is running, try again soon")
				return
			}
			defer s.reg.gcLock.RUnlock()
		}
		rec := &recorder{ResponseWriter: w, status: http.StatusOK}
		next.ServeHTTP(rec, r)
		s.track(r, m, t, rec)
	})
}

func (s *server) track(r *http.Request, m []string, t *token, rec *recorder) {
	actor := "anonymous"
	if t != nil {
		actor = t.Name
	}
	pulls, pushes := 0, 0
	if m != nil && m[2] == "manifests" {
		name, ref := m[1], m[3]
		isTag := !strings.Contains(ref, ":")
		switch {
		case r.Method == http.MethodGet && rec.status == http.StatusOK && (isTag || isIndex(rec.Header().Get("Content-Type"))):
			pulls = 1
			s.store.record(event{Type: "pull", Repo: name, Ref: ref, Actor: actor})
		case r.Method == http.MethodPut && rec.status == http.StatusCreated:
			s.reg.stale.Store(true)
			if isTag {
				pushes = 1
				s.store.record(event{Type: "push", Repo: name, Ref: ref, Actor: actor})
			}
		case r.Method == http.MethodDelete && rec.status == http.StatusAccepted:
			s.reg.stale.Store(true)
			s.store.record(event{Type: "delete", Repo: name, Ref: ref, Actor: actor})
		}
	}
	var egress int64
	if r.Method == http.MethodGet {
		egress = rec.bytes
	}
	if pulls+pushes > 0 || egress > 0 {
		s.store.count(pulls, pushes, egress)
	}
}

func (s *server) user(r *http.Request) string {
	c, err := r.Cookie("session")
	if err != nil {
		return ""
	}
	u, ok := s.verify("session", c.Value)
	if !ok || u != s.store.account().Username {
		return ""
	}
	return u
}

func (s *server) startSession(w http.ResponseWriter, r *http.Request, username string) {
	http.SetCookie(w, &http.Cookie{
		Name:     "session",
		Value:    s.sign("session", username, sessionTTL),
		Path:     "/",
		MaxAge:   int(sessionTTL.Seconds()),
		HttpOnly: true,
		Secure:   s.secure(r),
		SameSite: http.SameSiteLaxMode,
	})
}
