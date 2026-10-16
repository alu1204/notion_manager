package proxy

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/http/cookiejar"
	"net/url"
	"strings"
	"sync"
	"time"
)

const (
	notionPublicOrigin = "https://www.notion.so"
	emailLoginTTL      = 10 * time.Minute
	emailLoginUA       = "Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/136.0.0.0 Safari/537.36"
)

// emailLoginSession keeps the cookie jar and Notion login state between
// "send code" and "submit code". The jar is what carries device cookies
// (and csrf, when Notion sets it) across the two requests.
type emailLoginSession struct {
	ID           string
	Email        string
	ExpiresAt    time.Time
	CSRFState    string
	DeviceID     string
	UserID       string
	TokenV2      string
	client       *http.Client
	apiBase      string
	loginPageURL string
}

// EmailLoginStore is the in-memory session table for the two-step login.
type EmailLoginStore struct {
	mu        sync.Mutex
	sessions  map[string]*emailLoginSession
	apiBase   string
	loginPage string
	newClient func(jar http.CookieJar) *http.Client
	now       func() time.Time
}

func newEmailLoginStore(apiBase string, newClient func(jar http.CookieJar) *http.Client) *EmailLoginStore {
	if apiBase == "" {
		apiBase = notionPublicOrigin
	}
	apiBase = strings.TrimRight(apiBase, "/")
	return &EmailLoginStore{
		sessions:  map[string]*emailLoginSession{},
		apiBase:   apiBase,
		loginPage: apiBase + "/login",
		newClient: newClient,
		now:       time.Now,
	}
}

func newChromeEmailClient(jar http.CookieJar) *http.Client {
	base := getChromeHTTPClient(45 * time.Second)
	return &http.Client{
		Timeout:   45 * time.Second,
		Jar:       jar,
		Transport: base.Transport,
	}
}

var defaultEmailLoginStore = newEmailLoginStore(notionPublicOrigin, newChromeEmailClient)

// HandleEmailLoginStart sends a Notion email login code.
// POST /admin/accounts/email/start {"email":"..."}
func HandleEmailLoginStart(auth *DashboardAuth) http.HandlerFunc {
	return handleEmailLoginStart(auth, nil)
}

func handleEmailLoginStart(auth *DashboardAuth, store *EmailLoginStore) http.HandlerFunc {
	if store == nil {
		store = defaultEmailLoginStore
	}
	return func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			writeJSON(w, http.StatusMethodNotAllowed, map[string]string{"error": "method not allowed"})
			return
		}
		if !dashboardAuthorized(auth, r) {
			writeJSON(w, http.StatusUnauthorized, map[string]string{"error": "unauthorized, dashboard login required"})
			return
		}
		var body struct {
			Email string `json:"email"`
		}
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid request body"})
			return
		}
		email := strings.TrimSpace(body.Email)
		if !strings.Contains(email, "@") || strings.ContainsAny(email, " \t\r\n") {
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": "a valid email is required"})
			return
		}

		session, meta, err := store.begin(email)
		if err != nil {
			log.Printf("[email-login] start failed for %s: %v", email, err)
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": err.Error()})
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{
			"status":             "ok",
			"session_id":         session.ID,
			"email":              session.Email,
			"expires_in":         int(time.Until(session.ExpiresAt).Seconds()),
			"password_signin":    meta.PasswordSignIn,
			"has_account":        meta.HasAccount,
			"challenge_provider": meta.ChallengeProvider,
		})
	}
}

// HandleEmailLoginVerify submits the email code, then imports workspaces.
// POST /admin/accounts/email/verify {"session_id","code","space_id"?,"space_name"?}
// Omit space_id and space_name to import every workspace on the account.
func HandleEmailLoginVerify(pool *AccountPool, accountsDir string, auth *DashboardAuth) http.HandlerFunc {
	return handleEmailLoginVerify(pool, accountsDir, auth, nil)
}

