package console

import (
	"crypto/hmac"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"encoding/json"
	"net/http"
	"strings"
	"sync"
	"time"
)

const sessionCookie = "ac_session"

type authPayload struct {
	Exp  int64  `json:"exp"`
	CSRF string `json:"csrf"`
}

type authInfo struct {
	CSRF string
	Exp  int64
}

type loginBucket struct {
	fails int
	last  time.Time
	until time.Time
}

type authManager struct {
	cfg     *Config
	stateMu sync.RWMutex
	mu      sync.Mutex
	login   map[string]*loginBucket
}

func newAuthManager(cfg *Config) *authManager {
	return &authManager{cfg: cfg, login: map[string]*loginBucket{}}
}

func (a *authManager) token() string {
	a.stateMu.RLock()
	ttl := a.cfg.SessionTTLHours
	a.stateMu.RUnlock()
	p := authPayload{
		Exp:  time.Now().Add(time.Duration(ttl) * time.Hour).Unix(),
		CSRF: randomID(24),
	}
	b, _ := json.Marshal(p)
	body := base64.RawURLEncoding.EncodeToString(b)
	sig := a.sign([]byte(body))
	return body + "." + base64.RawURLEncoding.EncodeToString(sig)
}

func (a *authManager) sign(data []byte) []byte {
	a.stateMu.RLock()
	defer a.stateMu.RUnlock()
	return a.cfg.sign(data)
}

func (a *authManager) verifyPassword(pw string) bool {
	a.stateMu.RLock()
	defer a.stateMu.RUnlock()
	return verifyPassword(a.cfg.PasswordHash, pw)
}

// mustChangePassword reports whether the default password still has to be
// replaced before normal API use is allowed.
func (a *authManager) mustChangePassword() bool {
	a.stateMu.RLock()
	defer a.stateMu.RUnlock()
	return a.cfg.MustChangePassword
}

// changePassword updates the hash in memory. The caller persists the config.
// setConfig points the manager at the config currently published by Server.
// Settings changes use copy-on-write, so without this the manager would keep
// validating against a stale snapshot.
func (a *authManager) setConfig(c *Config) {
	a.stateMu.Lock()
	a.cfg = c
	a.stateMu.Unlock()
}

func (a *authManager) changePassword(newHash string, rotateSecret bool) {
	a.stateMu.Lock()
	defer a.stateMu.Unlock()
	a.cfg.PasswordHash = newHash
	a.cfg.MustChangePassword = false
	if rotateSecret {
		if b, err := randomBytes(32); err == nil {
			a.cfg.Secret = base64.StdEncoding.EncodeToString(b)
		}
	}
}

func (a *authManager) verifyToken(tok string) (*authInfo, bool) {
	parts := strings.Split(tok, ".")
	if len(parts) != 2 {
		return nil, false
	}
	rawBody := []byte(parts[0])
	body, err := base64.RawURLEncoding.DecodeString(parts[0])
	if err != nil {
		return nil, false
	}
	sig, err := base64.RawURLEncoding.DecodeString(parts[1])
	if err != nil {
		return nil, false
	}
	want := a.sign(rawBody)
	if !hmac.Equal(sig, want) {
		return nil, false
	}
	var p authPayload
	if json.Unmarshal(body, &p) != nil || p.Exp < time.Now().Unix() {
		return nil, false
	}
	return &authInfo{CSRF: p.CSRF, Exp: p.Exp}, true
}

func (a *authManager) authenticate(r *http.Request) (*authInfo, bool) {
	c, err := r.Cookie(sessionCookie)
	if err != nil {
		return nil, false
	}
	return a.verifyToken(c.Value)
}

func (a *authManager) setCookie(w http.ResponseWriter) string {
	tok := a.token()
	a.stateMu.RLock()
	ttl := a.cfg.SessionTTLHours
	a.stateMu.RUnlock()
	http.SetCookie(w, &http.Cookie{
		Name: sessionCookie, Value: tok, Path: "/", HttpOnly: true,
		SameSite: http.SameSiteStrictMode,
		MaxAge:   ttl * 3600,
	})
	if info, ok := a.verifyToken(tok); ok {
		return info.CSRF
	}
	return ""
}

func (a *authManager) clearCookie(w http.ResponseWriter) {
	http.SetCookie(w, &http.Cookie{Name: sessionCookie, Value: "", Path: "/", MaxAge: -1, HttpOnly: true})
}

func (a *authManager) checkCSRF(r *http.Request, info *authInfo) bool {
	if r.Method == http.MethodGet || r.Method == http.MethodHead || r.Method == http.MethodOptions {
		return true
	}
	h := r.Header.Get("X-CSRF-Token")
	return h != "" && subtle.ConstantTimeCompare([]byte(h), []byte(info.CSRF)) == 1
}

// loginAllowed implements a small per-IP lockout without external storage.
func (a *authManager) loginAllowed(ip string) (bool, time.Duration) {
	a.mu.Lock()
	defer a.mu.Unlock()
	b := a.login[ip]
	if b == nil {
		return true, 0
	}
	if time.Now().Before(b.until) {
		return false, time.Until(b.until)
	}
	return true, 0
}

func (a *authManager) loginFailed(ip string) {
	a.mu.Lock()
	defer a.mu.Unlock()
	now := time.Now()
	b := a.login[ip]
	if b == nil || now.Sub(b.last) > 10*time.Minute {
		b = &loginBucket{}
		a.login[ip] = b
	}
	b.fails++
	b.last = now
	if b.fails >= 5 {
		b.until = now.Add(60 * time.Second)
		b.fails = 0
	}
	// Bound the map.
	if len(a.login) > 5000 {
		a.login = map[string]*loginBucket{ip: b}
	}
}

func (a *authManager) loginOK(ip string) {
	a.mu.Lock()
	delete(a.login, ip)
	a.mu.Unlock()
}

func clientIP(r *http.Request) string {
	// Listeners are direct (loopback + EasyTier). Do not trust X-Forwarded-For:
	// a client could otherwise bypass the login lockout by rotating the header.
	host := r.RemoteAddr
	if i := strings.LastIndex(host, ":"); i > 0 {
		host = host[:i]
	}
	return host
}

func securityHeaders(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		h := w.Header()
		h.Set("X-Content-Type-Options", "nosniff")
		h.Set("X-Frame-Options", "DENY")
		h.Set("Referrer-Policy", "no-referrer")
		h.Set("Permissions-Policy", "camera=(), microphone=(), geolocation=()")
		h.Set("Content-Security-Policy",
			"default-src 'self'; script-src 'self'; style-src 'self' 'unsafe-inline'; "+
				"img-src 'self' data:; connect-src 'self'; font-src 'self'; "+
				"base-uri 'none'; form-action 'self'; frame-ancestors 'none'")
		if strings.HasPrefix(r.URL.Path, "/api/") || strings.HasPrefix(r.URL.Path, "/n/") {
			h.Set("Cache-Control", "no-store")
		}
		next.ServeHTTP(w, r)
	})
}

func writeJSON(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(v)
}

func writeErr(w http.ResponseWriter, code int, msg string) {
	writeJSON(w, code, map[string]any{"error": msg})
}

func constantEqual(a, b string) bool {
	return subtle.ConstantTimeCompare([]byte(a), []byte(b)) == 1
}

var _ = sha256.Size
