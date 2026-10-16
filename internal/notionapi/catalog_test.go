package notionapi

import (
	"encoding/json"
	"errors"
	"strings"
	"testing"
)

func TestParseAvailableModelsAcceptsCurrentAndLegacyShapes(t *testing.T) {
	body := []byte(`{
		"models": [
			{"model":"agave-flan","modelMessage":"Opus 5","modelFamily":"anthropic","isDisabled":false},
			{"id":"angel-cake-high","displayName":"Sonnet 5","disabled":false},
			{"model":"old-model","modelMessage":"Hidden","isDisabled":true}
		]
	}`)
	got, err := ParseAvailableModels(body)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 || got[0].ID != "agave-flan" || got[0].Name != "Opus 5" || got[1].ID != "angel-cake-high" || got[1].Name != "Sonnet 5" {
		t.Fatalf("models = %#v", got)
	}

	wrapped := []byte(`{"data":{"models":[{"modelId":"orange-mousse","name":"GPT-5.6 Sol"}]}}`)
	got, err = ParseAvailableModels(wrapped)
	if err != nil || len(got) != 1 || got[0].ID != "orange-mousse" || got[0].Name != "GPT-5.6 Sol" {
		t.Fatalf("wrapped models = %#v err=%v", got, err)
	}
}

func TestParseWorkspaceCatalogNestedPointersAndFanout(t *testing.T) {
	body := []byte(`{
		"fanoutData":[{"request":{"cell":"eu"},"headers":{"x-notion-cell":"eu"}}],
		"users":{
			"user-1":{
				"notion_user":{"user-1":{"value":{"value":{"name":"Ada","email":"ada@example.com"},"role":"editor"}}},
				"user_root":{"user-1":{"value":{"value":{"space_view_pointers":[
					{"id":"view-guest","spaceId":"space-guest","table":"space_view"},
					{"id":"view-team","spaceId":"space-team","table":"space_view"}
				]},"role":"editor"}}},
				"space":{"space-team":{"value":{"value":{"id":"space-team","name":"Team","plan_type":"team","settings":{"disable_ai_feature":false}},"role":"editor"}}},
				"user_settings":{"user-1":{"value":{"value":{"settings":{"time_zone":"Asia/Shanghai"}}}}}
			}
		}
	}`)
	cat, err := ParseWorkspaceCatalog(body)
	if err != nil {
		t.Fatal(err)
	}
	if len(cat.Fanout) != 1 || len(cat.Users) != 1 {
		t.Fatalf("catalog = %#v", cat)
	}
	user := cat.Users[0]
	if user.Email != "ada@example.com" || user.Timezone != "Asia/Shanghai" || len(user.Workspaces) != 2 {
		t.Fatalf("user = %#v", user)
	}
	if user.Workspaces[0].Name != "" || user.Workspaces[0].SpaceID != "space-guest" {
		t.Fatalf("guest workspace = %#v", user.Workspaces[0])
	}
	if user.Workspaces[1].Name != "Team" || !user.Workspaces[1].AIEnabled {
		t.Fatalf("team workspace = %#v", user.Workspaces[1])
	}
	best, ok := Select(user, "")
	if !ok || best.SpaceID != "space-team" {
		t.Fatalf("best = %#v ok=%v", best, ok)
	}
	if cat.CountForUser("missing") != 0 || cat.ProbeCount("missing") != 2 {
		t.Fatalf("counts strict=%d probe=%d", cat.CountForUser("missing"), cat.ProbeCount("missing"))
	}
}

func TestParseWorkspaceCatalogLegacyLoadUserContent(t *testing.T) {
	body := []byte(`{
		"recordMap":{
			"notion_user":{"user-2":{"value":{"name":"Bea","email":"bea@example.com"}}},
			"user_root":{"user-2":{"value":{"space_view_pointers":[{"id":"sv","spaceId":"space-2"}]}}},
			"space":{"space-2":{"value":{"id":"space-2","name":"Personal","plan_type":"personal"}}}
		}
	}`)
	cat, err := ParseWorkspaceCatalog(body)
	if err != nil {
		t.Fatal(err)
	}
	if cat.CountForUser("user-2") != 1 || cat.Users[0].Workspaces[0].Name != "Personal" {
		t.Fatalf("catalog = %#v", cat)
	}
	if cat.CountForUser("user-2") == 0 {
		t.Fatal("flat value wrapper was dropped")
	}
}

func TestFetchCatalogFollowsFanoutAndPublicSpace(t *testing.T) {
	calls := []string{}
	post := func(endpoint string, body []byte, headers map[string]string) (int, []byte, error) {
		calls = append(calls, endpoint)
		switch endpoint {
		case "getSpacesInitial":
			return 200, []byte(`{"fanoutData":[{"request":{"region":"usw"},"headers":{"x-notion-cell":"usw"}}]}`), nil
		case "getSpacesFanout":
			if headers["x-notion-cell"] != "usw" || !strings.Contains(string(body), "usw") {
				t.Fatalf("fanout request body=%s headers=%v", body, headers)
			}
			return 200, []byte(`{"users":{"user-1":{
				"notion_user":{"user-1":{"value":{"value":{"name":"Ada","email":"ada@example.com"}}}},
				"user_root":{"user-1":{"value":{"value":{"space_view_pointers":[{"id":"sv","spaceId":"space-guest"}]}}}}
			}}}`), nil
		case "getPublicSpaceData":
			var req struct {
				SpaceIDs []string `json:"spaceIds"`
			}
			if err := json.Unmarshal(body, &req); err != nil || len(req.SpaceIDs) != 1 || req.SpaceIDs[0] != "space-guest" {
				t.Fatalf("public space body = %s", body)
			}
			return 200, []byte(`{"results":[{"id":"space-guest","name":"Guest","planType":"personal"}]}`), nil
		default:
			return 0, nil, errors.New("unexpected " + endpoint)
		}
	}
	cat, err := FetchCatalog(post)
	if err != nil {
		t.Fatal(err)
	}
	if len(cat.Users) != 1 || cat.Users[0].Workspaces[0].Name != "Guest" || cat.Users[0].Workspaces[0].PlanType != "personal" {
		t.Fatalf("catalog = %#v", cat.Users)
	}
	if strings.Join(calls, ",") != "getSpacesInitial,getSpacesFanout,getPublicSpaceData" {
		t.Fatalf("calls = %v", calls)
	}
}

func TestFetchCatalogFallsBackToLoadUserContent(t *testing.T) {
	post := func(endpoint string, body []byte, headers map[string]string) (int, []byte, error) {
		switch endpoint {
		case "getSpacesInitial", "getSpaces":
			return 404, []byte(`{"error":"gone"}`), nil
		case "loadUserContent":
			return 200, []byte(`{"recordMap":{
				"notion_user":{"u":{"value":{"value":{"name":"Cy","email":"cy@example.com"}}}},
				"user_root":{"u":{"value":{"value":{"space_views":["sv-only"]}}}}
			}}`), nil
		default:
			t.Fatalf("unexpected %s", endpoint)
			return 0, nil, nil
		}
	}
	cat, err := FetchCatalog(post)
	if err != nil {
		t.Fatal(err)
	}
	if cat.CountForUser("u") != 1 {
		t.Fatalf("legacy space_views not counted: %#v", cat.Users)
	}
}
