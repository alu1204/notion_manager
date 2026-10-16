package proxy

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"

	"notion-manager/internal/notionapi"
)

// AccountDTO is the safe admin/dashboard representation.
// Never contains token_v2, cookies, or authorization headers.
type AccountDTO struct {
	AccountID    string     `json:"account_id"`
	UserEmail    string     `json:"user_email"`
	UserName     string     `json:"user_name"`
	SpaceName    string     `json:"space_name"`
	SpaceIDShort string     `json:"space_id_short"`
	PlanType     string     `json:"plan_type"`
	QuotaInfo    *QuotaInfo `json:"quota_info,omitempty"`
	IsExhausted  bool       `json:"is_exhausted"`
	IsPermanent  bool       `json:"is_permanently_exhausted"`
	SpaceCount   int        `json:"space_count"`
}

// NewAccountDTO builds a safe DTO from an Account (no secrets).
func NewAccountDTO(acc *Account) AccountDTO {
	acc.EnsureAccountID()
	quota := acc.quotaSnapshot()
	return AccountDTO{
		AccountID:    acc.AccountID,
		UserEmail:    acc.UserEmail,
		UserName:     acc.UserName,
		SpaceName:    acc.SpaceName,
		SpaceIDShort: acc.ShortSpaceID(),
		PlanType:     acc.PlanType,
		QuotaInfo:    quota.Info,
		IsExhausted:  quota.ExhaustedAt != nil,
		IsPermanent:  quota.PermanentlyExhausted,
		SpaceCount:   acc.SpaceCount,
	}
}

// DiscoverAccountFromToken calls Notion APIs using the given token_v2 to discover
// all account information (user, space, models, quota).
func DiscoverAccountFromToken(tokenV2 string) (*Account, error) {
	return DiscoverAccountFromTokenWithOptions(tokenV2, AccountDiscoveryOptions{})
}

// AccountDiscoveryOptions pins a multi-account Notion session to the intended
// active user and workspace. Empty fields retain the legacy automatic choice.
type AccountDiscoveryOptions struct {
	ActiveUserID  string
	SpaceID       string
	ExpectedEmail string
}

var (
	discoveryHTTPClient  = getChromeHTTPClient
	discoveryFetchModels = FetchModels
	discoveryCheckQuota  = CheckQuota
)

// DiscoverAccountFromTokenWithOptions discovers an account while applying
// optional exact selectors for multi-account browser sessions. It returns
// the single best workspace. Startup uses this seam and stays one account.
func DiscoverAccountFromTokenWithOptions(tokenV2 string, options AccountDiscoveryOptions) (*Account, error) {
	accounts, err := DiscoverAccountsFromTokenWithOptions(tokenV2, options)
	if err != nil {
		return nil, err
	}
	best := preferredDiscoveredAccount(accounts, options.SpaceID)
	if best == nil {
		return nil, fmt.Errorf("no workspace found for this account")
	}
	return best, nil
}

