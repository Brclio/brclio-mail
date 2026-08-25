package httpapi

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/url"
	"runtime/debug"
	"strings"
	"sync"
	"time"

	"github.com/Brclio/brclio-mail/internal/config"
	"github.com/Brclio/brclio-mail/internal/security"
	"github.com/Brclio/brclio-mail/internal/service"
	"github.com/Brclio/brclio-mail/internal/store"
)

const sessionCookie = "brclio_session"

type Server struct {
	Config  config.Config
	Store   *store.Store
	Service *service.Service
	Version string
	Logger  *slog.Logger
	mux     *http.ServeMux
	limiter *loginLimiter
	static  http.Handler
}

type contextKey string

const userKey contextKey = "user"

func New(cfg config.Config, db *store.Store, svc *service.Service, logger *slog.Logger, static http.Handler) *Server {
	if logger == nil {
		logger = slog.Default()
	}
	s := &Server{Config: cfg, Store: db, Service: svc, Version: buildVersion(), Logger: logger, mux: http.NewServeMux(), limiter: newLoginLimiter(), static: static}
	s.routes()
	return s
}

func (s *Server) Handler() http.Handler {
	return s.securityHeaders(s.recoverer(s.csrf(s.mux)))
}

func (s *Server) routes() {
	s.mux.HandleFunc("GET /healthz", s.health)
	s.mux.HandleFunc("GET /api/status", s.status)
	s.mux.HandleFunc("POST /api/setup", s.setup)
	s.mux.HandleFunc("POST /api/auth/login", s.login)
	s.mux.HandleFunc("POST /api/auth/logout", s.withUser(s.logout))
	s.mux.HandleFunc("GET /api/me", s.withUser(s.me))
	s.mux.HandleFunc("GET /api/mailboxes", s.withUser(s.mailboxes))
	s.mux.HandleFunc("GET /api/messages", s.withUser(s.messages))
	s.mux.HandleFunc("GET /api/messages/{id}", s.withUser(s.message))
	s.mux.HandleFunc("GET /api/attachments/{id}", s.withUser(s.attachment))
	s.mux.HandleFunc("POST /api/messages/{id}/flags", s.withUser(s.flags))
	s.mux.HandleFunc("POST /api/messages/{id}/move", s.withUser(s.move))
	s.mux.HandleFunc("POST /api/messages/{id}/expunge", s.withUser(s.expunge))
	s.mux.HandleFunc("POST /api/compose", s.withUser(s.compose))
	s.mux.HandleFunc("POST /api/drafts", s.withUser(s.draft))
	s.mux.HandleFunc("GET /api/app-passwords", s.withUser(s.appPasswords))
	s.mux.HandleFunc("POST /api/app-passwords", s.withUser(s.createAppPassword))
	s.mux.HandleFunc("DELETE /api/app-passwords/{id}", s.withUser(s.revokeAppPassword))

	s.mux.HandleFunc("GET /api/admin/stats", s.withAdmin(s.adminStats))
	s.mux.HandleFunc("GET /api/admin/domains", s.withAdmin(s.adminDomains))
	s.mux.HandleFunc("POST /api/admin/domains", s.withAdmin(s.adminCreateDomain))
	s.mux.HandleFunc("GET /api/admin/domains/{id}/dns", s.withAdmin(s.adminDomainDNS))
	s.mux.HandleFunc("POST /api/admin/domains/{id}/verify", s.withAdmin(s.adminDomainVerify))
	s.mux.HandleFunc("GET /api/admin/users", s.withAdmin(s.adminUsers))
	s.mux.HandleFunc("POST /api/admin/users", s.withAdmin(s.adminCreateUser))
	s.mux.HandleFunc("PATCH /api/admin/users/{id}", s.withAdmin(s.adminUpdateUser))
	s.mux.HandleFunc("GET /api/admin/aliases", s.withAdmin(s.adminAliases))
	s.mux.HandleFunc("POST /api/admin/aliases", s.withAdmin(s.adminCreateAlias))
	s.mux.HandleFunc("GET /api/admin/archive", s.withAdmin(s.adminArchive))
	s.mux.HandleFunc("POST /api/admin/archive/{id}/view", s.withAdmin(s.adminArchiveView))
	s.mux.HandleFunc("POST /api/admin/archive/{id}/attachments/{attachmentId}", s.withAdmin(s.adminAttachment))
	s.mux.HandleFunc("GET /api/admin/queue", s.withAdmin(s.adminQueue))
	s.mux.HandleFunc("GET /api/admin/audit", s.withAdmin(s.adminAudit))

	if s.static != nil {
		s.mux.Handle("/", s.static)
	}
}

