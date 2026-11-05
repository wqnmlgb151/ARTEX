package server

import (
	"context"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/hex"
	"net"
	"net/http"
	"net/url"
	"os"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/golang-jwt/jwt/v5"
)

// The password hash is a persistent credential version. A successful atomic
// password replacement revokes every JWT for the previous version, across restarts.
func authEpoch(hash string) string {
	sum := sha256.Sum256([]byte(hash))
	return hex.EncodeToString(sum[:])
}

type authClaims struct {
	Epoch string `json:"epoch"`
	jwt.RegisteredClaims
}

func signJWT(key []byte, hash string) (string, error) {
	return jwt.NewWithClaims(jwt.SigningMethodHS256, authClaims{Epoch: authEpoch(hash), RegisteredClaims: jwt.RegisteredClaims{
		Subject: "ARTEX", ExpiresAt: jwt.NewNumericDate(time.Now().Add(jwtTTL)), IssuedAt: jwt.NewNumericDate(time.Now()),
	}}).SignedString(key)
}

func validSession(token string, key []byte, hash string) bool {
	if token == "" || hash == "" {
		return false
	}
	var claims authClaims
	t, err := jwt.ParseWithClaims(token, &claims, func(*jwt.Token) (any, error) { return key, nil }, jwt.WithValidMethods([]string{"HS256"}), jwt.WithExpirationRequired())
	return err == nil && t.Valid && claims.Subject == "ARTEX" && subtle.ConstantTimeCompare([]byte(claims.Epoch), []byte(authEpoch(hash))) == 1
}

func extractToken(r *http.Request) string {
	if h := r.Header.Get("Authorization"); strings.HasPrefix(h, "Bearer ") {
		return strings.TrimPrefix(h, "Bearer ")
	}
	if c, err := r.Cookie("artex_token"); err == nil {
		return c.Value
	}
	return ""
}

func (s *Server) requireAuth(h http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Cache-Control", "no-store")
		switch r.URL.Path {
		case "/api/auth/status", "/api/auth/login", "/api/auth/init", "/api/auth/logout", "/api/health":
			h.ServeHTTP(w, r)
			return
		}
		pg := s.pg(w)
		if pg == nil {
			return
		}
		hash, _, err := pg.GetSetting(authPassKey)
		if err != nil {
			writeErr(w, 503, errDataSourceUnavailable)
			return
		}
		token := extractToken(r)
		if !validSession(token, s.jwtKey, hash) {
			writeErr(w, 401, "会话无效或已过期，请重新登录")
			return
		}
		// An established stream is also bounded by credential revocation. New
		// requests fail immediately; open streams close on the next one-second check.
		if strings.Contains(r.Header.Get("Accept"), "text/event-stream") {
			ctx, cancel := context.WithCancel(r.Context())
			defer cancel()
			r = r.WithContext(ctx)
			go func() {
				ticker := time.NewTicker(time.Second)
				defer ticker.Stop()
				for {
					select {
					case <-ctx.Done():
						return
					case <-ticker.C:
						check, stop := context.WithTimeout(ctx, 5*time.Second)
						var current string
						err := pg.QueryRowContext(check, `SELECT value FROM settings WHERE key=$1`, authPassKey).Scan(&current)
						stop()
						if err != nil || !validSession(token, s.jwtKey, current) {
							cancel()
							return
						}
					}
				}
			}()
		}
		h.ServeHTTP(w, r)
	})
}

func trustedProxy(ip string) bool {
	addr := net.ParseIP(ip)
	if addr == nil {
		return false
	}
	for _, v := range strings.Split(os.Getenv("ARTEX_TRUSTED_PROXIES"), ",") {
		v = strings.TrimSpace(v)
		if _, block, err := net.ParseCIDR(v); err == nil && block.Contains(addr) {
			return true
		}
		if addr.Equal(net.ParseIP(v)) {
			return true
		}
	}
	return false
}

