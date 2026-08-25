package httpapi

import (
	"fmt"
	"net/http"
	"strconv"
	"strings"

	"github.com/Brclio/brclio-mail/internal/mailcore"
	"github.com/Brclio/brclio-mail/internal/security"
	"github.com/Brclio/brclio-mail/internal/store"
)

func (s *Server) mailboxes(w http.ResponseWriter, r *http.Request, user store.User) {
	items, err := s.Store.ListMailboxes(r.Context(), user.ID)
	if err != nil {
		handleStoreError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"items": items})
}

func (s *Server) messages(w http.ResponseWriter, r *http.Request, user store.User) {
	page := clampInt(parseInt(r.URL.Query().Get("page"), 1), 1, 100000)
	limit := clampInt(parseInt(r.URL.Query().Get("limit"), 50), 1, 100)
	items, err := s.Store.ListMessages(r.Context(), store.MessageQuery{UserID: user.ID, Mailbox: r.URL.Query().Get("mailbox"), Search: r.URL.Query().Get("q"), Limit: limit + 1, Offset: (page - 1) * limit})
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

func (s *Server) message(w http.ResponseWriter, r *http.Request, user store.User) {
	item, err := s.Store.GetMessage(r.Context(), user.ID, r.PathValue("id"), false)
	if err != nil {
		handleStoreError(w, err)
		return
	}
	item.UserFlags = setFlag(item.UserFlags, "\\Seen", true)
	_ = s.Store.UpdateFlags(r.Context(), user.ID, item.MailboxEntryID, item.UserFlags)
	attachments, err := s.Store.ListAttachments(r.Context(), item.ID)
	if err != nil {
		handleStoreError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"message": item, "attachments": attachments})
}

func (s *Server) attachment(w http.ResponseWriter, r *http.Request, user store.User) {
	item, err := s.Store.GetAttachment(r.Context(), user.ID, r.PathValue("id"), false)
	if err != nil {
		handleStoreError(w, err)
		return
	}
	serveAttachment(w, item)
}
func (s *Server) adminAttachment(w http.ResponseWriter, r *http.Request, user store.User) {
	var request struct {
		Reason string `json:"reason"`
	}
	if !decodeJSON(w, r, &request, 64*1024) {
		return
	}
	reason := strings.TrimSpace(request.Reason)
	if len([]rune(reason)) < 4 {
		writeError(w, http.StatusBadRequest, "reason_required", "下载归档附件前必须填写查看理由")
		return
	}
	item, err := s.Store.GetAttachment(r.Context(), "", r.PathValue("attachmentId"), true)
	if err != nil {
		handleStoreError(w, err)
		return
	}
	if item.MessageID != r.PathValue("id") {
		writeError(w, http.StatusNotFound, "not_found", "附件不属于指定邮件")
		return
	}
	if _, err = s.Store.GetMessage(r.Context(), "", item.MessageID, true); err != nil {
		handleStoreError(w, err)
		return
	}
	metadata := `{"messageId":` + strconv.Quote(item.MessageID) + `}`
	if err = s.Store.Audit(r.Context(), store.AuditEvent{ActorID: user.ID, Action: "archive.attachment.download", TargetType: "attachment", TargetID: item.ID, Reason: reason, Metadata: metadata, IP: s.clientIP(r)}); err != nil {
		handleStoreError(w, err)
		return
	}
	serveAttachment(w, item)
}
func serveAttachment(w http.ResponseWriter, item store.Attachment) {
	w.Header().Set("Content-Type", item.ContentType)
	w.Header().Set("Content-Disposition", fmt.Sprintf(`attachment; filename=%q`, strings.ReplaceAll(item.Filename, `"`, `_`)))
	w.Header().Set("X-Content-Type-Options", "nosniff")
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(item.Content)
}

func (s *Server) flags(w http.ResponseWriter, r *http.Request, user store.User) {
	item, err := s.Store.GetMessage(r.Context(), user.ID, r.PathValue("id"), false)
	if err != nil {
		handleStoreError(w, err)
		return
	}
	var request struct {
		Seen     *bool `json:"seen"`
		Starred  *bool `json:"starred"`
		Answered *bool `json:"answered"`
	}
	if !decodeJSON(w, r, &request, 64*1024) {
		return
	}
	if request.Seen != nil {
		item.UserFlags = setFlag(item.UserFlags, "\\Seen", *request.Seen)
	}
	if request.Starred != nil {
		item.UserFlags = setFlag(item.UserFlags, "\\Flagged", *request.Starred)
	}
	if request.Answered != nil {
		item.UserFlags = setFlag(item.UserFlags, "\\Answered", *request.Answered)
	}
	if err = s.Store.UpdateFlags(r.Context(), user.ID, item.MailboxEntryID, item.UserFlags); err != nil {
		handleStoreError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"flags": item.UserFlags})
}