// DiscoverAccountsFromTokenWithOptions lists every workspace that matches
// the selectors. A pinned SpaceID returns only that workspace.
func DiscoverAccountsFromTokenWithOptions(tokenV2 string, options AccountDiscoveryOptions) ([]*Account, error) {
	client := discoveryHTTPClient(AppConfig.APITimeoutDuration())
	var lastStatus int
	var lastBody []byte
	cat, err := notionapi.FetchCatalog(discoveryPoster(client, tokenV2, options, &lastStatus, &lastBody))
	if err != nil {
		return nil, fmt.Errorf("loadUserContent request failed: %w", err)
	}
	if len(cat.Users) == 0 && lastStatus != 0 && lastStatus != http.StatusOK {
		return nil, fmt.Errorf("loadUserContent API error %d: %s", lastStatus, truncate(string(lastBody), 300))
	}

	activeUserID := strings.TrimSpace(options.ActiveUserID)
	expectedEmail := strings.TrimSpace(options.ExpectedEmail)
	requestedSpaceID := strings.TrimSpace(options.SpaceID)
	var matched []notionapi.User
	for _, user := range cat.Users {
		if user.ID == "" {
			continue
		}
		if activeUserID != "" && user.ID != activeUserID {
			continue
		}
		if expectedEmail != "" && !strings.EqualFold(user.Email, expectedEmail) {
			continue
		}
		if requestedSpaceID != "" && !userHasSpace(user, requestedSpaceID) {
			continue
		}
		matched = append(matched, user)
	}
	if len(matched) == 0 {
		if activeUserID != "" || expectedEmail != "" || requestedSpaceID != "" {
			return nil, fmt.Errorf("no user matched the configured Notion account selectors")
		}
		return nil, fmt.Errorf("no user found in loadUserContent response")
	}

	now := time.Now()
	browserID := generateUUIDv4()
	deviceID := generateUUIDv4()
	var accounts []*Account
	for _, user := range matched {
		spaces := user.Workspaces
		if requestedSpaceID != "" {
			var pinned []notionapi.Workspace
			for _, ws := range spaces {
				if ws.SpaceID == requestedSpaceID {
					pinned = append(pinned, ws)
				}
			}
			if len(pinned) == 0 {
				return nil, fmt.Errorf("requested NOTION_SPACE_ID was not found for the active account")
			}
			spaces = pinned
		}
		if len(spaces) == 0 {
			continue
		}
		timezone := user.Timezone
		if timezone == "" {
			timezone = "UTC"
		}
		checkedAt := now
		for _, ws := range spaces {
			acc := &Account{
				TokenV2:            tokenV2,
				UserID:             user.ID,
				UserName:           user.Name,
				UserEmail:          user.Email,
				SpaceID:            ws.SpaceID,
				SpaceName:          ws.Name,
				SpaceViewID:        ws.SpaceViewID,
				PlanType:           ws.PlanType,
				Timezone:           timezone,
				ClientVersion:      DefaultClientVersion,
				BrowserID:          browserID,
				DeviceID:           deviceID,
				SpaceCount:         len(user.Workspaces),
				WorkspaceCheckedAt: &checkedAt,
			}
			acc.EnsureAccountID()
			models, modelErr := discoveryFetchModels(acc)
			if modelErr != nil {
				log.Printf("[add-account] model fetch failed (non-fatal): %v", modelErr)
			} else {
				acc.setModels(models)
			}
			quota, quotaErr := discoveryCheckQuota(acc)
			if quotaErr != nil {
				log.Printf("[add-account] quota check failed (non-fatal): %v", quotaErr)
			} else {
				checked := time.Now()
				acc.setQuotaInfo(quota, &checked)
			}
			accounts = append(accounts, acc)
		}
	}
	if len(accounts) == 0 {
		if requestedSpaceID != "" {
			return nil, fmt.Errorf("requested NOTION_SPACE_ID was not found for the active account")
		}
		return nil, fmt.Errorf("no workspace found for this account")
	}
	return accounts, nil
}

func discoveryPoster(client *http.Client, tokenV2 string, options AccountDiscoveryOptions, lastStatus *int, lastBody *[]byte) notionapi.Poster {
	return func(endpoint string, body []byte, extra map[string]string) (int, []byte, error) {
		req, err := http.NewRequest("POST", NotionAPIBase+"/"+endpoint, bytes.NewReader(body))
		if err != nil {
			return 0, nil, fmt.Errorf("create loadUserContent request: %w", err)
		}
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("Accept", "application/json")
		req.Header.Set("User-Agent", AppConfig.Browser.UserAgent)
		activeUserID := strings.TrimSpace(options.ActiveUserID)
		cookieHeader := "token_v2=" + tokenV2
		if activeUserID != "" {
			cookieHeader = accountCookieHeader(&Account{TokenV2: tokenV2, UserID: activeUserID})
			req.Header.Set("x-notion-active-user-header", activeUserID)
		}
		req.Header.Set("Cookie", cookieHeader)
		for key, value := range extra {
			if key != "" && value != "" {
				req.Header.Set(key, value)
			}
		}
		resp, err := client.Do(req)
		if err != nil {
			return 0, nil, err
		}
		defer resp.Body.Close()
		raw, _ := io.ReadAll(resp.Body)
		if resp.StatusCode != http.StatusOK && lastStatus != nil {
			*lastStatus = resp.StatusCode
			if lastBody != nil {
				*lastBody = raw
			}
		}
		return resp.StatusCode, raw, nil
	}
}