func (s *Server) withUser(next func(http.ResponseWriter, *http.Request, store.User)) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		cookie, err := r.Cookie(sessionCookie)
		if err != nil || cookie.Value == "" {
			writeError(w, http.StatusUnauthorized, "authentication_required", "请先登录")
			return
		}
		user, err := s.Store.UserForSession(r.Context(), security.TokenHash(cookie.Value))
		if err != nil {
			s.clearSessionCookie(w)
			writeError(w, http.StatusUnauthorized, "authentication_required", "登录已失效，请重新登录")
			return
		}
		r = r.WithContext(context.WithValue(r.Context(), userKey, user))
		next(w, r, user)
	}
}

func (s *Server) withAdmin(next func(http.ResponseWriter, *http.Request, store.User)) http.HandlerFunc {
	return s.withUser(func(w http.ResponseWriter, r *http.Request, user store.User) {
		if user.Role != store.RoleAdmin {
			writeError(w, http.StatusForbidden, "admin_required", "需要管理员权限")
			return
		}
		next(w, r, user)
	})
}

func (s *Server) securityHeaders(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("X-Content-Type-Options", "nosniff")
		w.Header().Set("X-Frame-Options", "DENY")
		w.Header().Set("Referrer-Policy", "same-origin")
		w.Header().Set("Permissions-Policy", "camera=(), microphone=(), geolocation=()")
		w.Header().Set("Cross-Origin-Opener-Policy", "same-origin")
		w.Header().Set("Cross-Origin-Resource-Policy", "same-origin")
		if strings.HasPrefix(strings.ToLower(s.Config.BaseURL), "https://") && !s.Config.DevMode {
			w.Header().Set("Strict-Transport-Security", "max-age=31536000")
		}
		if strings.HasPrefix(r.URL.Path, "/api/") {
			w.Header().Set("Cache-Control", "no-store")
		}
		w.Header().Set("Content-Security-Policy", "default-src 'self'; img-src 'self' data: blob:; style-src 'self'; script-src 'self'; connect-src 'self'; frame-src 'none'; object-src 'none'; base-uri 'self'; form-action 'self'; frame-ancestors 'none'")
		next.ServeHTTP(w, r)
	})
}

func (s *Server) csrf(next http.Handler) http.Handler {
	base, _ := url.Parse(s.Config.BaseURL)
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet && r.Method != http.MethodHead && r.Method != http.MethodOptions {
			if contentType := r.Header.Get("Content-Type"); !strings.HasPrefix(contentType, "application/json") {
				writeError(w, http.StatusUnsupportedMediaType, "json_required", "请求必须使用 application/json")
				return
			}
			if origin := r.Header.Get("Origin"); origin != "" {
				parsed, err := url.Parse(origin)
				allowed := err == nil && base != nil && strings.EqualFold(parsed.Scheme, base.Scheme) && strings.EqualFold(parsed.Host, base.Host)
				if s.Config.DevMode && parsed != nil && (parsed.Hostname() == "localhost" || parsed.Hostname() == "127.0.0.1") {
					allowed = true
				}
				if !allowed {
					writeError(w, http.StatusForbidden, "origin_rejected", "请求来源不受信任")
					return
				}
			}
		}
		next.ServeHTTP(w, r)
	})
}

func (s *Server) recoverer(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		defer func() {
			if recovered := recover(); recovered != nil {
				s.Logger.Error("http panic", "error", recovered, "stack", string(debug.Stack()))
				writeError(w, http.StatusInternalServerError, "internal_error", "服务器内部错误")
			}
		}()
		next.ServeHTTP(w, r)
	})
}

func decodeJSON(w http.ResponseWriter, r *http.Request, value any, maxBytes int64) bool {
	if maxBytes <= 0 {
		maxBytes = 1024 * 1024
	}
	r.Body = http.MaxBytesReader(w, r.Body, maxBytes)
	decoder := json.NewDecoder(r.Body)
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(value); err != nil {
		writeError(w, http.StatusBadRequest, "invalid_json", "请求内容无法解析")
		return false
	}
	if err := decoder.Decode(&struct{}{}); !errors.Is(err, io.EOF) {
		writeError(w, http.StatusBadRequest, "invalid_json", "请求只能包含一个 JSON 对象")
		return false
	}
	return true
}

