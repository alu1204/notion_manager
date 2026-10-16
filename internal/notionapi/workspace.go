package notionapi

import (
	"encoding/json"
	"sort"
	"strings"
)

// Workspace is one Notion space the account can open.
type Workspace struct {
	SpaceID     string
	Name        string
	PlanType    string
	SpaceViewID string
	AIEnabled   bool
}

// User is one Notion user inside a getSpaces / loadUserContent payload,
// together with the spaces that user can actually open.
type User struct {
	ID                   string
	Name                 string
	Email                string
	Timezone             string
	Workspaces           []Workspace
	LegacySpaceViewCount int
}

// FanoutRequest is one cell returned by getSpacesInitial. The current
// client posts each request to getSpacesFanout.
type FanoutRequest struct {
	Request json.RawMessage   `json:"request"`
	Headers map[string]string `json:"headers"`
}

// Catalog is the normalized workspace listing.
type Catalog struct {
	Users  []User
	Fanout []FanoutRequest
}

// ParseWorkspaceCatalog accepts the response shapes Notion has used for
// workspace discovery:
//   - loadUserContent: {"recordMap":{notion_user,user_root,space,user_settings}}
//   - getSpaces: {"<userId>":{notion_user,user_root,space}}
//   - getSpaces fanout: {"users":{"<userId>":{...}}, "fanoutData":[...]}
//   - getSpacesInitial when it only carries fanoutData
//
// Records are unwrapped through the v3 value/role nesting. A user with
// space_view_pointers but no embedded space record is still listed; callers
// can fill names later with getPublicSpaceData.
func ParseWorkspaceCatalog(body []byte) (Catalog, error) {
	body = trimSpace(body)
	if len(body) == 0 || body[0] != '{' {
		return Catalog{}, nil
	}
	var root map[string]json.RawMessage
	if err := json.Unmarshal(body, &root); err != nil {
		return Catalog{}, err
	}
	cat := Catalog{Fanout: fanoutFrom(root["fanoutData"])}
	if raw, ok := root["recordMap"]; ok {
		cat.Users = append(cat.Users, usersFromTables(decodeMap(raw))...)
		return cat, nil
	}
	if raw, ok := root["users"]; ok {
		var users map[string]json.RawMessage
		if json.Unmarshal(raw, &users) == nil {
			for _, recordMap := range users {
				cat.Users = append(cat.Users, usersFromTables(decodeMap(recordMap))...)
			}
		}
		return cat, nil
	}
	for key, raw := range root {
		if key == "fanoutData" || key == "spaceInaccessiblePages" || key == "recordMap" || key == "users" {
			continue
		}
		tables := decodeMap(raw)
		if tables == nil {
			continue
		}
		if _, ok := tables["user_root"]; !ok {
			if _, ok := tables["notion_user"]; !ok {
				continue
			}
		}
		cat.Users = append(cat.Users, usersFromTables(tables)...)
	}
	return cat, nil
}

// MergeCatalogs combines fanout cells. Later cells fill in empty profile
// fields and add spaces that were not present yet.
func MergeCatalogs(parts ...Catalog) Catalog {
	merged := Catalog{}
	index := map[string]int{}
	for _, part := range parts {
		merged.Fanout = append(merged.Fanout, part.Fanout...)
		for _, user := range part.Users {
			pos, ok := index[user.ID]
			if !ok || user.ID == "" {
				if user.ID != "" {
					index[user.ID] = len(merged.Users)
				}
				merged.Users = append(merged.Users, user)
				continue
			}
			dst := &merged.Users[pos]
			if dst.Name == "" {
				dst.Name = user.Name
			}
			if dst.Email == "" {
				dst.Email = user.Email
			}
			if dst.Timezone == "" {
				dst.Timezone = user.Timezone
			}
			if user.LegacySpaceViewCount > dst.LegacySpaceViewCount {
				dst.LegacySpaceViewCount = user.LegacySpaceViewCount
			}
			seen := map[string]bool{}
			for _, ws := range dst.Workspaces {
				seen[ws.SpaceID] = true
			}
			for _, ws := range user.Workspaces {
				if ws.SpaceID == "" || seen[ws.SpaceID] {
					continue
				}
				dst.Workspaces = append(dst.Workspaces, ws)
				seen[ws.SpaceID] = true
			}
		}
	}
	return merged
}

