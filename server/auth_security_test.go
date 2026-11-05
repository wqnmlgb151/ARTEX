package server

import (
	"database/sql"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/Autumn-27/artex/db"
	"github.com/golang-jwt/jwt/v5"
)

func authTestServer(t *testing.T) *Server {
	t.Helper()
	dsn, _, err := db.DSN()
	if err != nil {
		t.Skipf("postgres config unavailable: %v", err)
	}
	conn, err := sql.Open("pgx", dsn)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { conn.Close() })
	conn.SetMaxOpenConns(1)
	if _, err = conn.Exec(`CREATE TEMP TABLE settings(key TEXT PRIMARY KEY,value TEXT NOT NULL,updated_at TIMESTAMPTZ DEFAULT now())`); err != nil {
		t.Fatal(err)
	}
	return &Server{m: &Manager{pg: &db.DB{DB: conn}}, jwtKey: []byte("test-signing-key-with-at-least-32-bytes")}
}

func authRequest(s *Server, method, path, body string, cookie *http.Cookie) *httptest.ResponseRecorder {
	mux := http.NewServeMux()
	mux.HandleFunc("POST /api/auth/init", s.authInit)
	mux.HandleFunc("POST /api/auth/login", s.authLogin)
	mux.HandleFunc("POST /api/auth/change-password", s.authChangePassword)
	mux.HandleFunc("POST /api/auth/logout", s.authLogout)
	mux.HandleFunc("GET /api/auth/status", s.authStatus)
	mux.HandleFunc("GET /api/private", func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(200) })
	r := httptest.NewRequest(method, path, strings.NewReader(body))
	r.RemoteAddr = "192.0.2.1:9000"
	r.Header.Set("X-Artex-CSRF", "1")
	if cookie != nil {
		r.AddCookie(cookie)
	}
	w := httptest.NewRecorder()
	cors(s.requireAuth(mux)).ServeHTTP(w, r)
	return w
}

func TestAuthCookieRevocationAndRateLimit(t *testing.T) {
	s := authTestServer(t)
	w := authRequest(s, "POST", "/api/auth/init", `{"password":"password-one"}`, nil)
	if w.Code != 200 {
		t.Fatal(w.Code, w.Body.String())
	}
	cookies := w.Result().Cookies()
	if len(cookies) != 1 {
		t.Fatal("missing session cookie")
	}
	old := cookies[0]
	if !old.HttpOnly || old.Path != "/" || old.SameSite != http.SameSiteLaxMode {
		t.Fatalf("unsafe cookie: %+v", old)
	}
	if strings.Contains(w.Body.String(), old.Value) {
		t.Fatal("JWT in response body")
	}
	if w = authRequest(s, "GET", "/api/private", "", old); w.Code != 200 {
		t.Fatal(w.Code)
	}
	w = authRequest(s, "POST", "/api/auth/change-password", `{"old_password":"password-one","new_password":"password-two"}`, old)
	if w.Code != 200 {
		t.Fatal(w.Code, w.Body.String())
	}
	if w = authRequest(s, "GET", "/api/private", "", old); w.Code != 401 {
		t.Fatalf("revoked token allowed: %d", w.Code)
	}
	// Reconstructing the server must not revive the old session.
	restarted := &Server{m: s.m, jwtKey: s.jwtKey}
	if w = authRequest(restarted, "GET", "/api/private", "", old); w.Code != 401 {
		t.Fatal("revoked token survived restart")
	}
	for i := 0; i < 5; i++ {
		w = authRequest(restarted, "POST", "/api/auth/login", `{"username":"ARTEX","password":"wrong"}`, nil)
		if w.Code != 401 {
			t.Fatalf("failure %d: %d", i, w.Code)
		}
	}
	w = authRequest(restarted, "POST", "/api/auth/login", `{"username":"ARTEX","password":"password-two"}`, nil)
	if w.Code != 429 || w.Header().Get("Retry-After") == "" {
		t.Fatalf("rate limit: %d", w.Code)
	}
	restarted.loginLimit.now = func() time.Time { return time.Now().Add(16 * time.Minute) }
	w = authRequest(restarted, "POST", "/api/auth/login", `{"username":"ARTEX","password":"password-two"}`, nil)
	if w.Code != 200 {
		t.Fatal("login did not recover", w.Code)
	}
	current := w.Result().Cookies()[0]
	w = authRequest(restarted, "GET", "/api/auth/status", "", current)
	var status struct{ Authenticated bool }
	if err := json.Unmarshal(w.Body.Bytes(), &status); err != nil || !status.Authenticated {
		t.Fatal("status not authenticated")
	}
	w = authRequest(restarted, "POST", "/api/auth/logout", "", current)
	if w.Code != 200 || w.Result().Cookies()[0].MaxAge >= 0 {
		t.Fatal("logout did not clear cookie")
	}
	if err := s.m.pg.Close(); err != nil {
		t.Fatal(err)
	}
	if w = authRequest(restarted, "GET", "/api/private", "", current); w.Code != 503 {
		t.Fatalf("database failure allowed session: %d", w.Code)
	}
}