func writeJSON(w http.ResponseWriter, status int, value any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(value)
}

func writeError(w http.ResponseWriter, status int, code, message string) {
	writeJSON(w, status, map[string]any{"error": map[string]string{"code": code, "message": message}})
}

func handleStoreError(w http.ResponseWriter, err error) {
	switch {
	case errors.Is(err, store.ErrNotFound):
		writeError(w, http.StatusNotFound, "not_found", "没有找到对应内容")
	case errors.Is(err, store.ErrConflict):
		writeError(w, http.StatusConflict, "conflict", "该内容已存在")
	case errors.Is(err, store.ErrForbidden):
		writeError(w, http.StatusForbidden, "forbidden", "没有权限执行此操作")
	case errors.Is(err, store.ErrQuotaExceeded):
		writeError(w, http.StatusInsufficientStorage, "quota_exceeded", "邮箱空间不足")
	case errors.Is(err, store.ErrArchiveFull):
		writeError(w, http.StatusInsufficientStorage, "archive_full", "邮件归档空间已达上限，请联系管理员")
	case errors.Is(err, store.ErrAuditFailed):
		writeError(w, http.StatusInternalServerError, "audit_unavailable", "必要的审计记录写入失败，本次操作未完成")
	case errors.Is(err, store.ErrResourceLimit):
		writeError(w, http.StatusInsufficientStorage, "resource_limit", "已达到此类资源的数量上限")
	case errors.Is(err, store.ErrDomainUnverified):
		writeError(w, http.StatusConflict, "domain_unverified", "域名所有权尚未通过 DNS 验证")
	default:
		// Never return SQLite/schema/path details to an unauthenticated client.
		// Public validation errors deliberately use one stable message.
		writeError(w, http.StatusBadRequest, "request_failed", "请求参数无效或操作无法完成")
	}
}

func (s *Server) clientIP(r *http.Request) string {
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err == nil {
		return host
	}
	return r.RemoteAddr
}

func (s *Server) setSessionCookie(w http.ResponseWriter, token string, expires time.Time) {
	secure := strings.HasPrefix(strings.ToLower(s.Config.BaseURL), "https://") && !s.Config.DevMode
	http.SetCookie(w, &http.Cookie{Name: sessionCookie, Value: token, Path: "/", Expires: expires, MaxAge: int(time.Until(expires).Seconds()), HttpOnly: true, Secure: secure, SameSite: http.SameSiteStrictMode})
}
func (s *Server) clearSessionCookie(w http.ResponseWriter) {
	secure := strings.HasPrefix(strings.ToLower(s.Config.BaseURL), "https://") && !s.Config.DevMode
	http.SetCookie(w, &http.Cookie{Name: sessionCookie, Value: "", Path: "/", MaxAge: -1, HttpOnly: true, Secure: secure, SameSite: http.SameSiteStrictMode})
}

func buildVersion() string {
	if info, ok := debug.ReadBuildInfo(); ok && info.Main.Version != "" && info.Main.Version != "(devel)" {
		return info.Main.Version
	}
	return "0.2.1-dev"
}

type loginLimiter struct {
	mu       sync.Mutex
	attempts map[string][]time.Time
}

func newLoginLimiter() *loginLimiter { return &loginLimiter{attempts: map[string][]time.Time{}} }
func (l *loginLimiter) allow(key string, limit int, window time.Duration) bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	now := time.Now()
	cutoff := now.Add(-window)
	if _, exists := l.attempts[key]; !exists && len(l.attempts) >= 10000 {
		for candidate, times := range l.attempts {
			if len(times) == 0 || times[len(times)-1].Before(cutoff) {
				delete(l.attempts, candidate)
			}
		}
		if len(l.attempts) >= 10000 {
			return false
		}
	}
	previous := l.attempts[key]
	recent := previous[:0]
	for _, item := range previous {
		if item.After(cutoff) {
			recent = append(recent, item)
		}
	}
	if len(recent) >= limit {
		l.attempts[key] = recent
		return false
	}
	l.attempts[key] = append(recent, now)
	return true
}
func (l *loginLimiter) clear(key string) { l.mu.Lock(); delete(l.attempts, key); l.mu.Unlock() }

func clampInt(value, min, max int) int {
	if value < min {
		return min
	}
	if value > max {
		return max
	}
	return value
}
func parseInt(value string, fallback int) int {
	var result int
	if _, err := fmt.Sscanf(value, "%d", &result); err != nil {
		return fallback
	}
	return result
}
