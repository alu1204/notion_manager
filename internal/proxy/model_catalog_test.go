package proxy

import "testing"

func TestResolveModelFuzzyPicksNewestFamilyAlias(t *testing.T) {
	original := SnapshotModelMap()
	t.Cleanup(func() { ReplaceModelMap(original) })
	ReplaceModelMap(map[string]string{
		"opus-4.6":   "avocado-froyo-medium",
		"opus-5":     "agave-flan",
		"sonnet-4.6": "almond-croissant-low",
		"sonnet-5":   "angel-cake-high",
		"haiku-4.5":  "anthropic-haiku-4.5",
	})

	if got := ResolveModel("some-opus-preview"); got != "agave-flan" {
		t.Fatalf("opus fuzzy = %q, want agave-flan", got)
	}
	if got := ResolveModel("claude-sonnet-4-5"); got != "angel-cake-high" {
		t.Fatalf("sonnet alias = %q, want angel-cake-high", got)
	}
	if got := ResolveModel("claude-haiku-3-5"); got != "anthropic-haiku-4.5" {
		t.Fatalf("haiku alias = %q, want anthropic-haiku-4.5", got)
	}
}

func TestRegisterFetchedModelKeepsBuiltinAliases(t *testing.T) {
	original := SnapshotModelMap()
	t.Cleanup(func() { ReplaceModelMap(original) })
	ReplaceModelMap(map[string]string{})

	registerFetchedModel("Opus 5", "agave-flan")
	if got := ResolveModel("opus-5"); got != "agave-flan" {
		t.Fatalf("normalized display = %q", got)
	}
	if got := ResolveModel("claude-opus-5"); got != "agave-flan" {
		t.Fatalf("anthropic alias = %q", got)
	}
	if got, ok := GetModelID("opus-4.6"); ok && got == "agave-flan" {
		t.Fatalf("unrelated alias was overwritten: %q", got)
	}
}

func TestBuiltinModelMapIncludesCurrentFamilies(t *testing.T) {
	for _, name := range []string{"opus-5", "sonnet-5", "gpt-5.6-sol", "gemini-3.5-flash", "grok-4.3"} {
		if _, ok := builtinModelMap()[name]; !ok {
			t.Fatalf("builtin map missing %s", name)
		}
	}
	if DefaultConfig().Proxy.DefaultModel != "opus-5" {
		t.Fatalf("default model = %q", DefaultConfig().Proxy.DefaultModel)
	}
	if DefaultConfig().Proxy.ClientVersion != "23.13.20261016.0235" {
		t.Fatalf("client version = %q", DefaultConfig().Proxy.ClientVersion)
	}
}
