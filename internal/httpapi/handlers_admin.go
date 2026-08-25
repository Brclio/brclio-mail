package httpapi

import (
	"encoding/json"
	"net/http"
	"os"

	"github.com/Brclio/brclio-mail/internal/security"
	"github.com/Brclio/brclio-mail/internal/store"
)

func (s *Server) adminStats(w http.ResponseWriter, r *http.Request, user store.User) {
	stats, err := s.Store.Stats(r.Context())
	if err != nil {
		handleStoreError(w, err)
		return
	}
	var dbBytes, walBytes int64
	if info, e := os.Stat(s.Config.DatabasePath); e == nil {
		dbBytes = info.Size()
	}
	if info, e := os.Stat(s.Config.DatabasePath + "-wal"); e == nil {
		walBytes = info.Size()
	}
	writeJSON(w, http.StatusOK, map[string]any{"stats": stats,
		"physicalStorage": map[string]int64{"databaseBytes": dbBytes, "walBytes": walBytes, "totalBytes": dbBytes + walBytes},
		"limits":          map[string]int64{"archiveBytes": s.Config.MaxArchiveBytes, "messageBytes": s.Config.MaxMessageBytes, "minFreeDiskBytes": s.Config.MinFreeDiskBytes}})
}
func (s *Server) adminDomains(w http.ResponseWriter, r *http.Request, user store.User) {
	items, err := s.Store.ListDomains(r.Context())
	if err != nil {
		handleStoreError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"items": items})
}
func (s *Server) adminCreateDomain(w http.ResponseWriter, r *http.Request, user store.User) {
	var request struct {
		Name string `json:"name"`
	}
	if !decodeJSON(w, r, &request, 64*1024) {
		return
	}
	item, err := s.Service.CreateDomainAudited(r.Context(), request.Name,
		store.AuditEvent{ActorID: user.ID, Action: "domain.create", TargetType: "domain", IP: s.clientIP(r)})
	if err != nil {
		handleStoreError(w, err)
		return
	}
	writeJSON(w, http.StatusCreated, map[string]any{"domain": item, "records": s.Store.DNSExpectedRecords(item, s.Config.Hostname, s.Config.BaseURL)})
}
func (s *Server) adminDomainDNS(w http.ResponseWriter, r *http.Request, user store.User) {
	items, err := s.Store.ListDomains(r.Context())
	if err != nil {
		handleStoreError(w, err)
		return
	}
	for _, item := range items {
		if item.ID == r.PathValue("id") {
			writeJSON(w, http.StatusOK, map[string]any{"domain": item, "records": s.Store.DNSExpectedRecords(item, s.Config.Hostname, s.Config.BaseURL), "note": "PTR 与 25 端口需在云服务商侧配置；本页只给出期望值"})
			return
		}
	}
	writeError(w, http.StatusNotFound, "not_found", "域名不存在")
}