func handleEmailLoginVerify(pool *AccountPool, accountsDir string, auth *DashboardAuth, store *EmailLoginStore) http.HandlerFunc {
	if store == nil {
		store = defaultEmailLoginStore
	}
	return func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			writeJSON(w, http.StatusMethodNotAllowed, map[string]string{"error": "method not allowed"})
			return
		}
		if !dashboardAuthorized(auth, r) {
			writeJSON(w, http.StatusUnauthorized, map[string]string{"error": "unauthorized, dashboard login required"})
			return
		}
		var body struct {
			SessionID string `json:"session_id"`
			Code      string `json:"code"`
			SpaceID   string `json:"space_id"`
			SpaceName string `json:"space_name"`
		}
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid request body"})
			return
		}
		sessionID := strings.TrimSpace(body.SessionID)
		if sessionID == "" {
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": "session_id is required"})
			return
		}
		token, userID, err := store.complete(sessionID, strings.TrimSpace(body.Code))
		if err != nil {
			status := http.StatusBadRequest
			if err == errEmailLoginNotFound {
				status = http.StatusNotFound
			}
			writeJSON(w, status, map[string]string{"error": err.Error()})
			return
		}

		accounts, err := DiscoverAccountsFromTokenWithOptions(token, AccountDiscoveryOptions{ActiveUserID: userID})
		if err != nil {
			writeJSON(w, http.StatusBadRequest, map[string]any{
				"error":      fmt.Sprintf("登录成功，但读取工作组失败: %v", err),
				"session_id": sessionID,
			})
			return
		}
		selected, err := selectDiscoveredAccounts(accounts, body.SpaceID, body.SpaceName)
		if err != nil {
			writeJSON(w, http.StatusConflict, map[string]any{
				"error":      err.Error(),
				"session_id": sessionID,
				"workspaces": workspaceListPayload(accounts),
			})
			return
		}

		best := preferredDiscoveredAccount(selected, strings.TrimSpace(body.SpaceID))
		saved := make([]map[string]string, 0, len(selected))
		bestFilename := ""
		for _, acc := range selected {
			filename, saveErr := SaveAccountToFile(acc, accountsDir)
			if saveErr != nil {
				writeJSON(w, http.StatusInternalServerError, map[string]string{
					"error": fmt.Sprintf("Failed to save account: %v", saveErr),
				})
				return
			}
			pool.AddAccount(acc)
			if best != nil && acc.AccountID == best.AccountID {
				bestFilename = filename
			}
			saved = append(saved, workspacePayload(acc, filename))
		}
		store.drop(sessionID)
		if bestFilename == "" && len(saved) > 0 {
			bestFilename = saved[0]["filename"]
		}
		if best == nil {
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": "no workspace to import"})
			return
		}
		log.Printf("[email-login] imported %d workspace(s) for %s", len(saved), best.UserEmail)
		writeJSON(w, http.StatusOK, map[string]any{
			"status":   "ok",
			"filename": bestFilename,
			"account": map[string]string{
				"name":      best.UserName,
				"email":     best.UserEmail,
				"space":     best.SpaceName,
				"plan_type": best.PlanType,
			},
			"workspaces": saved,
		})
	}
}

func dashboardAuthorized(auth *DashboardAuth, r *http.Request) bool {
	if auth == nil || !auth.HasAdminPassword() {
		return true
	}
	return auth.ValidateSession(r)
}

type loginOptionsMeta struct {
	LoginOptionsToken string `json:"loginOptionsToken"`
	AuthFlowID        string `json:"authFlowId"`
	SAMLSignIn        string `json:"samlSignIn"`
	HasAccount        bool   `json:"hasAccount"`
	MustReverify      bool   `json:"mustReverify"`
	PasswordSignIn    bool   `json:"passwordSignIn"`
	ChallengeProvider string `json:"challengeProvider"`
}

