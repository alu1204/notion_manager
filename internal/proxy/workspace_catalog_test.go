package proxy

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func TestCheckUserWorkspaceCountsPointers(t *testing.T) {
	originalClient := workspaceHTTPClient
	originalConfig := AppConfig
	t.Cleanup(func() {
		workspaceHTTPClient = originalClient
		AppConfig = originalConfig
	})
	AppConfig = DefaultConfig()
	body := `{
		"recordMap": {
			"notion_user": {"user-1": {"value":{"value":{"name":"Ada","email":"ada@example.com"}}}},
			"user_root": {"user-1": {"value":{"value":{"space_view_pointers":[
				{"id":"view-a","spaceId":"space-a"},
				{"id":"view-b","spaceId":"space-b"}
			]}}}},
			"space": {
				"space-a": {"value":{"value":{"id":"space-a","name":"A","plan_type":"personal"}}}
			}
		}
	}`
	workspaceHTTPClient = func(time.Duration) *http.Client {
		return &http.Client{Transport: accountDiscoveryRoundTripper(func(req *http.Request) (*http.Response, error) {
			return &http.Response{
				StatusCode: http.StatusOK,
				Header:     make(http.Header),
				Body:       io.NopCloser(strings.NewReader(body)),
				Request:    req,
			}, nil
		})}
	}

	count, err := CheckUserWorkspace(&Account{UserID: "user-1", TokenV2: "token", ClientVersion: DefaultClientVersion})
	if err != nil {
		t.Fatal(err)
	}
	if count != 2 {
		t.Fatalf("pointer count = %d, want 2", count)
	}

	count, err = CheckUserWorkspace(&Account{UserID: "missing-user", TokenV2: "token", ClientVersion: DefaultClientVersion})
	if err != nil {
		t.Fatal(err)
	}
	if count != 2 {
		t.Fatalf("fallback count = %d, want 2", count)
	}
}

