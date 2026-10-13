package reader

import (
	"encoding/json"
	"strings"
	"testing"
)

func TestCollectionCitationSecretProtectedOwner(t *testing.T) {
	s := testServer(t)
	put := func(extra map[string]any) {
		t.Helper()
		response := request(t, s, "GET", "/api/ai/config", nil)
		var body map[string]any
		if response.Code != 200 || json.Unmarshal(response.Body.Bytes(), &body) != nil {
			t.Fatal(response.Body.String())
		}
		for key, value := range extra {
			body[key] = value
		}
		response = request(t, s, "PUT", "/api/ai/config", strings.NewReader(mustJSONText(body)))
		if response.Code != 200 {
			t.Fatalf("protected owner: %d %s", response.Code, response.Body.String())
		}
		if strings.Contains(response.Body.String(), "citation-fixture-secret") {
			t.Fatal("secret exposed by update")
		}
	}
	put(map[string]any{"citationsApiKey": "citation-fixture-secret"})
	if s.Collection().citationKey() != "citation-fixture-secret" {
		t.Fatal("citation transport did not receive protected key")
	}
	put(map[string]any{"primary": "codex"})
	if s.Collection().citationKey() != "citation-fixture-secret" {
		t.Fatal("ordinary AI edit erased citation key")
	}
	for _, path := range []string{"/api/ai/config", "/api/collection/settings"} {
		response := request(t, s, "GET", path, nil)
		var body map[string]any
		if response.Code != 200 || json.Unmarshal(response.Body.Bytes(), &body) != nil || body["hasCitationsKey"] != true || strings.Contains(response.Body.String(), "citation-fixture-secret") {
			t.Fatalf("public secret marker: %s %s", path, response.Body.String())
		}
	}
	cfg, err := s.Store.CollectionSettings()
	if err != nil {
		t.Fatal(err)
	}
	if response := request(t, s, "PUT", "/api/collection/settings", strings.NewReader(mustJSONText(cfg))); response.Code != 200 {
		t.Fatal(response.Body.String())
	}
	var stored string
	if err := s.Store.DB.QueryRow("SELECT body FROM collection_settings").Scan(&stored); err != nil || strings.Contains(stored, "citation-fixture-secret") {
		t.Fatalf("secret leaked to SQLite: %v", err)
	}
	put(map[string]any{"clearCitationsKey": true})
	if s.Collection().citationKey() != "" {
		t.Fatal("explicit clear retained secret")
	}
	response := request(t, s, "GET", "/api/ai/config", nil)
	if strings.Contains(response.Body.String(), `"hasCitationsKey":true`) || strings.Contains(response.Body.String(), `"clearCitationsKey":true`) {
		t.Fatal("clear command persisted as config")
	}
}

func TestCollectionSettingsQuotaIsDerivedAndReadOnly(t *testing.T) {
	s := testServer(t)
	day := utcDay(s.Collection().clockNow())
	if _, err := s.Store.DB.Exec("INSERT INTO collection_quota_days(utc_day,sent,reserved,uncertain) VALUES(?,3,1,1)", day); err != nil {
		t.Fatal(err)
	}
	response := request(t, s, "GET", "/api/collection/settings", nil)
	var body map[string]any
	if response.Code != 200 || json.Unmarshal(response.Body.Bytes(), &body) != nil {
		t.Fatal(response.Body.String())
	}
	quota, ok := body["quota"].(map[string]any)
	if !ok || quota["utcDay"] != day || quota["sent"] != float64(3) || quota["reserved"] != float64(1) || quota["uncertain"] != float64(1) {
		t.Fatalf("quota not derived: %s", response.Body.String())
	}
	body["quota"] = map[string]any{"utcDay": day, "sent": 0, "reserved": 0, "uncertain": 0}
	body["hasCitationsKey"] = true
	response = request(t, s, "PUT", "/api/collection/settings", strings.NewReader(mustJSONText(body)))
	if response.Code != 200 || json.Unmarshal(response.Body.Bytes(), &body) != nil {
		t.Fatal(response.Body.String())
	}
	quota, ok = body["quota"].(map[string]any)
	if !ok || quota["sent"] != float64(3) || body["hasCitationsKey"] != false {
		t.Fatalf("caller rewrote derived facts: %s", response.Body.String())
	}
	var stored string
	if err := s.Store.DB.QueryRow("SELECT body FROM collection_settings").Scan(&stored); err != nil || strings.Contains(stored, `"quota"`) || strings.Contains(stored, `"hasCitationsKey":true`) {
		t.Fatalf("derived facts persisted: %s %v", stored, err)
	}
}