func userHasSpace(user notionapi.User, spaceID string) bool {
	for _, ws := range user.Workspaces {
		if ws.SpaceID == spaceID {
			return true
		}
	}
	return false
}

func preferredDiscoveredAccount(accounts []*Account, requestedSpaceID string) *Account {
	if len(accounts) == 0 {
		return nil
	}
	requestedSpaceID = strings.TrimSpace(requestedSpaceID)
	if requestedSpaceID != "" {
		for _, acc := range accounts {
			if acc.SpaceID == requestedSpaceID {
				return acc
			}
		}
	}
	best := accounts[0]
	bestRank := discoveredWorkspaceRank(best)
	for _, acc := range accounts[1:] {
		if rank := discoveredWorkspaceRank(acc); rank > bestRank {
			best = acc
			bestRank = rank
		}
	}
	return best
}

func discoveredWorkspaceRank(acc *Account) int {
	if acc == nil {
		return 0
	}
	rank := 2
	if acc.PlanType != "" && !strings.EqualFold(acc.PlanType, "free") {
		rank++
	}
	return rank
}

// SaveAccountToFile writes an Account to a JSON file in the accounts directory.
// The filename uses the full account_id for collision safety:
//
//	<account_id>__<sanitized_email>.json
//
// If a file with the same account_id already exists (possibly under an old
// naming scheme), it is overwritten at the same path. This prevents
// duplicate files after migration.
// findFileByAccountIDOnDisk scans dir for a JSON file whose "account_id" field
// (or computed account_id from user_id+space_id) matches the given accountID.
// Returns the full path if found, or "" if no match exists.
func findFileByAccountIDOnDisk(dir, accountID string) (string, error) {
	entries, err := readPrivateAccountsDir(dir)
	if err != nil {
		return "", err
	}
	for _, entry := range entries {
		if entry.IsDir() || filepath.Ext(entry.Name()) != ".json" {
			continue
		}
		path := filepath.Join(dir, entry.Name())
		data, err := readPrivateAccountFile(path)
		if err != nil {
			return "", err
		}
		var m map[string]interface{}
		if err := json.Unmarshal(data, &m); err != nil {
			continue
		}
		aid, _ := m["account_id"].(string)
		if aid == "" {
			uid, _ := m["user_id"].(string)
			sid, _ := m["space_id"].(string)
			if uid != "" && sid != "" {
				aid = ComputeAccountID(uid, sid)
			}
		}
		if aid == accountID {
			return path, nil
		}
	}
	return "", nil
}