func remoteIP(r *http.Request) string {
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		return r.RemoteAddr
	}
	return host
}
func clientIP(r *http.Request) string {
	ip := remoteIP(r)
	chain := strings.Split(r.Header.Get("X-Forwarded-For"), ",")
	for i := len(chain) - 1; i >= 0 && trustedProxy(ip); i-- {
		next := strings.TrimSpace(chain[i])
		if net.ParseIP(next) == nil {
			break
		}
		ip = next
	}
	return ip
}
func secureRequest(r *http.Request) bool {
	return os.Getenv("ARTEX_COOKIE_SECURE") == "1" || r.TLS != nil || (trustedProxy(remoteIP(r)) && r.Header.Get("X-Forwarded-Proto") == "https")
}
func setSessionCookie(w http.ResponseWriter, r *http.Request, token string) {
	age := int(jwtTTL.Seconds())
	expires := time.Now().Add(jwtTTL)
	if token == "" {
		age = -1
		expires = time.Unix(1, 0)
	}
	http.SetCookie(w, &http.Cookie{Name: "artex_token", Value: token, Path: "/", HttpOnly: true, Secure: secureRequest(r), SameSite: http.SameSiteLaxMode, MaxAge: age, Expires: expires})
}
func (s *Server) authLogout(w http.ResponseWriter, r *http.Request) {
	setSessionCookie(w, r, "")
	writeJSON(w, 200, map[string]bool{"ok": true})
}

func allowedOrigin(r *http.Request, origin string) bool {
	u, err := url.Parse(origin)
	if err != nil || u.Host == "" || u.User != nil || u.RawQuery != "" || u.Fragment != "" || u.Path != "" || (u.Scheme != "http" && u.Scheme != "https") {
		return false
	}
	scheme := "http"
	if secureRequest(r) {
		scheme = "https"
	}
	if origin == scheme+"://"+r.Host {
		return true
	}
	for _, v := range strings.Split(os.Getenv("ARTEX_ALLOWED_ORIGINS"), ",") {
		if origin == strings.TrimSpace(v) {
			return true
		}
	}
	// Local Next.js development uses a streaming connection to Go on :8787.
	return (origin == "http://localhost:3000" || origin == "http://127.0.0.1:3000") && (strings.Split(r.Host, ":")[0] == u.Hostname())
}

func cors(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Add("Vary", "Origin")
		origin := r.Header.Get("Origin")
		if origin != "" {
			if !allowedOrigin(r, origin) {
				writeErr(w, 403, "请求来源不受信任")
				return
			}
			w.Header().Set("Access-Control-Allow-Origin", origin)
			w.Header().Set("Access-Control-Allow-Credentials", "true")
			w.Header().Set("Access-Control-Allow-Methods", "GET,POST,PUT,PATCH,DELETE,OPTIONS")
			w.Header().Set("Access-Control-Allow-Headers", "Content-Type,Authorization,X-Artex-CSRF")
		}
		if r.Method == http.MethodOptions {
			w.WriteHeader(204)
			return
		}
		if r.Method != "GET" && r.Method != "HEAD" {
			// Non-browser Bearer clients retain their API authentication protocol.
			if !strings.HasPrefix(r.Header.Get("Authorization"), "Bearer ") && r.Header.Get("X-Artex-CSRF") != "1" {
				writeErr(w, 403, "缺少 CSRF 请求校验")
				return
			}
		}
		next.ServeHTTP(w, r)
	})
}

type loginWindow struct {
	failures int
	start    time.Time
}
type loginLimiter struct {
	mu      sync.Mutex
	clients map[string]loginWindow
	account loginWindow
	now     func() time.Time
}

// Caller holds the limiter through credential verification: concurrent requests
// cannot all pass the threshold before any failure is accounted for.
func (s *Server) beginLogin(w http.ResponseWriter, r *http.Request) (func(bool), bool) {
	l := &s.loginLimit
	l.mu.Lock()
	now := time.Now()
	if l.now != nil {
		now = l.now()
	}
	if l.clients == nil {
		l.clients = map[string]loginWindow{}
	}
	for ip, v := range l.clients {
		if now.Sub(v.start) >= 15*time.Minute {
			delete(l.clients, ip)
		}
	}
	if now.Sub(l.account.start) >= 15*time.Minute {
		l.account = loginWindow{start: now}
	}
	ip := clientIP(r)
	v := l.clients[ip]
	if v.start.IsZero() {
		v.start = now
	}
	until := time.Time{}
	if v.failures >= 5 {
		until = v.start.Add(15 * time.Minute)
	}
	if l.account.failures >= 20 && l.account.start.Add(15*time.Minute).After(until) {
		until = l.account.start.Add(15 * time.Minute)
	}
	if until.After(now) {
		l.mu.Unlock()
		w.Header().Set("Retry-After", strconv.Itoa(int(until.Sub(now).Seconds())+1))
		writeErr(w, 429, "登录失败次数过多，请稍后重试")
		return nil, false
	}
	return func(failed bool) {
		if failed {
			if len(l.clients) >= 4096 {
				for key := range l.clients {
					delete(l.clients, key)
					break
				}
			}
			v.failures++
			l.clients[ip] = v
			l.account.failures++
		}
		l.mu.Unlock()
	}, true
}
