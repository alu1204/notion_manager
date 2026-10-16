package proxy

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestEmailLoginImportsNamedWorkspace(t *testing.T) {
	var sawCSRF, sawDevice bool
	var loginBody map[string]any
	var sendBody map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/login":
			http.SetCookie(w, &http.Cookie{Name: "csrf", Value: "csrf-from-login", Path: "/"})
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write([]byte("<html>login</html>"))
		case "/api/v3/getLoginOptions":
			if r.Header.Get("x-csrf-token") != "csrf-from-login" {
				t.Errorf("getLoginOptions csrf = %q", r.Header.Get("x-csrf-token"))
			}
			writeJSON(w, http.StatusOK, map[string]any{
				"hasAccount":        true,
				"samlSignIn":        "unavailable",
				"passwordSignIn":    false,
				"loginOptionsToken": "login-token",
				"authFlowId":        "flow-1",
			})
		case "/api/v3/sendTemporaryPassword":
			_ = json.NewDecoder(r.Body).Decode(&sendBody)
			writeJSON(w, http.StatusOK, map[string]any{"csrfState": "state-from-notion"})
		case "/api/v3/loginWithEmail":
			if !strings.Contains(r.Header.Get("Cookie"), "csrf=csrf-from-login") {
				t.Errorf("login cookie = %q", r.Header.Get("Cookie"))
			}
			if !strings.Contains(r.Header.Get("Cookie"), "device_id=") {
				t.Errorf("login cookie missing device_id: %q", r.Header.Get("Cookie"))
			} else {
				sawDevice = true
			}
			if r.Header.Get("x-csrf-token") == "csrf-from-login" {
				sawCSRF = true
			}
			_ = json.NewDecoder(r.Body).Decode(&loginBody)
			http.SetCookie(w, &http.Cookie{Name: "token_v2", Value: "secret-token", Path: "/", HttpOnly: true})
			http.SetCookie(w, &http.Cookie{Name: "notion_user_id", Value: "target-user", Path: "/"})
			writeJSON(w, http.StatusOK, map[string]any{"isNewSignup": false, "userId": "target-user"})
		default:
			http.NotFound(w, r)
		}
	}))
	defer srv.Close()

	store := newEmailLoginStore(srv.URL, func(jar http.CookieJar) *http.Client {
		return &http.Client{Jar: jar, Timeout: 5 * time.Second}
	})
	stubDiscoveryByPath(t, func(req *http.Request) (int, string) {
		if !strings.Contains(req.Header.Get("Cookie"), "token_v2=secret-token") {
			t.Errorf("discovery cookie = %q", req.Header.Get("Cookie"))
		}
		return http.StatusOK, emailLoginCatalog
	})
	dir := t.TempDir()
	pool := NewAccountPool()
	auth := NewDashboardAuth("", "")

	startReq := httptest.NewRequest(http.MethodPost, "/admin/accounts/email/start", strings.NewReader(`{"email":"ada@example.com"}`))
	startRec := httptest.NewRecorder()
	handleEmailLoginStart(auth, store).ServeHTTP(startRec, startReq)
	if startRec.Code != http.StatusOK {
		t.Fatalf("start status %d body %s", startRec.Code, startRec.Body.String())
	}
	var started struct {
		SessionID string `json:"session_id"`
		Email     string `json:"email"`
	}
	if err := json.Unmarshal(startRec.Body.Bytes(), &started); err != nil {
		t.Fatal(err)
	}
	if started.SessionID == "" || started.Email != "ada@example.com" {
		t.Fatalf("start response = %+v", started)
	}
	if sendBody["loginOptionsToken"] != "login-token" || sendBody["disableLoginLink"] != true || sendBody["native"] != false {
		t.Fatalf("sendTemporaryPassword body = %#v", sendBody)
	}

	missReq := httptest.NewRequest(http.MethodPost, "/admin/accounts/email/verify", strings.NewReader(
		`{"session_id":"`+started.SessionID+`","code":"CODE","space_name":"不存在的空间"}`,
	))
	missRec := httptest.NewRecorder()
	handleEmailLoginVerify(pool, dir, auth, store).ServeHTTP(missRec, missReq)
	if missRec.Code != http.StatusConflict {
		t.Fatalf("missing workspace status %d body %s", missRec.Code, missRec.Body.String())
	}
	if !strings.Contains(missRec.Body.String(), "virgilcopelan3076的工作空间") {
		t.Fatalf("missing-workspace response did not list real spaces: %s", missRec.Body.String())
	}
	if loginBody["state"] != "state-from-notion" || loginBody["password"] != "CODE" {
		t.Fatalf("loginWithEmail body = %#v", loginBody)
	}
	if !sawCSRF || !sawDevice {
		t.Fatalf("csrf forwarded=%v device cookie=%v", sawCSRF, sawDevice)
	}

	verifyReq := httptest.NewRequest(http.MethodPost, "/admin/accounts/email/verify", strings.NewReader(
		`{"session_id":"`+started.SessionID+`","space_name":"virgilcopelan3076的工作空间"}`,
	))
	verifyRec := httptest.NewRecorder()
	handleEmailLoginVerify(pool, dir, auth, store).ServeHTTP(verifyRec, verifyReq)
	if verifyRec.Code != http.StatusOK {
		t.Fatalf("verify status %d body %s", verifyRec.Code, verifyRec.Body.String())
	}
	var verified struct {
		Workspaces []map[string]string `json:"workspaces"`
		Account    map[string]string   `json:"account"`
	}
	if err := json.Unmarshal(verifyRec.Body.Bytes(), &verified); err != nil {
		t.Fatal(err)
	}
	if len(verified.Workspaces) != 1 || verified.Account["space"] != "virgilcopelan3076的工作空间" {
		t.Fatalf("imported = %+v account %+v", verified.Workspaces, verified.Account)
	}
	if pool.GetByEmail("target@example.com") == nil {
		t.Fatal("account was not added to the pool")
	}
	matches, _ := filepath.Glob(filepath.Join(dir, "*.json"))
	if len(matches) != 1 {
		t.Fatalf("saved files = %d, want 1", len(matches))
	}
}