func (s *EmailLoginStore) begin(email string) (*emailLoginSession, loginOptionsMeta, error) {
	s.prune()
	jar, err := cookiejar.New(nil)
	if err != nil {
		return nil, loginOptionsMeta{}, err
	}
	client := s.newClient(jar)
	if client.Jar == nil {
		client.Jar = jar
	}
	session := &emailLoginSession{
		ID:           generateUUIDv4(),
		Email:        email,
		ExpiresAt:    s.now().Add(emailLoginTTL),
		client:       client,
		apiBase:      s.apiBase,
		loginPageURL: s.loginPage,
	}

	if err := session.warmLoginPage(); err != nil {
		return nil, loginOptionsMeta{}, err
	}
	session.DeviceID = session.ensureDeviceID()

	status, body, err := session.postJSON("/api/v3/getLoginOptions", map[string]any{
		"email":                email,
		"requireWorkTypeEmail": false,
	})
	if err != nil {
		return nil, loginOptionsMeta{}, fmt.Errorf("getLoginOptions: %w", err)
	}
	if status < 200 || status >= 300 {
		return nil, loginOptionsMeta{}, fmt.Errorf("getLoginOptions: %s", notionAPIError(status, body))
	}
	var meta loginOptionsMeta
	if err := json.Unmarshal(body, &meta); err != nil {
		return nil, loginOptionsMeta{}, fmt.Errorf("getLoginOptions: invalid response")
	}
	if strings.EqualFold(meta.SAMLSignIn, "required") {
		return nil, meta, fmt.Errorf("这个邮箱要求 SAML 单点登录，不能用邮件验证码登录")
	}
	if meta.LoginOptionsToken == "" {
		return nil, meta, fmt.Errorf("getLoginOptions 没有返回 loginOptionsToken")
	}
	if meta.AuthFlowID == "" {
		meta.AuthFlowID = generateUUIDv4()
	}

	// The web client sends the code request immediately for accounts without
	// a password. Password accounts still accept this call; Notion decides
	// whether to mail a code. challengeToken is omitted because this server
	// cannot solve hCaptcha — Notion currently validates the address before
	// rejecting a missing token, and a captcha failure is returned as an error.
	sendPayload := map[string]any{
		"email":              email,
		"redirectURL":        session.loginPageURL,
		"disableLoginLink":   true,
		"native":             false,
		"isSignup":           false,
		"shouldHidePasscode": false,
		"loginOptionsToken":  meta.LoginOptionsToken,
		"authFlowId":         meta.AuthFlowID,
		"deviceId":           session.DeviceID,
		"loginRouteOrigin":   "login",
	}
	status, body, err = session.postJSON("/api/v3/sendTemporaryPassword", sendPayload)
	if err != nil {
		return nil, meta, fmt.Errorf("sendTemporaryPassword: %w", err)
	}
	if status < 200 || status >= 300 {
		return nil, meta, fmt.Errorf("sendTemporaryPassword: %s", notionAPIError(status, body))
	}
	var sent struct {
		CSRFState string `json:"csrfState"`
	}
	if err := json.Unmarshal(body, &sent); err != nil || sent.CSRFState == "" {
		return nil, meta, fmt.Errorf("sendTemporaryPassword 没有返回 csrfState")
	}
	session.CSRFState = sent.CSRFState

	s.mu.Lock()
	s.sessions[session.ID] = session
	s.mu.Unlock()
	log.Printf("[email-login] code requested for %s session=%s", email, session.ID)
	return session, meta, nil
}