func SaveAccountToFile(acc *Account, dir string) (string, error) {
	acc.EnsureAccountID()
	if err := ensurePrivateAccountsDir(dir); err != nil {
		return "", fmt.Errorf("create accounts dir: %w", err)
	}

	name := acc.UserEmail
	if name == "" {
		name = acc.UserName
	}
	if name == "" {
		name = "unknown"
	}
	// Sanitize filename
	name = strings.Map(func(r rune) rune {
		if r == '@' || r == '.' || r == '-' || r == '_' || (r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z') || (r >= '0' && r <= '9') {
			return r
		}
		return '_'
	}, name)

	newFilename := acc.AccountID + "__" + name + ".json"

	// Check if an existing file already has this account_id (could be old
	// naming scheme like "<aid[:12]>_email.json"). If found, reuse that path
	// to avoid duplicates; otherwise use the new canonical name.
	filename := newFilename
	existingPath, err := findFileByAccountIDOnDisk(dir, acc.AccountID)
	if err != nil {
		return "", fmt.Errorf("find existing account file: %w", err)
	}
	if existingPath != "" {
		filename = filepath.Base(existingPath)
	}
	path := filepath.Join(dir, filename)

	// Build the JSON structure
	acc.EnsureAccountID()
	data := map[string]interface{}{
		"account_id":     acc.AccountID,
		"token_v2":       acc.TokenV2,
		"user_id":        acc.UserID,
		"user_name":      acc.UserName,
		"user_email":     acc.UserEmail,
		"space_id":       acc.SpaceID,
		"space_name":     acc.SpaceName,
		"space_view_id":  acc.SpaceViewID,
		"plan_type":      acc.PlanType,
		"timezone":       acc.Timezone,
		"client_version": acc.ClientVersion,
		"browser_id":     acc.BrowserID,
		"device_id":      acc.DeviceID,
	}
	modelSnapshot := acc.modelsSnapshot()
	quota := acc.quotaSnapshot()
	if len(modelSnapshot) > 0 {
		var models []map[string]string
		for _, m := range modelSnapshot {
			models = append(models, map[string]string{"id": m.ID, "name": m.Name})
		}
		data["available_models"] = models
	}
	if quota.Info != nil {
		data["quota_info"] = map[string]interface{}{
			"is_eligible":         quota.Info.IsEligible,
			"space_usage":         quota.Info.SpaceUsage,
			"space_limit":         quota.Info.SpaceLimit,
			"user_usage":          quota.Info.UserUsage,
			"user_limit":          quota.Info.UserLimit,
			"last_usage_at":       quota.Info.LastUsageAtMs,
			"research_mode_usage": quota.Info.ResearchModeUsage,
			"has_premium":         quota.Info.HasPremium,
			"premium_balance":     quota.Info.PremiumBalance,
			"premium_usage":       quota.Info.PremiumUsage,
			"premium_limit":       quota.Info.PremiumLimit,
		}
	}
	if quota.CheckedAt != nil {
		data["quota_checked_at"] = quota.CheckedAt.Format(time.RFC3339)
	}
	if acc.WorkspaceCheckedAt != nil {
		data["space_count"] = acc.SpaceCount
		data["workspace_checked_at"] = acc.WorkspaceCheckedAt.Format(time.RFC3339)
	}
	data["extracted_at"] = time.Now().Format(time.RFC3339)

	out, err := json.MarshalIndent(data, "", "  ")
	if err != nil {
		return "", fmt.Errorf("marshal account JSON: %w", err)
	}

	// Ensure accounts directory exists
	if err := ensurePrivateAccountsDir(dir); err != nil {
		return "", fmt.Errorf("create accounts dir: %w", err)
	}

	if err := writePrivateAccountFile(path, append(out, '\n')); err != nil {
		return "", fmt.Errorf("write account file: %w", err)
	}

	return filename, nil
}

// AddAccount adds an account to the pool (hot-load, no restart needed).
func (p *AccountPool) AddAccount(acc *Account) {
	p.mu.Lock()
	defer p.mu.Unlock()
	// Check for duplicate by account_id (same user_id + space_id)
	acc.EnsureAccountID()
	for i, existing := range p.accounts {
		existing.EnsureAccountID()
		if existing.AccountID != "" && existing.AccountID == acc.AccountID {
			// Replace existing (same workspace)
			p.accounts[i] = acc
			log.Printf("[account] replaced: %s (%s) aid=%s", acc.UserName, acc.UserEmail, acc.ShortSpaceID())
			return
		}
	}
	p.accounts = append(p.accounts, acc)
	log.Printf("[account] added: %s (%s) [%s]", acc.UserName, acc.UserEmail, acc.PlanType)
}

// DeleteAccountFile removes the JSON file for an account from the accounts directory.
// Matches by account_id first; falls back to user_id+space_id.
func DeleteAccountFile(accountID, dir string) error {
	entries, err := readPrivateAccountsDir(dir)
	if err != nil {
		return err
	}
	for _, entry := range entries {
		if entry.IsDir() || filepath.Ext(entry.Name()) != ".json" {
			continue
		}
		path := filepath.Join(dir, entry.Name())
		data, err := readPrivateAccountFile(path)
		if err != nil {
			return err
		}
		var existing map[string]interface{}
		if err := json.Unmarshal(data, &existing); err != nil {
			continue
		}
		exAID, _ := existing["account_id"].(string)
		if exAID == "" {
			// Legacy file: compute from user_id+space_id
			exUID, _ := existing["user_id"].(string)
			exSID, _ := existing["space_id"].(string)
			if exUID != "" && exSID != "" {
				exAID = ComputeAccountID(exUID, exSID)
			}
		}
		if exAID == accountID {
			if err := os.Remove(path); err != nil {
				return fmt.Errorf("delete file %s: %w", entry.Name(), err)
			}
			log.Printf("[account] deleted file: %s", entry.Name())
			return nil
		}
	}
	return fmt.Errorf("account file not found for account_id %s", accountID)
}