// CountForUser is strict: a missing user counts as zero. Used while polling
// onboarding, where another account in the payload must not look like success.
func (c Catalog) CountForUser(userID string) int {
	user, ok := c.find(userID)
	if !ok {
		return 0
	}
	return user.count()
}

// ProbeCount is the pool probe. When the account's user id is present, only
// that user's spaces count. When the id is absent (Notion re-keyed the
// record), any non-empty user keeps a healthy account from being blacklisted.
func (c Catalog) ProbeCount(userID string) int {
	if user, ok := c.find(userID); ok {
		return user.count()
	}
	best := 0
	for _, user := range c.Users {
		if n := user.count(); n > best {
			best = n
		}
	}
	return best
}

func (u User) count() int {
	if n := len(u.Workspaces); n > 0 {
		return n
	}
	return u.LegacySpaceViewCount
}

func (c Catalog) find(userID string) (User, bool) {
	if userID == "" {
		return User{}, false
	}
	for _, user := range c.Users {
		if user.ID == userID {
			return user, true
		}
	}
	return User{}, false
}

// Select returns the workspace the caller asked for, or the best AI-capable
// non-free space. ok is false when a requested space is not in the catalog.
func Select(user User, requestedSpaceID string) (Workspace, bool) {
	requestedSpaceID = strings.TrimSpace(requestedSpaceID)
	if requestedSpaceID != "" {
		for _, ws := range user.Workspaces {
			if ws.SpaceID == requestedSpaceID {
				return ws, true
			}
		}
		return Workspace{}, false
	}
	if len(user.Workspaces) == 0 {
		return Workspace{}, false
	}
	best := user.Workspaces[0]
	bestRank := workspaceRank(best)
	for _, ws := range user.Workspaces[1:] {
		if rank := workspaceRank(ws); rank > bestRank {
			best = ws
			bestRank = rank
		}
	}
	return best, true
}

func workspaceRank(ws Workspace) int {
	rank := 0
	if ws.AIEnabled {
		rank += 2
	}
	if !strings.EqualFold(ws.PlanType, "free") && ws.PlanType != "" {
		rank += 1
	}
	return rank
}

func fanoutFrom(raw json.RawMessage) []FanoutRequest {
	if len(bytesTrim(raw)) == 0 {
		return nil
	}
	var items []FanoutRequest
	if json.Unmarshal(raw, &items) != nil {
		return nil
	}
	return items
}

func decodeMap(raw json.RawMessage) map[string]json.RawMessage {
	raw = bytesTrim(raw)
	if len(raw) == 0 || raw[0] != '{' {
		return nil
	}
	var tables map[string]json.RawMessage
	if json.Unmarshal(raw, &tables) != nil {
		return nil
	}
	if inner, ok := tables["recordMap"]; ok {
		if nested := decodeMap(inner); nested != nil {
			return nested
		}
	}
	return tables
}