func (s *Server) move(w http.ResponseWriter, r *http.Request, user store.User) {
	item, err := s.Store.GetMessage(r.Context(), user.ID, r.PathValue("id"), false)
	if err != nil {
		handleStoreError(w, err)
		return
	}
	var request struct {
		Mailbox string `json:"mailbox"`
	}
	if !decodeJSON(w, r, &request, 64*1024) {
		return
	}
	if err := s.Store.MoveMessage(r.Context(), user.ID, item.MailboxEntryID, request.Mailbox); err != nil {
		handleStoreError(w, err)
		return
	}
	_ = s.Store.Audit(r.Context(), store.AuditEvent{ActorID: user.ID, Action: "message.move", TargetType: "message", TargetID: item.ID, Metadata: `{"mailbox":` + strconv.Quote(request.Mailbox) + `}`, IP: s.clientIP(r)})
	writeJSON(w, http.StatusOK, map[string]bool{"ok": true})
}
func (s *Server) expunge(w http.ResponseWriter, r *http.Request, user store.User) {
	item, err := s.Store.GetMessage(r.Context(), user.ID, r.PathValue("id"), false)
	if err != nil {
		handleStoreError(w, err)
		return
	}
	if err := s.Store.ExpungeMessage(r.Context(), user.ID, item.MailboxEntryID); err != nil {
		handleStoreError(w, err)
		return
	}
	_ = s.Store.Audit(r.Context(), store.AuditEvent{ActorID: user.ID, Action: "message.expunge", TargetType: "message", TargetID: item.ID, IP: s.clientIP(r)})
	writeJSON(w, http.StatusOK, map[string]bool{"ok": true})
}

func (s *Server) compose(w http.ResponseWriter, r *http.Request, user store.User) {
	var request mailcore.ComposeRequest
	if !decodeJSON(w, r, &request, s.Config.MaxMessageBytes*2) {
		return
	}
	message, err := s.Service.Send(r.Context(), user, request, s.clientIP(r))
	if err != nil {
		handleStoreError(w, err)
		return
	}
	writeJSON(w, http.StatusAccepted, map[string]any{"message": message})
}
func (s *Server) draft(w http.ResponseWriter, r *http.Request, user store.User) {
	var request struct {
		ID          string                       `json:"id"`
		To          []string                     `json:"to"`
		CC          []string                     `json:"cc"`
		BCC         []string                     `json:"bcc"`
		Subject     string                       `json:"subject"`
		Body        string                       `json:"body"`
		HTMLBody    string                       `json:"htmlBody"`
		Attachments []mailcore.ComposeAttachment `json:"attachments"`
	}
	if !decodeJSON(w, r, &request, s.Config.MaxMessageBytes*2) {
		return
	}
	message, err := s.Service.SaveDraft(r.Context(), user, mailcore.ComposeRequest{To: request.To, CC: request.CC, BCC: request.BCC, Subject: request.Subject, TextBody: request.Body, HTMLBody: request.HTMLBody, Attachments: request.Attachments}, request.ID, s.clientIP(r))
	if err != nil {
		handleStoreError(w, err)
		return
	}
	writeJSON(w, http.StatusCreated, map[string]any{"message": message})
}

func (s *Server) appPasswords(w http.ResponseWriter, r *http.Request, user store.User) {
	items, err := s.Store.ListAppPasswords(r.Context(), user.ID)
	if err != nil {
		handleStoreError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"items": items})
}
func (s *Server) createAppPassword(w http.ResponseWriter, r *http.Request, user store.User) {
	var request struct {
		Name string `json:"name"`
	}
	if !decodeJSON(w, r, &request, 64*1024) {
		return
	}
	token, err := security.RandomToken(18)
	if err != nil {
		handleStoreError(w, err)
		return
	}
	secret := "brclio-" + token
	item, err := s.Store.CreateAppPassword(r.Context(), user.ID, request.Name, security.TokenHash(secret))
	if err != nil {
		handleStoreError(w, err)
		return
	}
	_ = s.Store.Audit(r.Context(), store.AuditEvent{ActorID: user.ID, Action: "app_password.create", TargetType: "app_password", TargetID: item.ID, IP: s.clientIP(r)})
	writeJSON(w, http.StatusCreated, map[string]any{"appPassword": item, "secret": secret, "warning": "该密码只显示一次，请立即保存"})
}
func (s *Server) revokeAppPassword(w http.ResponseWriter, r *http.Request, user store.User) {
	if err := s.Store.RevokeAppPassword(r.Context(), user.ID, r.PathValue("id")); err != nil {
		handleStoreError(w, err)
		return
	}
	_ = s.Store.Audit(r.Context(), store.AuditEvent{ActorID: user.ID, Action: "app_password.revoke", TargetType: "app_password", TargetID: r.PathValue("id"), IP: s.clientIP(r)})
	writeJSON(w, http.StatusOK, map[string]bool{"ok": true})
}

func setFlag(flags []string, flag string, enabled bool) []string {
	result := make([]string, 0, len(flags)+1)
	found := false
	for _, item := range flags {
		if strings.EqualFold(item, flag) {
			found = true
			if enabled {
				result = append(result, flag)
			}
		} else {
			result = append(result, item)
		}
	}
	if enabled && !found {
		result = append(result, flag)
	}
	return result
}