// DeleteAccountFileByEmail removes the JSON file by email (legacy).
// Returns AmbiguousEmailError if multiple files match.
func DeleteAccountFileByEmail(email, dir string) error {
	entries, err := readPrivateAccountsDir(dir)
	if err != nil {
		return err
	}
	var matches []string
	for _, entry := range entries {
		if entry.IsDir() || filepath.Ext(entry.Name()) != ".json" {
			continue
		}
		path := filepath.Join(dir, entry.Name())
		data, err := readPrivateAccountFile(path)
		if err != nil {
			return err
		}
		var existing map[string]interface{}
		if err := json.Unmarshal(data, &existing); err != nil {
			continue
		}
		if e, _ := existing["user_email"].(string); strings.EqualFold(e, email) {
			matches = append(matches, path)
		}
	}
	switch len(matches) {
	case 0:
		return fmt.Errorf("account file not found for %s", email)
	case 1:
		if err := os.Remove(matches[0]); err != nil {
			return fmt.Errorf("delete file: %w", err)
		}
		log.Printf("[account] deleted file: %s", filepath.Base(matches[0]))
		return nil
	default:
		return &AmbiguousEmailError{Email: email, Count: len(matches)}
	}
}

// HandleAddAccount accepts a token_v2, discovers account info via Notion APIs,
// saves it to disk, and hot-loads it into the pool.
func HandleAddAccount(pool *AccountPool, accountsDir string, auth *DashboardAuth) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")

		if r.Method != "POST" {
			http.Error(w, `{"error":"method not allowed"}`, http.StatusMethodNotAllowed)
			return
		}

		// Require dashboard session
		if auth.HasAdminPassword() && !auth.ValidateSession(r) {
			http.Error(w, `{"error":"unauthorized, dashboard login required"}`, http.StatusUnauthorized)
			return
		}

		var body struct {
			TokenV2      string `json:"token_v2"`
			NotionUserID string `json:"notion_user_id"`
			SpaceID      string `json:"space_id"`
		}
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			http.Error(w, `{"error":"invalid request body"}`, http.StatusBadRequest)
			return
		}
		tokenV2 := strings.TrimSpace(body.TokenV2)
		if tokenV2 == "" {
			http.Error(w, `{"error":"token_v2 is required"}`, http.StatusBadRequest)
			return
		}

		log.Printf("[add-account] discovering account from token_v2 (%d chars)...", len(tokenV2))

		accounts, err := DiscoverAccountsFromTokenWithOptions(tokenV2, AccountDiscoveryOptions{
			ActiveUserID: strings.TrimSpace(body.NotionUserID),
			SpaceID:      strings.TrimSpace(body.SpaceID),
		})
		if err != nil {
			log.Printf("[add-account] discovery failed: %v", err)
			w.WriteHeader(http.StatusBadRequest)
			json.NewEncoder(w).Encode(map[string]string{
				"error": fmt.Sprintf("Failed to discover account: %v", err),
			})
			return
		}

		best := preferredDiscoveredAccount(accounts, body.SpaceID)
		saved := make([]map[string]string, 0, len(accounts))
		bestFilename := ""
		for _, acc := range accounts {
			filename, saveErr := SaveAccountToFile(acc, accountsDir)
			if saveErr != nil {
				log.Printf("[add-account] save failed: %v", saveErr)
				w.WriteHeader(http.StatusInternalServerError)
				json.NewEncoder(w).Encode(map[string]string{
					"error": fmt.Sprintf("Failed to save account: %v", saveErr),
				})
				return
			}
			pool.AddAccount(acc)
			if best != nil && acc.AccountID == best.AccountID {
				bestFilename = filename
			}
			saved = append(saved, workspacePayload(acc, filename))
			log.Printf("[add-account] success: %s (%s) space=%s → %s", acc.UserName, acc.UserEmail, acc.SpaceName, filename)
		}
		if bestFilename == "" && len(saved) > 0 {
			bestFilename = saved[0]["filename"]
		}

		json.NewEncoder(w).Encode(map[string]interface{}{
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

// HandleListWorkspaces lists the workspaces behind a token without saving them.
func HandleListWorkspaces(auth *DashboardAuth) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if r.Method != "POST" {
			http.Error(w, `{"error":"method not allowed"}`, http.StatusMethodNotAllowed)
			return
		}
		if auth.HasAdminPassword() && !auth.ValidateSession(r) {
			http.Error(w, `{"error":"unauthorized, dashboard login required"}`, http.StatusUnauthorized)
			return
		}
		var body struct {
			TokenV2      string `json:"token_v2"`
			NotionUserID string `json:"notion_user_id"`
		}
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			http.Error(w, `{"error":"invalid request body"}`, http.StatusBadRequest)
			return
		}
		tokenV2 := strings.TrimSpace(body.TokenV2)
		if tokenV2 == "" {
			http.Error(w, `{"error":"token_v2 is required"}`, http.StatusBadRequest)
			return
		}
		accounts, err := DiscoverAccountsFromTokenWithOptions(tokenV2, AccountDiscoveryOptions{
			ActiveUserID: strings.TrimSpace(body.NotionUserID),
		})
		if err != nil {
			w.WriteHeader(http.StatusBadRequest)
			json.NewEncoder(w).Encode(map[string]string{
				"error": fmt.Sprintf("Failed to list workspaces: %v", err),
			})
			return
		}
		workspaces := make([]map[string]string, 0, len(accounts))
		for _, acc := range accounts {
			workspaces = append(workspaces, workspacePayload(acc, ""))
		}
		json.NewEncoder(w).Encode(map[string]interface{}{
			"status":     "ok",
			"workspaces": workspaces,
		})
	}
}