func usersFromTables(tables map[string]json.RawMessage) []User {
	if len(tables) == 0 {
		return nil
	}
	users := map[string]*User{}
	order := []string{}
	ensure := func(id string) *User {
		if id == "" {
			return nil
		}
		if user, ok := users[id]; ok {
			return user
		}
		user := &User{ID: id}
		users[id] = user
		order = append(order, id)
		return user
	}

	spaces := map[string]map[string]interface{}{}
	if raw, ok := tables["space"]; ok {
		for id, rec := range decodeMap(raw) {
			data := UnwrapRecord(rec)
			if data == nil {
				continue
			}
			if StringField(data, "id") == "" {
				data["id"] = id
			}
			spaces[id] = data
		}
	}

	if raw, ok := tables["notion_user"]; ok {
		for id, rec := range decodeMap(raw) {
			data := UnwrapRecord(rec)
			user := ensure(id)
			if user == nil || data == nil {
				continue
			}
			user.Name = StringField(data, "name")
			user.Email = StringField(data, "email")
		}
	}
	if raw, ok := tables["user_settings"]; ok {
		for id, rec := range decodeMap(raw) {
			data := UnwrapRecord(rec)
			user := ensure(id)
			if user == nil || data == nil {
				continue
			}
			settings, _ := data["settings"].(map[string]interface{})
			if tz := StringField(settings, "time_zone", "timeZone"); tz != "" {
				user.Timezone = tz
			}
		}
	}
	if raw, ok := tables["user_root"]; ok {
		for id, rec := range decodeMap(raw) {
			data := UnwrapRecord(rec)
			user := ensure(id)
			if user == nil || data == nil {
				continue
			}
			user.LegacySpaceViewCount = len(stringList(data["space_views"]))
			seen := map[string]bool{}
			for _, ptr := range spacePointers(data["space_view_pointers"]) {
				if ptr.SpaceID == "" || seen[ptr.SpaceID] {
					continue
				}
				seen[ptr.SpaceID] = true
				user.Workspaces = append(user.Workspaces, workspaceFromSpace(ptr, spaces[ptr.SpaceID]))
			}
		}
	}

	out := make([]User, 0, len(order))
	for _, id := range order {
		out = append(out, *users[id])
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	return out
}

type spacePointer struct {
	ID      string
	SpaceID string
}

func spacePointers(raw interface{}) []spacePointer {
	items, ok := raw.([]interface{})
	if !ok {
		return nil
	}
	var pointers []spacePointer
	for _, item := range items {
		obj, ok := item.(map[string]interface{})
		if !ok {
			continue
		}
		pointers = append(pointers, spacePointer{
			ID:      StringField(obj, "id"),
			SpaceID: StringField(obj, "spaceId", "space_id"),
		})
	}
	return pointers
}

func stringList(raw interface{}) []string {
	items, ok := raw.([]interface{})
	if !ok {
		return nil
	}
	var out []string
	for _, item := range items {
		if s, ok := item.(string); ok && s != "" {
			out = append(out, s)
		}
	}
	return out
}

func workspaceFromSpace(ptr spacePointer, data map[string]interface{}) Workspace {
	ws := Workspace{
		SpaceID:     ptr.SpaceID,
		SpaceViewID: ptr.ID,
		AIEnabled:   true,
	}
	if data == nil {
		return ws
	}
	if id := StringField(data, "id"); id != "" {
		ws.SpaceID = id
	}
	ws.Name = StringField(data, "name")
	ws.PlanType = StringField(data, "plan_type", "planType")
	settings, _ := data["settings"].(map[string]interface{})
	if enabled, ok := BoolField(settings, "enable_ai_feature"); ok && !enabled {
		ws.AIEnabled = false
	}
	if disabled, ok := BoolField(settings, "disable_ai_feature"); ok && disabled {
		ws.AIEnabled = false
	}
	return ws
}

// PublicSpace is one result from getPublicSpaceData, used when a guest
// pointer has no embedded space record.
type PublicSpace struct {
	ID       string
	Name     string
	PlanType string
}

// ParsePublicSpaces reads getPublicSpaceData results.
func ParsePublicSpaces(body []byte) []PublicSpace {
	body = trimSpace(body)
	if len(body) == 0 || body[0] != '{' {
		return nil
	}
	var root struct {
		Results []map[string]interface{} `json:"results"`
	}
	if json.Unmarshal(body, &root) != nil {
		return nil
	}
	var spaces []PublicSpace
	for _, item := range root.Results {
		id := StringField(item, "id")
		if id == "" {
			continue
		}
		spaces = append(spaces, PublicSpace{
			ID:       id,
			Name:     StringField(item, "name"),
			PlanType: StringField(item, "plan_type", "planType"),
		})
	}
	return spaces
}

// ApplyPublicSpaces fills blank workspace names from getPublicSpaceData.
func (c *Catalog) ApplyPublicSpaces(spaces []PublicSpace) {
	byID := map[string]PublicSpace{}
	for _, space := range spaces {
		byID[space.ID] = space
	}
	for i := range c.Users {
		for j := range c.Users[i].Workspaces {
			ws := &c.Users[i].Workspaces[j]
			pub, ok := byID[ws.SpaceID]
			if !ok {
				continue
			}
			if ws.Name == "" {
				ws.Name = pub.Name
			}
			if ws.PlanType == "" {
				ws.PlanType = pub.PlanType
			}
		}
	}
}