func TestEmailLoginImportsAllWhenUnfiltered(t *testing.T) {
	srv := emailLoginServer(t, false)
	defer srv.Close()
	store := testEmailStore(srv)
	stubDiscoveryByPath(t, func(*http.Request) (int, string) {
		return http.StatusOK, emailLoginCatalog
	})
	dir := t.TempDir()
	pool := NewAccountPool()
	sessionID := startEmailSession(t, store, "ada@example.com")

	req := httptest.NewRequest(http.MethodPost, "/admin/accounts/email/verify", strings.NewReader(
		`{"session_id":"`+sessionID+`","code":"123456"}`,
	))
	rec := httptest.NewRecorder()
	handleEmailLoginVerify(pool, dir, NewDashboardAuth("", ""), store).ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status %d body %s", rec.Code, rec.Body.String())
	}
	var verified struct {
		Workspaces []map[string]string `json:"workspaces"`
	}
	_ = json.Unmarshal(rec.Body.Bytes(), &verified)
	if len(verified.Workspaces) != 2 {
		t.Fatalf("workspaces = %+v", verified.Workspaces)
	}
}

func TestEmailLoginRejectsSAMLAndSurfacesNotionError(t *testing.T) {
	posts := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/login":
			w.WriteHeader(http.StatusOK)
		case "/api/v3/getLoginOptions":
			writeJSON(w, http.StatusOK, map[string]any{
				"hasAccount": true, "samlSignIn": "required", "loginOptionsToken": "tok", "authFlowId": "flow",
			})
		default:
			posts++
			http.Error(w, "unexpected", http.StatusInternalServerError)
		}
	}))
	defer srv.Close()
	store := testEmailStore(srv)
	req := httptest.NewRequest(http.MethodPost, "/admin/accounts/email/start", strings.NewReader(`{"email":"saml@example.com"}`))
	rec := httptest.NewRecorder()
	handleEmailLoginStart(NewDashboardAuth("", ""), store).ServeHTTP(rec, req)
	if rec.Code != http.StatusBadRequest || !strings.Contains(rec.Body.String(), "SAML") {
		t.Fatalf("saml status %d body %s", rec.Code, rec.Body.String())
	}
	if posts != 0 {
		t.Fatalf("sendTemporaryPassword calls = %d", posts)
	}

	bad := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/login" {
			w.WriteHeader(http.StatusOK)
			return
		}
		if r.URL.Path == "/api/v3/getLoginOptions" {
			writeJSON(w, http.StatusOK, map[string]any{
				"hasAccount": true, "samlSignIn": "unavailable", "loginOptionsToken": "tok", "authFlowId": "flow",
			})
			return
		}
		w.WriteHeader(http.StatusBadRequest)
		_, _ = io.WriteString(w, `{"isNotionError":true,"message":"Something went wrong. (400)","debugMessage":"We could not reach the email address you provided. Please try again with a different email.","clientData":{"type":"email_unreachable"}}`)
	}))
	defer bad.Close()
	rec = httptest.NewRecorder()
	req = httptest.NewRequest(http.MethodPost, "/admin/accounts/email/start", strings.NewReader(`{"email":"gone@example.com"}`))
	handleEmailLoginStart(NewDashboardAuth("", ""), testEmailStore(bad)).ServeHTTP(rec, req)
	if rec.Code != http.StatusBadRequest || !strings.Contains(rec.Body.String(), "email_unreachable") {
		t.Fatalf("unreachable status %d body %s", rec.Code, rec.Body.String())
	}
}