func workspacePayload(acc *Account, filename string) map[string]string {
	if acc == nil {
		return map[string]string{}
	}
	return map[string]string{
		"name":      acc.UserName,
		"email":     acc.UserEmail,
		"space":     acc.SpaceName,
		"space_id":  acc.SpaceID,
		"plan_type": acc.PlanType,
		"filename":  filename,
	}
}

// HandleDeleteAccount removes an account from the pool and deletes its file.
func HandleDeleteAccount(pool *AccountPool, accountsDir string, auth *DashboardAuth) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")

		if r.Method != "POST" {
			http.Error(w, `{"error":"method not allowed"}`, http.StatusMethodNotAllowed)
			return
		}

		// Require dashboard session
		if auth.HasAdminPassword() && !auth.ValidateSession(r) {
			http.Error(w, `{"error":"unauthorized, dashboard login required"}`, http.StatusUnauthorized)
			return
		}

		var body struct {
			Email string `json:"email"`
		}
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			http.Error(w, `{"error":"invalid request body"}`, http.StatusBadRequest)
			return
		}
		email := strings.TrimSpace(body.Email)
		if email == "" {
			http.Error(w, `{"error":"email is required"}`, http.StatusBadRequest)
			return
		}

		// Remove from pool and get account_id for file deletion
		acc := pool.GetByEmail(email)
		if acc == nil {
			w.WriteHeader(http.StatusNotFound)
			json.NewEncoder(w).Encode(map[string]string{"error": "account not found in pool"})
			return
		}
		acc.EnsureAccountID()
		accountID := acc.AccountID
		pool.RemoveAccountByEmail(email)

		// Delete file by account_id
		if accountID != "" {
			if err := DeleteAccountFile(accountID, accountsDir); err != nil {
				log.Printf("[delete-account] file deletion warning: %v", err)
			}
		} else {
			// Legacy fallback: no account_id available
			if err := DeleteAccountFileByEmail(email, accountsDir); err != nil {
				log.Printf("[delete-account] file deletion warning (by email): %v", err)
			}
		}

		log.Printf("[delete-account] removed: %s (account_id=%s)", email, accountID)
		json.NewEncoder(w).Encode(map[string]string{"status": "ok"})
	}
}

func truncate(s string, max int) string {
	if len(s) <= max {
		return s
	}
	return s[:max]
}