func (s *EmailLoginStore) complete(sessionID, code string) (string, string, error) {
	s.prune()
	s.mu.Lock()
	session := s.sessions[sessionID]
	s.mu.Unlock()
	if session == nil {
		return "", "", errEmailLoginNotFound
	}
	if session.TokenV2 != "" {
		return session.TokenV2, session.UserID, nil
	}
	if code == "" {
		return "", "", fmt.Errorf("code is required")
	}

	// Current web login submits the temporary passcode as `password` together
	// with the csrfState from sendTemporaryPassword as `state`.
	status, body, hdr, err := session.postJSONHeaders("/api/v3/loginWithEmail", map[string]any{
		"state":            session.CSRFState,
		"password":         code,
		"appSource":        "notion",
		"loginRouteOrigin": "login",
	})
	if err != nil {
		return "", "", fmt.Errorf("loginWithEmail: %w", err)
	}
	if status < 200 || status >= 300 {
		return "", "", fmt.Errorf("loginWithEmail: %s", notionAPIError(status, body))
	}
	var logged struct {
		UserID string `json:"userId"`
	}
	_ = json.Unmarshal(body, &logged)

	token := session.cookie("token_v2")
	if token == "" {
		token = cookieFromHeader(hdr, "token_v2")
	}
	userID := logged.UserID
	if userID == "" {
		userID = session.cookie("notion_user_id")
	}
	if userID == "" {
		userID = cookieFromHeader(hdr, "notion_user_id")
	}
	if token == "" {
		return "", "", fmt.Errorf("登录响应里没有 token_v2")
	}
	session.TokenV2 = token
	session.UserID = userID
	return token, userID, nil
}

func (s *EmailLoginStore) drop(id string) {
	s.mu.Lock()
	delete(s.sessions, id)
	s.mu.Unlock()
}

func (s *EmailLoginStore) prune() {
	now := s.now()
	s.mu.Lock()
	defer s.mu.Unlock()
	for id, session := range s.sessions {
		if !session.ExpiresAt.After(now) {
			delete(s.sessions, id)
		}
	}
}

var errEmailLoginNotFound = fmt.Errorf("登录会话不存在或已过期，请重新发送验证码")

func (s *emailLoginSession) warmLoginPage() error {
	req, err := http.NewRequest(http.MethodGet, s.loginPageURL, nil)
	if err != nil {
		return err
	}
	req.Header.Set("User-Agent", emailLoginUA)
	req.Header.Set("Accept", "text/html,application/xhtml+xml")
	resp, err := s.client.Do(req)
	if err != nil {
		return fmt.Errorf("open login page: %w", err)
	}
	io.Copy(io.Discard, resp.Body)
	resp.Body.Close()
	return nil
}

func (s *emailLoginSession) ensureDeviceID() string {
	if id := s.cookie("device_id"); id != "" {
		s.rememberCookie("device_id", id)
		return id
	}
	id := generateUUIDv4()
	s.rememberCookie("device_id", id)
	return id
}

func (s *emailLoginSession) cookie(name string) string {
	if s.client == nil || s.client.Jar == nil {
		return ""
	}
	// GET /login currently redirects to app.notion.com, which sets device_id
	// on that host. API calls stay on www.notion.so, so look in both jars.
	for _, raw := range []string{s.apiBase, s.loginPageURL, "https://www.notion.so/", "https://app.notion.com/login"} {
		u, err := url.Parse(raw)
		if err != nil {
			continue
		}
		for _, ck := range s.client.Jar.Cookies(u) {
			if ck.Name == name && ck.Value != "" {
				return ck.Value
			}
		}
	}
	return ""
}

func (s *emailLoginSession) rememberCookie(name, value string) {
	if s.client == nil || s.client.Jar == nil || value == "" {
		return
	}
	u, err := url.Parse(s.apiBase)
	if err != nil {
		return
	}
	s.client.Jar.SetCookies(u, []*http.Cookie{{Name: name, Value: value, Path: "/"}})
}

func (s *emailLoginSession) postJSON(path string, payload any) (int, []byte, error) {
	status, body, _, err := s.postJSONHeaders(path, payload)
	return status, body, err
}