func TestDiscoverAccountsFollowsFanoutAndImportsAll(t *testing.T) {
	initial := `{"fanoutData":[{"request":{"cell":"eu"},"headers":{"x-notion-cell":"eu"}}]}`
	fanout := `{
		"users": {
			"target-user": {
				"notion_user": {"target-user": {"value":{"value":{"name":"Target","email":"target@example.com"}}}},
				"user_root": {"target-user": {"value":{"value":{"space_view_pointers":[
					{"spaceId":"space-a","id":"view-a"},
					{"spaceId":"space-b","id":"view-b"}
				]}}}},
				"space": {
					"space-a": {"value":{"value":{"id":"space-a","name":"Alpha","plan_type":"free"}}},
					"space-b": {"value":{"value":{"id":"space-b","name":"Beta","plan_type":"team"}}}
				}
			}
		}
	}`
	sawFanoutHeader := false
	stubDiscoveryByPath(t, func(req *http.Request) (int, string) {
		switch {
		case strings.HasSuffix(req.URL.Path, "/getSpacesInitial"):
			return http.StatusOK, initial
		case strings.HasSuffix(req.URL.Path, "/getSpacesFanout"):
			if req.Header.Get("x-notion-cell") != "eu" {
				t.Errorf("fanout header = %q", req.Header.Get("x-notion-cell"))
			}
			sawFanoutHeader = true
			return http.StatusOK, fanout
		default:
			return http.StatusNotFound, `{}`
		}
	})

	accounts, err := DiscoverAccountsFromTokenWithOptions("secret-token", AccountDiscoveryOptions{ActiveUserID: "target-user"})
	if err != nil {
		t.Fatal(err)
	}
	if !sawFanoutHeader || len(accounts) != 2 {
		t.Fatalf("fanout=%v accounts=%d", sawFanoutHeader, len(accounts))
	}
	best, err := DiscoverAccountFromTokenWithOptions("secret-token", AccountDiscoveryOptions{ActiveUserID: "target-user"})
	if err != nil {
		t.Fatal(err)
	}
	if best.SpaceID != "space-b" || best.SpaceName != "Beta" {
		t.Fatalf("best = %s %s", best.SpaceID, best.SpaceName)
	}
	if best.SpaceCount != 2 || best.WorkspaceCheckedAt == nil {
		t.Fatalf("workspace probe not recorded: count=%d checked=%v", best.SpaceCount, best.WorkspaceCheckedAt)
	}

	pool := NewAccountPool()
	dir := t.TempDir()
	request := httptest.NewRequest(http.MethodPost, "/admin/accounts/add", strings.NewReader(`{"token_v2":"secret-token","notion_user_id":"target-user"}`))
	recorder := httptest.NewRecorder()
	HandleAddAccount(pool, dir, NewDashboardAuth("", "")).ServeHTTP(recorder, request)
	if recorder.Code != http.StatusOK {
		t.Fatalf("add status=%d body=%s", recorder.Code, recorder.Body.String())
	}
	if pool.Count() != 2 {
		t.Fatalf("pool count=%d, want 2", pool.Count())
	}
	var payload struct {
		Workspaces []map[string]string `json:"workspaces"`
		Account    map[string]string   `json:"account"`
	}
	if err := json.Unmarshal(recorder.Body.Bytes(), &payload); err != nil {
		t.Fatal(err)
	}
	if len(payload.Workspaces) != 2 || payload.Account["space"] != "Beta" {
		t.Fatalf("payload = %#v", payload)
	}

	pinned := httptest.NewRequest(http.MethodPost, "/admin/accounts/add", strings.NewReader(
		`{"token_v2":"secret-token","notion_user_id":"target-user","space_id":"space-a"}`,
	))
	pinnedRec := httptest.NewRecorder()
	pinnedPool := NewAccountPool()
	HandleAddAccount(pinnedPool, t.TempDir(), NewDashboardAuth("", "")).ServeHTTP(pinnedRec, pinned)
	if pinnedRec.Code != http.StatusOK || pinnedPool.Count() != 1 {
		t.Fatalf("pinned status=%d count=%d body=%s", pinnedRec.Code, pinnedPool.Count(), pinnedRec.Body.String())
	}
	if acc := pinnedPool.accounts[0]; acc.SpaceID != "space-a" {
		t.Fatalf("pinned space = %s", acc.SpaceID)
	}

	listReq := httptest.NewRequest(http.MethodPost, "/admin/accounts/workspaces", strings.NewReader(`{"token_v2":"secret-token","notion_user_id":"target-user"}`))
	listRec := httptest.NewRecorder()
	HandleListWorkspaces(NewDashboardAuth("", "")).ServeHTTP(listRec, listReq)
	if listRec.Code != http.StatusOK || !strings.Contains(listRec.Body.String(), "space-b") {
		t.Fatalf("list status=%d body=%s", listRec.Code, listRec.Body.String())
	}
	if listPool := NewAccountPool(); listPool.Count() != 0 {
		t.Fatal("list endpoint must not save accounts")
	}
}

func stubDiscoveryByPath(t *testing.T, respond func(*http.Request) (int, string)) {
	t.Helper()
	originalConfig := AppConfig
	originalHTTPClient := discoveryHTTPClient
	originalFetchModels := discoveryFetchModels
	originalCheckQuota := discoveryCheckQuota
	AppConfig = DefaultConfig()
	discoveryHTTPClient = func(time.Duration) *http.Client {
		return &http.Client{Transport: accountDiscoveryRoundTripper(func(req *http.Request) (*http.Response, error) {
			status, body := respond(req)
			return &http.Response{
				StatusCode: status,
				Header:     make(http.Header),
				Body:       io.NopCloser(strings.NewReader(body)),
				Request:    req,
			}, nil
		})}
	}
	discoveryFetchModels = func(*Account) ([]ModelEntry, error) { return nil, nil }
	discoveryCheckQuota = func(*Account) (*QuotaInfo, error) { return nil, nil }
	t.Cleanup(func() {
		AppConfig = originalConfig
		discoveryHTTPClient = originalHTTPClient
		discoveryFetchModels = originalFetchModels
		discoveryCheckQuota = originalCheckQuota
	})
}
