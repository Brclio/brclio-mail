package httpapi

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"testing"

	"github.com/Brclio/brclio-mail/internal/config"
	"github.com/Brclio/brclio-mail/internal/mailcore"
	"github.com/Brclio/brclio-mail/internal/security"
	"github.com/Brclio/brclio-mail/internal/service"
	"github.com/Brclio/brclio-mail/internal/store"
)

type apiFixture struct {
	handler http.Handler
	server  *Server
	db      *store.Store
	svc     *service.Service
	cfg     config.Config
}

func newAPIFixture(t *testing.T) apiFixture {
	t.Helper()
	db, err := store.Open(filepath.Join(t.TempDir(), "mail.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	cfg := config.Config{BaseURL: "http://mail.test", Hostname: "mail.test", SetupToken: "claim-token", DevMode: true,
		SessionTTL: 24 * 60 * 60 * 1e9, MaxMessageBytes: 25 * 1024 * 1024}
	svc := service.New(db, cfg)
	server := New(cfg, db, svc, nil, nil)
	return apiFixture{handler: server.Handler(), server: server, db: db, svc: svc, cfg: cfg}
}

func TestHTTPStatusUsesInjectedReleaseVersion(t *testing.T) {
	fixture := newAPIFixture(t)
	fixture.server.Version = "0.1.0-preview+test"
	for _, path := range []string{"/healthz", "/api/status"} {
		response := performJSON(t, fixture.handler, http.MethodGet, path, nil, nil, "")
		if response.Code != http.StatusOK {
			t.Fatalf("%s status=%d body=%s", path, response.Code, response.Body.String())
		}
		var payload map[string]any
		if err := json.Unmarshal(response.Body.Bytes(), &payload); err != nil {
			t.Fatal(err)
		}
		if payload["version"] != fixture.server.Version {
			t.Fatalf("%s version=%v want=%s", path, payload["version"], fixture.server.Version)
		}
	}
}

func performJSON(t *testing.T, handler http.Handler, method, path string, body any, cookie *http.Cookie, origin string) *httptest.ResponseRecorder {
	t.Helper()
	var payload []byte
	if body != nil {
		var err error
		payload, err = json.Marshal(body)
		if err != nil {
			t.Fatal(err)
		}
	}
	request := httptest.NewRequest(method, path, bytes.NewReader(payload))
	if body != nil {
		request.Header.Set("Content-Type", "application/json")
	}
	if cookie != nil {
		request.AddCookie(cookie)
	}
	if origin != "" {
		request.Header.Set("Origin", origin)
	}
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	return response
}

func setupAdministrator(t *testing.T, fixture apiFixture) (store.User, *http.Cookie) {
	t.Helper()
	response := performJSON(t, fixture.handler, http.MethodPost, "/api/setup", map[string]any{
		"domain": "example.com", "email": "admin@example.com", "displayName": "Admin",
		"password": "correct horse battery staple", "setupToken": "claim-token",
	}, nil, "http://mail.test")
	if response.Code != http.StatusCreated {
		t.Fatalf("setup status=%d body=%s", response.Code, response.Body.String())
	}
	cookies := response.Result().Cookies()
	if len(cookies) != 1 || cookies[0].Name != sessionCookie {
		t.Fatalf("setup did not create session cookie: %#v", cookies)
	}
	user, err := fixture.db.GetUserByEmail(context.Background(), "admin@example.com")
	if err != nil {
		t.Fatal(err)
	}
	return user, cookies[0]
}

func TestSetupCSRFAndSingleJSONObject(t *testing.T) {
	fixture := newAPIFixture(t)
	rejected := performJSON(t, fixture.handler, http.MethodPost, "/api/setup", map[string]any{
		"domain": "example.com", "email": "admin@example.com", "password": "correct horse battery staple", "setupToken": "claim-token",
	}, nil, "https://evil.example")
	if rejected.Code != http.StatusForbidden {
		t.Fatalf("cross-origin setup status=%d", rejected.Code)
	}

	request := httptest.NewRequest(http.MethodPost, "/api/setup", bytes.NewBufferString(`{"domain":"example.com"} {"extra":true}`))
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("Origin", "http://mail.test")
	response := httptest.NewRecorder()
	fixture.handler.ServeHTTP(response, request)
	if response.Code != http.StatusBadRequest {
		t.Fatalf("multiple JSON objects status=%d", response.Code)
	}
	if response.Header().Get("Content-Security-Policy") == "" || response.Header().Get("Cache-Control") != "no-store" {
		t.Fatal("API security headers are missing")
	}
}

func TestArchiveBodyRequiresReasonAndIsAudited(t *testing.T) {
	fixture := newAPIFixture(t)
	admin, cookie := setupAdministrator(t, fixture)
	message, err := fixture.svc.Send(context.Background(), admin, mailcore.ComposeRequest{
		To: []string{"outside@example.net"}, Subject: "Private review", TextBody: "body must not leak in archive listing",
	}, "test")
	if err != nil {
		t.Fatal(err)
	}
	list := performJSON(t, fixture.handler, http.MethodGet, "/api/admin/archive?q=Private", nil, cookie, "")
	if list.Code != http.StatusOK {
		t.Fatalf("archive list status=%d body=%s", list.Code, list.Body.String())
	}
	if bytes.Contains(list.Body.Bytes(), []byte("body must not leak")) {
		t.Fatal("archive list leaked a message body before a reason-gated view")
	}
	bodySearch := performJSON(t, fixture.handler, http.MethodGet, "/api/admin/archive?q=must%20not%20leak", nil, cookie, "")
	if bodySearch.Code != http.StatusOK || bytes.Contains(bodySearch.Body.Bytes(), []byte(message.ID)) {
		t.Fatalf("archive search exposed a body-only match: status=%d body=%s", bodySearch.Code, bodySearch.Body.String())
	}
	withoutReason := performJSON(t, fixture.handler, http.MethodPost, "/api/admin/archive/"+message.ID+"/view", map[string]string{"reason": "x"}, cookie, "http://mail.test")
	if withoutReason.Code != http.StatusBadRequest {
		t.Fatalf("short reason status=%d body=%s", withoutReason.Code, withoutReason.Body.String())
	}
	view := performJSON(t, fixture.handler, http.MethodPost, "/api/admin/archive/"+message.ID+"/view", map[string]string{"reason": "incident review"}, cookie, "http://mail.test")
	if view.Code != http.StatusOK || !bytes.Contains(view.Body.Bytes(), []byte("body must not leak")) {
		t.Fatalf("archive view status=%d body=%s", view.Code, view.Body.String())
	}
	audit, err := fixture.db.ListAudit(context.Background(), 50)
	if err != nil {
		t.Fatal(err)
	}
	var sawList, sawView bool
	for _, event := range audit {
		sawList = sawList || event.Action == "archive.list"
		sawView = sawView || event.Action == "archive.message.view" && event.Reason == "incident review"
	}
	if !sawList || !sawView {
		t.Fatalf("archive audit incomplete: list=%t view=%t", sawList, sawView)
	}
}

func TestDomainMutationRollsBackWhenMandatoryAuditFails(t *testing.T) {
	fixture := newAPIFixture(t)
	_, cookie := setupAdministrator(t, fixture)
	if _, err := fixture.db.DB().Exec(`CREATE TRIGGER reject_domain_audit BEFORE INSERT ON audit_log
		WHEN NEW.action='domain.create' BEGIN SELECT RAISE(FAIL, 'forced audit failure'); END`); err != nil {
		t.Fatal(err)
	}
	response := performJSON(t, fixture.handler, http.MethodPost, "/api/admin/domains", map[string]string{"name": "second.example"}, cookie, "http://mail.test")
	if response.Code < 500 {
		t.Fatalf("domain mutation reported success without audit: status=%d body=%s", response.Code, response.Body.String())
	}
	domains, err := fixture.db.ListDomains(context.Background())
	if err != nil || len(domains) != 1 {
		t.Fatalf("unaudited domain mutation was committed: count=%d err=%v", len(domains), err)
	}
}

func TestUserAndAliasMutationsRollBackWhenMandatoryAuditFails(t *testing.T) {
	fixture := newAPIFixture(t)
	admin, cookie := setupAdministrator(t, fixture)
	if _, err := fixture.db.DB().Exec(`CREATE TRIGGER reject_admin_mutation_audit BEFORE INSERT ON audit_log
		WHEN NEW.action IN ('user.update','alias.create') BEGIN SELECT RAISE(FAIL, 'forced audit failure'); END`); err != nil {
		t.Fatal(err)
	}
	updatedName := "Unaudited Name"
	response := performJSON(t, fixture.handler, http.MethodPatch, "/api/admin/users/"+admin.ID,
		map[string]any{"displayName": updatedName}, cookie, "http://mail.test")
	if response.Code != http.StatusInternalServerError {
		t.Fatalf("user mutation audit failure status=%d body=%s", response.Code, response.Body.String())
	}
	fresh, err := fixture.db.GetUserByID(context.Background(), admin.ID)
	if err != nil || fresh.DisplayName == updatedName {
		t.Fatalf("unaudited user mutation was committed: %#v err=%v", fresh, err)
	}
	response = performJSON(t, fixture.handler, http.MethodPost, "/api/admin/aliases",
		map[string]string{"address": "no-audit@example.com", "target": admin.Email}, cookie, "http://mail.test")
	if response.Code != http.StatusInternalServerError {
		t.Fatalf("alias mutation audit failure status=%d body=%s", response.Code, response.Body.String())
	}
	if _, err = fixture.db.ResolveRecipient(context.Background(), "no-audit@example.com"); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("unaudited alias mutation was committed: %v", err)
	}
}

func TestAppPasswordIsRejectedByWebLogin(t *testing.T) {
	fixture := newAPIFixture(t)
	admin, _ := setupAdministrator(t, fixture)
	secret := "brclio-client-only-secret"
	if _, err := fixture.db.CreateAppPassword(context.Background(), admin.ID, "client", security.TokenHash(secret)); err != nil {
		t.Fatal(err)
	}
	response := performJSON(t, fixture.handler, http.MethodPost, "/api/auth/login", map[string]string{"email": admin.Email, "password": secret}, nil, "http://mail.test")
	if response.Code != http.StatusUnauthorized {
		t.Fatalf("app password web login status=%d body=%s", response.Code, response.Body.String())
	}
}