func (s *emailLoginSession) postJSONHeaders(path string, payload any) (int, []byte, http.Header, error) {
	raw, err := json.Marshal(payload)
	if err != nil {
		return 0, nil, nil, err
	}
	req, err := http.NewRequest(http.MethodPost, s.apiBase+path, bytes.NewReader(raw))
	if err != nil {
		return 0, nil, nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json")
	req.Header.Set("Origin", notionPublicOrigin)
	req.Header.Set("Referer", s.loginPageURL)
	req.Header.Set("User-Agent", emailLoginUA)
	req.Header.Set("notion-client-version", DefaultClientVersion)
	if csrf := s.cookie("csrf"); csrf != "" {
		req.Header.Set("x-csrf-token", csrf)
	}
	resp, err := s.client.Do(req)
	if err != nil {
		return 0, nil, nil, err
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return resp.StatusCode, nil, resp.Header, err
	}
	return resp.StatusCode, body, resp.Header, nil
}

func notionAPIError(status int, body []byte) string {
	var payload struct {
		Message      string `json:"message"`
		DebugMessage string `json:"debugMessage"`
		ClientData   struct {
			Type string `json:"type"`
		} `json:"clientData"`
	}
	_ = json.Unmarshal(body, &payload)
	kind := payload.ClientData.Type
	if kind != "" && (strings.Contains(strings.ToLower(kind), "captcha") || strings.Contains(strings.ToLower(kind), "challenge")) {
		return "Notion 要求人机验证（" + kind + "），服务器无法代过验证码，请改用 token_v2 导入"
	}
	if payload.DebugMessage != "" && (payload.Message == "" || strings.Contains(payload.Message, "Something went wrong")) {
		if kind != "" {
			return payload.DebugMessage + " (" + kind + ")"
		}
		return payload.DebugMessage
	}
	if payload.Message != "" {
		return payload.Message
	}
	text := strings.TrimSpace(string(body))
	if len(text) > 300 {
		text = text[:300]
	}
	if text == "" {
		return fmt.Sprintf("HTTP %d", status)
	}
	return text
}

func cookieFromHeader(header http.Header, name string) string {
	if header == nil {
		return ""
	}
	prefix := name + "="
	for _, line := range header.Values("Set-Cookie") {
		line = strings.TrimSpace(line)
		if !strings.HasPrefix(line, prefix) {
			continue
		}
		value := strings.TrimPrefix(line, prefix)
		if i := strings.IndexByte(value, ';'); i >= 0 {
			value = value[:i]
		}
		return value
	}
	return ""
}

func selectDiscoveredAccounts(accounts []*Account, spaceID, spaceName string) ([]*Account, error) {
	spaceID = strings.TrimSpace(spaceID)
	spaceName = strings.TrimSpace(spaceName)
	if spaceID == "" && spaceName == "" {
		if len(accounts) == 0 {
			return nil, fmt.Errorf("这个账号没有可导入的工作组")
		}
		return accounts, nil
	}
	if spaceID != "" {
		var matched []*Account
		for _, acc := range accounts {
			if acc != nil && acc.SpaceID == spaceID {
				matched = append(matched, acc)
			}
		}
		if len(matched) == 0 {
			return nil, fmt.Errorf("没有找到 space_id=%s 的工作组", spaceID)
		}
		return matched, nil
	}

	var exact, folded, contains []*Account
	query := strings.ToLower(spaceName)
	for _, acc := range accounts {
		if acc == nil {
			continue
		}
		name := strings.TrimSpace(acc.SpaceName)
		if name == spaceName {
			exact = append(exact, acc)
			continue
		}
		if strings.EqualFold(name, spaceName) {
			folded = append(folded, acc)
			continue
		}
		if strings.Contains(strings.ToLower(name), query) {
			contains = append(contains, acc)
		}
	}
	switch {
	case len(exact) > 0:
		return exact, nil
	case len(folded) > 0:
		return folded, nil
	case len(contains) == 1:
		return contains, nil
	default:
		return nil, fmt.Errorf("没有找到名为「%s」的工作组", spaceName)
	}
}

func workspaceListPayload(accounts []*Account) []map[string]string {
	out := make([]map[string]string, 0, len(accounts))
	for _, acc := range accounts {
		out = append(out, workspacePayload(acc, ""))
	}
	return out
}

func writeJSON(w http.ResponseWriter, status int, payload any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(payload)
}
