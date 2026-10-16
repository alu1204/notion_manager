package notionapi

import (
	"bytes"
	"encoding/json"
)

// Poster issues one Notion /api/v3 call. headers are added on top of the
// caller's normal Notion headers; a nil map is fine.
type Poster func(endpoint string, body []byte, headers map[string]string) (status int, response []byte, err error)

// FetchCatalog loads the current workspace list.
//
// Notion's web client no longer treats loadUserContent as the remote source
// of truth. It calls getSpacesInitial, then posts each fanoutData item to
// getSpacesFanout. The older single-body getSpaces and loadUserContent
// responses are still accepted so an unchanged cell keeps working.
func FetchCatalog(post Poster) (Catalog, error) {
	var lastErr error
	for _, endpoint := range []string{"getSpacesInitial", "getSpaces", "loadUserContent"} {
		status, raw, err := post(endpoint, []byte("{}"), nil)
		if err != nil {
			lastErr = err
			continue
		}
		if status != 200 {
			continue
		}
		cat, err := ParseWorkspaceCatalog(raw)
		if err != nil {
			lastErr = err
			continue
		}
		cat = followFanout(post, cat)
		if len(cat.Users) == 0 {
			continue
		}
		fillPublicSpaces(post, &cat)
		return cat, nil
	}
	if lastErr != nil {
		return Catalog{}, lastErr
	}
	return Catalog{}, nil
}

func followFanout(post Poster, cat Catalog) Catalog {
	pending := cat.Fanout
	cat.Fanout = nil
	for depth := 0; depth < 3 && len(pending) > 0; depth++ {
		parts := []Catalog{cat}
		var next []FanoutRequest
		limit := len(pending)
		if limit > 20 {
			limit = 20
		}
		for _, item := range pending[:limit] {
			status, raw, err := post("getSpacesFanout", fanoutBody(item.Request), item.Headers)
			if err != nil || status != 200 {
				continue
			}
			more, err := ParseWorkspaceCatalog(raw)
			if err != nil {
				continue
			}
			next = append(next, more.Fanout...)
			more.Fanout = nil
			parts = append(parts, more)
		}
		cat = MergeCatalogs(parts...)
		cat.Fanout = nil
		pending = next
	}
	return cat
}

func fanoutBody(raw []byte) []byte {
	raw = bytes.TrimSpace(raw)
	if len(raw) == 0 || string(raw) == "null" {
		return []byte("{}")
	}
	return raw
}

func fillPublicSpaces(post Poster, cat *Catalog) {
	var missing []string
	seen := map[string]bool{}
	for _, user := range cat.Users {
		for _, ws := range user.Workspaces {
			if ws.SpaceID == "" || ws.Name != "" || seen[ws.SpaceID] {
				continue
			}
			seen[ws.SpaceID] = true
			missing = append(missing, ws.SpaceID)
		}
	}
	if len(missing) == 0 {
		return
	}
	body, err := json.Marshal(map[string]interface{}{
		"type":     "space-ids",
		"spaceIds": missing,
	})
	if err != nil {
		return
	}
	status, raw, err := post("getPublicSpaceData", body, nil)
	if err != nil || status != 200 {
		return
	}
	cat.ApplyPublicSpaces(ParsePublicSpaces(raw))
}