func (s *Server) adminDomainVerify(w http.ResponseWriter, r *http.Request, user store.User) {
	var request struct{}
	if !decodeJSON(w, r, &request, 1024) {
		return
	}
	domain, err := s.Service.VerifyDomainAudited(r.Context(), r.PathValue("id"),
		store.AuditEvent{ActorID: user.ID, Action: "domain.verify", TargetType: "domain", TargetID: r.PathValue("id"), IP: s.clientIP(r)})
	if err != nil {
		handleStoreError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"domain": domain, "verified": true})
}
func (s *Server) adminUsers(w http.ResponseWriter, r *http.Request, user store.User) {
	items, err := s.Store.ListUsers(r.Context())
	if err != nil {
		handleStoreError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"items": items})
}
func (s *Server) adminCreateUser(w http.ResponseWriter, r *http.Request, user store.User) {
	var request struct {
		Email       string `json:"email"`
		DisplayName string `json:"displayName"`
		Password    string `json:"password"`
		Role        string `json:"role"`
		QuotaBytes  int64  `json:"quotaBytes"`
	}
	if !decodeJSON(w, r, &request, 128*1024) {
		return
	}
	if request.Role == "" {
		request.Role = store.RoleUser
	}
	if request.QuotaBytes == 0 {
		request.QuotaBytes = 5 * 1024 * 1024 * 1024
	}
	hash, err := security.HashPassword(request.Password)
	if err != nil {
		handleStoreError(w, err)
		return
	}
	item, err := s.Store.CreateUserAudited(r.Context(), request.Email, request.DisplayName, hash, request.Role, request.QuotaBytes,
		store.AuditEvent{ActorID: user.ID, Action: "user.create", TargetType: "user", IP: s.clientIP(r)})
	if err != nil {
		handleStoreError(w, err)
		return
	}
	writeJSON(w, http.StatusCreated, map[string]any{"user": item})
}
func (s *Server) adminUpdateUser(w http.ResponseWriter, r *http.Request, user store.User) {
	var request struct {
		DisplayName *string `json:"displayName"`
		Status      *string `json:"status"`
		Role        *string `json:"role"`
		QuotaBytes  *int64  `json:"quotaBytes"`
		Password    *string `json:"password"`
	}
	if !decodeJSON(w, r, &request, 128*1024) {
		return
	}
	update := store.UserUpdate{DisplayName: request.DisplayName, Status: request.Status, Role: request.Role, QuotaBytes: request.QuotaBytes}
	if request.Password != nil {
		hash, err := security.HashPassword(*request.Password)
		if err != nil {
			handleStoreError(w, err)
			return
		}
		update.PasswordHash = &hash
	}
	if user.ID == r.PathValue("id") && request.Status != nil && *request.Status == store.StatusSuspended {
		writeError(w, http.StatusBadRequest, "self_suspend", "不能停用当前管理员账户")
		return
	}
	if err := s.Store.UpdateUserAudited(r.Context(), r.PathValue("id"), update,
		store.AuditEvent{ActorID: user.ID, Action: "user.update", TargetType: "user", TargetID: r.PathValue("id"), IP: s.clientIP(r)}); err != nil {
		handleStoreError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]bool{"ok": true})
}
func (s *Server) adminAliases(w http.ResponseWriter, r *http.Request, user store.User) {
	items, err := s.Store.ListAliases(r.Context())
	if err != nil {
		handleStoreError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"items": items})
}
func (s *Server) adminCreateAlias(w http.ResponseWriter, r *http.Request, user store.User) {
	var request struct {
		Address string `json:"address"`
		Target  string `json:"target"`
	}
	if !decodeJSON(w, r, &request, 64*1024) {
		return
	}
	item, err := s.Store.CreateAliasAudited(r.Context(), request.Address, request.Target,
		store.AuditEvent{ActorID: user.ID, Action: "alias.create", TargetType: "alias", IP: s.clientIP(r)})
	if err != nil {
		handleStoreError(w, err)
		return
	}
	writeJSON(w, http.StatusCreated, map[string]any{"alias": item})
}
func (s *Server) adminArchive(w http.ResponseWriter, r *http.Request, user store.User) {
	page := clampInt(parseInt(r.URL.Query().Get("page"), 1), 1, 100000)
	limit := clampInt(parseInt(r.URL.Query().Get("limit"), 50), 1, 100)
	items, err := s.Store.ListMessages(r.Context(), store.MessageQuery{Admin: true, Search: r.URL.Query().Get("q"), Limit: limit + 1, Offset: (page - 1) * limit})
	if err != nil {
		handleStoreError(w, err)
		return
	}
	hasMore := len(items) > limit
	if hasMore {
		items = items[:limit]
	}
	metadata, _ := json.Marshal(map[string]any{"query": r.URL.Query().Get("q"), "page": page, "resultCount": len(items)})
	if err := s.Store.Audit(r.Context(), store.AuditEvent{ActorID: user.ID, Action: "archive.list", TargetType: "archive", TargetID: "messages", Metadata: string(metadata), IP: s.clientIP(r)}); err != nil {
		handleStoreError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"items": items, "page": page, "hasMore": hasMore, "retention": "用户删除仅移除个人视图，原始邮件继续保留在管理员归档"})
}
func (s *Server) adminArchiveView(w http.ResponseWriter, r *http.Request, user store.User) {
	var request struct {
		Reason string `json:"reason"`
	}
	if !decodeJSON(w, r, &request, 64*1024) {
		return
	}
	item, err := s.Service.AdminView(r.Context(), user, r.PathValue("id"), request.Reason, s.clientIP(r))
	if err != nil {
		handleStoreError(w, err)
		return
	}
	attachments, err := s.Store.ListAttachments(r.Context(), item.ID)
	if err != nil {
		handleStoreError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"message": item, "attachments": attachments})
}
func (s *Server) adminQueue(w http.ResponseWriter, r *http.Request, user store.User) {
	items, err := s.Store.ListQueue(r.Context(), 100)
	if err != nil {
		handleStoreError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"items": items})
}
func (s *Server) adminAudit(w http.ResponseWriter, r *http.Request, user store.User) {
	page := clampInt(parseInt(r.URL.Query().Get("page"), 1), 1, 100000)
	limit := clampInt(parseInt(r.URL.Query().Get("limit"), 50), 1, 100)
	items, err := s.Store.ListAuditPage(r.Context(), limit+1, (page-1)*limit, r.URL.Query().Get("action"))
	if err != nil {
		handleStoreError(w, err)
		return
	}
	hasMore := len(items) > limit
	if hasMore {
		items = items[:limit]
	}
	writeJSON(w, http.StatusOK, map[string]any{"items": items, "page": page, "hasMore": hasMore})
}