func TestAuthCSRFAndTrustedProxy(t *testing.T) {
	h := cors(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(200) }))
	for _, origin := range []string{"https://evil.example", "null"} {
		r := httptest.NewRequest("POST", "http://localhost:8787/api/test", nil)
		r.Header.Set("Origin", origin)
		r.Header.Set("X-Artex-CSRF", "1")
		w := httptest.NewRecorder()
		h.ServeHTTP(w, r)
		if w.Code != 403 {
			t.Fatalf("cross site accepted: %s %d", origin, w.Code)
		}
	}
	r := httptest.NewRequest("POST", "http://localhost:8787/api/test", nil)
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)
	if w.Code != 403 {
		t.Fatal("missing CSRF header accepted")
	}
	r = httptest.NewRequest("GET", "http://localhost:8787/api/events", nil)
	r.Header.Set("Origin", "http://localhost:3000")
	w = httptest.NewRecorder()
	h.ServeHTTP(w, r)
	if w.Header().Get("Access-Control-Allow-Origin") != "http://localhost:3000" || w.Header().Get("Access-Control-Allow-Credentials") != "true" {
		t.Fatal("dev SSE credentials CORS missing")
	}
	r.RemoteAddr = "192.0.2.1:9000"
	r.Header.Set("X-Forwarded-For", "198.51.100.2")
	r.Header.Set("X-Forwarded-Proto", "https")
	if clientIP(r) != "192.0.2.1" || secureRequest(r) {
		t.Fatal("untrusted proxy spoof accepted")
	}
	t.Setenv("ARTEX_TRUSTED_PROXIES", "192.0.2.0/24")
	if clientIP(r) != "198.51.100.2" || !secureRequest(r) {
		t.Fatal("trusted proxy ignored")
	}
	if extractToken(httptest.NewRequest("GET", "/api/events?token=secret", nil)) != "" {
		t.Fatal("URL token accepted")
	}
}

func TestAuthConcurrentFailuresAndAccountLimit(t *testing.T) {
	s := authTestServer(t)
	if w := authRequest(s, "POST", "/api/auth/init", `{"password":"password-one"}`, nil); w.Code != 200 {
		t.Fatal(w.Code)
	}
	var wrong, limited atomic.Int32
	var wg sync.WaitGroup
	for i := 0; i < 12; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			w := authRequest(s, "POST", "/api/auth/login", `{"username":"ARTEX","password":"wrong"}`, nil)
			switch w.Code {
			case 401:
				wrong.Add(1)
			case 429:
				limited.Add(1)
			default:
				t.Errorf("unexpected response %d", w.Code)
			}
		}()
	}
	wg.Wait()
	if wrong.Load() != 5 || limited.Load() != 7 {
		t.Fatalf("concurrent attempts: wrong=%d limited=%d", wrong.Load(), limited.Load())
	}
	for i := 0; i < 15; i++ {
		r := httptest.NewRequest("POST", "/api/auth/login", strings.NewReader(`{"username":"wrong","password":"wrong"}`))
		r.RemoteAddr = fmt.Sprintf("198.51.100.%d:9000", i)
		w := httptest.NewRecorder()
		s.authLogin(w, r)
		if w.Code != 401 {
			t.Fatal("account threshold premature", w.Code)
		}
	}
	r := httptest.NewRequest("POST", "/api/auth/login", strings.NewReader(`{"username":"ARTEX","password":"password-one"}`))
	r.RemoteAddr = "203.0.113.1:9000"
	w := httptest.NewRecorder()
	s.authLogin(w, r)
	if w.Code != 429 {
		t.Fatal("distributed guessing not limited", w.Code)
	}
}

func TestAuthConcurrentPasswordChangeAndStreamRevocation(t *testing.T) {
	s := authTestServer(t)
	w := authRequest(s, "POST", "/api/auth/init", `{"password":"password-one"}`, nil)
	if w.Code != 200 {
		t.Fatal(w.Code)
	}
	cookie := w.Result().Cookies()[0]
	streamDone := make(chan struct{})
	started := make(chan struct{})
	r := httptest.NewRequest("GET", "/api/stream", nil)
	r.AddCookie(cookie)
	r.Header.Set("Accept", "text/event-stream")
	go func() {
		defer close(streamDone)
		s.requireAuth(http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) { close(started); <-r.Context().Done() })).ServeHTTP(httptest.NewRecorder(), r)
	}()
	<-started
	var successes atomic.Int32
	var wg sync.WaitGroup
	for i := 0; i < 2; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			w := authRequest(s, "POST", "/api/auth/change-password", `{"old_password":"password-one","new_password":"password-two"}`, cookie)
			if w.Code == 200 {
				successes.Add(1)
			} else if w.Code != 401 && w.Code != 409 {
				t.Errorf("concurrent change status %d", w.Code)
			}
		}()
	}
	wg.Wait()
	if successes.Load() != 1 {
		t.Fatalf("password replacements=%d", successes.Load())
	}
	select {
	case <-streamDone:
	case <-time.After(7 * time.Second):
		t.Fatal("revoked SSE stream stayed open")
	}
	hash, _, err := s.m.pg.GetSetting(authPassKey)
	if err != nil {
		t.Fatal(err)
	}
	legacy, err := jwt.NewWithClaims(jwt.SigningMethodHS256, jwt.RegisteredClaims{Subject: "ARTEX", ExpiresAt: jwt.NewNumericDate(time.Now().Add(time.Hour))}).SignedString(s.jwtKey)
	if err != nil {
		t.Fatal(err)
	}
	if validSession(legacy, s.jwtKey, hash) {
		t.Fatal("pre-upgrade token accepted")
	}
	t.Setenv("ARTEX_COOKIE_SECURE", "1")
	w = authRequest(s, "POST", "/api/auth/login", `{"username":"ARTEX","password":"password-two"}`, nil)
	if w.Code != 200 || !w.Result().Cookies()[0].Secure {
		t.Fatal("HTTPS cookie not Secure")
	}
}