func TestEmailLoginSessionExpiryAndAuth(t *testing.T) {
	srv := emailLoginServer(t, false)
	defer srv.Close()
	store := testEmailStore(srv)
	startedAt := time.Now()
	store.now = func() time.Time { return startedAt }
	sessionID := startEmailSession(t, store, "ada@example.com")
	store.now = func() time.Time { return startedAt.Add(emailLoginTTL + time.Second) }

	req := httptest.NewRequest(http.MethodPost, "/admin/accounts/email/verify", strings.NewReader(
		`{"session_id":"`+sessionID+`","code":"123456"}`,
	))
	rec := httptest.NewRecorder()
	handleEmailLoginVerify(NewAccountPool(), t.TempDir(), NewDashboardAuth("", ""), store).ServeHTTP(rec, req)
	if rec.Code != http.StatusNotFound {
		t.Fatalf("expired status %d body %s", rec.Code, rec.Body.String())
	}

	auth := NewDashboardAuth("$sha256$salt$hash", "")
	rec = httptest.NewRecorder()
	req = httptest.NewRequest(http.MethodPost, "/admin/accounts/email/start", strings.NewReader(`{"email":"ada@example.com"}`))
	handleEmailLoginStart(auth, store).ServeHTTP(rec, req)
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("unauthorized status %d", rec.Code)
	}
}

func TestSelectDiscoveredAccountsByName(t *testing.T) {
	accounts := []*Account{
		{SpaceID: "space-a", SpaceName: "Alpha"},
		{SpaceID: "space-b", SpaceName: "virgilcopelan3076的工作空间"},
	}
	got, err := selectDiscoveredAccounts(accounts, "", "virgilcopelan3076的工作空间")
	if err != nil || len(got) != 1 || got[0].SpaceID != "space-b" {
		t.Fatalf("exact = %+v err %v", got, err)
	}
	got, err = selectDiscoveredAccounts(accounts, "space-a", "ignored")
	if err != nil || len(got) != 1 || got[0].SpaceID != "space-a" {
		t.Fatalf("id wins = %+v err %v", got, err)
	}
	got, err = selectDiscoveredAccounts(accounts, "", "")
	if err != nil || len(got) != 2 {
		t.Fatalf("all = %d err %v", len(got), err)
	}
	if _, err := selectDiscoveredAccounts(accounts, "", "missing"); err == nil {
		t.Fatal("expected missing name error")
	}
}

func emailLoginServer(t *testing.T, failCode bool) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/login":
			w.WriteHeader(http.StatusOK)
		case "/api/v3/getLoginOptions":
			writeJSON(w, http.StatusOK, map[string]any{
				"hasAccount": true, "samlSignIn": "unavailable", "passwordSignIn": false,
				"loginOptionsToken": "login-token", "authFlowId": "flow-1",
			})
		case "/api/v3/sendTemporaryPassword":
			writeJSON(w, http.StatusOK, map[string]any{"csrfState": "state-1"})
		case "/api/v3/loginWithEmail":
			if failCode {
				w.WriteHeader(http.StatusBadRequest)
				_, _ = io.WriteString(w, `{"message":"Invalid code"}`)
				return
			}
			http.SetCookie(w, &http.Cookie{Name: "token_v2", Value: "secret-token", Path: "/"})
			writeJSON(w, http.StatusOK, map[string]any{"userId": "target-user"})
		default:
			http.NotFound(w, r)
		}
	}))
}

func testEmailStore(srv *httptest.Server) *EmailLoginStore {
	return newEmailLoginStore(srv.URL, func(jar http.CookieJar) *http.Client {
		return &http.Client{Jar: jar, Timeout: 5 * time.Second}
	})
}

func startEmailSession(t *testing.T, store *EmailLoginStore, email string) string {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, "/admin/accounts/email/start", strings.NewReader(`{"email":"`+email+`"}`))
	rec := httptest.NewRecorder()
	handleEmailLoginStart(NewDashboardAuth("", ""), store).ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("start status %d body %s", rec.Code, rec.Body.String())
	}
	var started struct {
		SessionID string `json:"session_id"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &started); err != nil {
		t.Fatal(err)
	}
	return started.SessionID
}

const emailLoginCatalog = `{
  "users": {
    "target-user": {
      "notion_user": {"target-user": {"value":{"value":{"name":"Target","email":"target@example.com"}}}},
      "user_root": {"target-user": {"value":{"value":{"space_view_pointers":[
        {"spaceId":"space-a","id":"view-a"},
        {"spaceId":"space-b","id":"view-b"}
      ]}}}},
      "space": {
        "space-a": {"value":{"value":{"id":"space-a","name":"Alpha","plan_type":"free"}}},
        "space-b": {"value":{"value":{"id":"space-b","name":"virgilcopelan3076的工作空间","plan_type":"personal"}}}
      }
    }
  }
}`

func TestEmailLoginTokenFromSetCookieWhenJarMissesIt(t *testing.T) {
	// cookiejar stores HttpOnly cookies; this guards the header fallback by
	// parsing a raw Set-Cookie line the same way the verify step does.
	header := http.Header{}
	header.Add("Set-Cookie", "token_v2=header-token; Path=/; HttpOnly")
	header.Add("Set-Cookie", "notion_user_id=user-from-cookie; Path=/")
	if got := cookieFromHeader(header, "token_v2"); got != "header-token" {
		t.Fatalf("token = %q", got)
	}
	if got := cookieFromHeader(header, "notion_user_id"); got != "user-from-cookie" {
		t.Fatalf("user = %q", got)
	}
	_ = os.DevNull
}
