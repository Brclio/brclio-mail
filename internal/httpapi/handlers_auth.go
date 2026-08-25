package httpapi

import (
	"net/http"
	"strings"
	"time"

	"github.com/Brclio/brclio-mail/internal/security"
	"github.com/Brclio/brclio-mail/internal/service"
	"github.com/Brclio/brclio-mail/internal/store"
)

func (s *Server) health(w http.ResponseWriter, r *http.Request) {
	if err := s.Store.DB().PingContext(r.Context()); err != nil {
		writeError(w, http.StatusServiceUnavailable, "database_unavailable", "database unavailable")
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"status": "ok", "version": s.Version, "sqliteVersion": s.Store.SQLiteVersion()})
}

func (s *Server) status(w http.ResponseWriter, r *http.Request) {
	count, err := s.Store.CountUsers(r.Context())
	if err != nil {
		writeError(w, http.StatusServiceUnavailable, "database_unavailable", "数据库暂不可用")
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"version": s.Version, "initialized": count > 0, "tlsConfigured": s.Config.MailTLSAvailable(), "devMode": s.Config.DevMode, "hostname": s.Config.Hostname, "baseUrl": s.Config.BaseURL, "sqliteVersion": s.Store.SQLiteVersion(), "preview": true})
}

func (s *Server) setup(w http.ResponseWriter, r *http.Request) {
	ip := s.clientIP(r)
	if !s.limiter.allow("setup:"+ip, 10, time.Hour) {
		writeError(w, http.StatusTooManyRequests, "rate_limited", "尝试次数过多，请稍后再试")
		return
	}
	var request service.SetupRequest
	if !decodeJSON(w, r, &request, 64*1024) {
		return
	}
	user, domain, err := s.Service.Setup(r.Context(), request, ip)
	if err != nil {
		handleStoreError(w, err)
		return
	}
	token, err := security.RandomToken(32)
	if err != nil {
		handleStoreError(w, err)
		return
	}
	expires := time.Now().UTC().Add(s.Config.SessionTTL)
	_, err = s.Store.CreateSession(r.Context(), user.ID, user.AuthVersion, security.TokenHash(token), ip, r.UserAgent(), expires)
	if err != nil {
		handleStoreError(w, err)
		return
	}
	s.setSessionCookie(w, token, expires)
	s.limiter.clear("setup:" + ip)
	writeJSON(w, http.StatusCreated, map[string]any{"user": user, "domain": domain})
}

func (s *Server) login(w http.ResponseWriter, r *http.Request) {
	ip := s.clientIP(r)
	var request struct {
		Email    string `json:"email"`
		Password string `json:"password"`
	}
	if !decodeJSON(w, r, &request, 64*1024) {
		return
	}
	ipKey := "login-ip:" + ip
	accountKey := "login-account:" + strings.ToLower(strings.TrimSpace(request.Email))
	if !s.limiter.allow(ipKey, 100, 15*time.Minute) || !s.limiter.allow(accountKey, 10, 15*time.Minute) {
		writeError(w, http.StatusTooManyRequests, "rate_limited", "登录尝试过多，请稍后再试")
		return
	}
	// App passwords are restricted to mail clients. The web UI grants access to
	// administration and must always require the primary password.
	user, err := s.Store.Authenticate(r.Context(), request.Email, request.Password, false)
	if err != nil {
		time.Sleep(150 * time.Millisecond)
		writeError(w, http.StatusUnauthorized, "invalid_credentials", "邮箱或密码不正确")
		return
	}
	token, err := security.RandomToken(32)
	if err != nil {
		handleStoreError(w, err)
		return
	}
	expires := time.Now().UTC().Add(s.Config.SessionTTL)
	if _, err = s.Store.CreateSession(r.Context(), user.ID, user.AuthVersion, security.TokenHash(token), ip, r.UserAgent(), expires); err != nil {
		handleStoreError(w, err)
		return
	}
	_ = s.Store.Audit(r.Context(), store.AuditEvent{ActorID: user.ID, Action: "auth.login", TargetType: "session", TargetID: "web", IP: ip})
	s.setSessionCookie(w, token, expires)
	s.limiter.clear(accountKey)
	writeJSON(w, http.StatusOK, map[string]any{"user": user})
}

func (s *Server) logout(w http.ResponseWriter, r *http.Request, user store.User) {
	if cookie, err := r.Cookie(sessionCookie); err == nil {
		_ = s.Store.DeleteSession(r.Context(), security.TokenHash(cookie.Value))
	}
	s.clearSessionCookie(w)
	_ = s.Store.Audit(r.Context(), store.AuditEvent{ActorID: user.ID, Action: "auth.logout", TargetType: "session", TargetID: "web", IP: s.clientIP(r)})
	writeJSON(w, http.StatusOK, map[string]bool{"ok": true})
}

func (s *Server) me(w http.ResponseWriter, r *http.Request, user store.User) {
	fresh, err := s.Store.GetUserByID(r.Context(), user.ID)
	if err != nil {
		handleStoreError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"user": fresh, "clientConfig": map[string]any{"hostname": s.Config.Hostname, "imapPort": 993, "smtpPort": 587, "security": "TLS/STARTTLS", "username": fresh.Email}})
}
