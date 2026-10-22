package console

import (
	"crypto/hmac"
	"crypto/subtle"
	"encoding/base64"
	"encoding/json"
	"net/http"
	"strings"
	"sync"
	"time"
)

const sessionCookie = "rymesh_session"

type authPayload struct {
	Exp  int64  `json:"exp"`
	CSRF string `json:"csrf"`
}

type authInfo struct {
	CSRF string
	Exp  int64
}

type authManager struct {
	cfg     *Config
	stateMu sync.RWMutex
}

func newAuthManager(cfg *Config) *authManager {
	return &authManager{cfg: cfg}
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

// setConfig points the manager at the config currently published by Server.
// Settings changes use copy-on-write, so without this the manager would keep
// validating against a stale snapshot.
func (a *authManager) setConfig(c *Config) {
	a.stateMu.Lock()
	a.cfg = c
	a.stateMu.Unlock()
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

func (a *authManager) checkCSRF(r *http.Request, info *authInfo) bool {
	if r.Method == http.MethodGet || r.Method == http.MethodHead || r.Method == http.MethodOptions {
		return true
	}
	h := r.Header.Get("X-CSRF-Token")
	return h != "" && subtle.ConstantTimeCompare([]byte(h), []byte(info.CSRF)) == 1
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
