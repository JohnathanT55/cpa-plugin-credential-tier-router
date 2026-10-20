package main

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func codexResponse(five, week *int, swap bool) []byte {
	windows := map[string]any{"primary_window": nil, "secondary_window": nil}
	first, second := "primary_window", "secondary_window"
	if swap {
		first, second = second, first
	}
	for _, entry := range []struct {
		key     string
		value   *int
		seconds int
	}{
		{first, five, 18000}, {second, week, 604800},
	} {
		if entry.value != nil {
			windows[entry.key] = map[string]any{"used_percent": 100 - *entry.value, "limit_window_seconds": entry.seconds, "reset_after_seconds": 3600}
		}
	}
	raw, _ := json.Marshal(map[string]any{"rate_limit": windows})
	return raw
}

func intPtr(value int) *int { return &value }

func TestCodexWindowBands(t *testing.T) {
	for _, test := range []struct {
		name       string
		five, week *int
		want       tierName
	}{
		{"both primary", intPtr(50), intPtr(20), tierPrimary},
		{"short regular", intPtr(49), intPtr(100), tierRegular},
		{"short regular boundary", intPtr(20), intPtr(100), tierRegular},
		{"short backup", intPtr(19), intPtr(100), tierBackup},
		{"weekly regular", intPtr(80), intPtr(19), tierRegular},
		{"weekly regular boundary", intPtr(80), intPtr(5), tierRegular},
		{"weekly backup", intPtr(80), intPtr(4), tierBackup},
		{"short exhausted", intPtr(0), intPtr(80), tierPaused},
		{"week exhausted", intPtr(80), intPtr(0), tierPaused},
		{"weekly only primary", nil, intPtr(20), tierPrimary},
		{"weekly only regular", nil, intPtr(5), tierRegular},
		{"weekly only backup", nil, intPtr(1), tierBackup},
		{"weekly only exhausted", nil, intPtr(0), tierPaused},
	} {
		for _, swap := range []bool{false, true} {
			t.Run(fmt.Sprintf("%s/swapped=%t", test.name, swap), func(t *testing.T) {
				now := time.Now().UTC()
				quota, err := parseCodexQuota(codexResponse(test.five, test.week, swap), now)
				if err != nil {
					t.Fatal(err)
				}
				got, _ := chooseTier(defaultSettings(), authFile{Provider: "codex"}, tierRegular, quota, now)
				if got != test.want {
					t.Fatalf("got %s, want %s", got, test.want)
				}
			})
		}
	}
}

func TestCodexRejectsIncompleteOrAmbiguousWindows(t *testing.T) {
	valid := `{"used_percent":20,"limit_window_seconds":604800,"reset_after_seconds":100}`
	for _, test := range []string{
		`{}`, `{"rate_limit":{}}`,
		`{"rate_limit":{"primary_window":{}}}`,
		`{"rate_limit":{"primary_window":{"used_percent":20,"reset_after_seconds":100}}}`,
		`{"rate_limit":{"primary_window":{"used_percent":20,"limit_window_seconds":123,"reset_after_seconds":100}}}`,
		`{"rate_limit":{"primary_window":{"used_percent":20,"limit_window_seconds":18000},"secondary_window":` + valid + `}}`,
		`{"rate_limit":{"primary_window":"invalid","secondary_window":` + valid + `}}`,
		`{"rate_limit":{"primary_window":` + valid + `,"secondary_window":` + valid + `}}`,
		`{"rate_limit":{"primary_window":{"used_percent":-1,"limit_window_seconds":18000,"reset_after_seconds":100}}}`,
	} {
		if _, err := parseCodexQuota([]byte(test), time.Now()); err == nil {
			t.Fatalf("accepted %s", test)
		}
	}
}

func TestCodexCustomBandsAndOtherPolicies(t *testing.T) {
	now := time.Now().UTC()
	quota, err := parseCodexQuota(codexResponse(intPtr(60), intPtr(30), false), now)
	if err != nil {
		t.Fatal(err)
	}
	cfg := defaultSettings()
	cfg.CodexWeeklyPrimaryMin = 40
	got, reason := chooseTier(cfg, authFile{Provider: "codex"}, tierBackup, quota, now)
	if got != tierRegular || !strings.Contains(reason, "周剩余 30%") {
		t.Fatalf("custom bands: %s, %s", got, reason)
	}
	// Antigravity continues to use the original thresholds.
	got, _ = chooseTier(cfg, authFile{Provider: "antigravity"}, tierBackup, readyQuota(30, nil, now), now)
	if got != tierRegular {
		t.Fatalf("Antigravity got %s", got)
	}
	for strategy, want := range map[strategyName]tierName{strategyRotate: tierRegular, strategyReset: tierPrimary, strategyManual: tierBackup} {
		cfg.Strategy = strategy
		cfg.ManualTiers["a"] = tierBackup
		got, _ = chooseTier(cfg, authFile{Provider: "codex", AuthIndex: "a"}, tierRegular, quota, now)
		if got != want {
			t.Fatalf("%s got %s, want %s", strategy, got, want)
		}
	}
}

func TestCodexWindowLossRemainsFailureAcrossThreshold(t *testing.T) {
	now := time.Now()
	previous, _ := parseCodexQuota(codexResponse(intPtr(10), intPtr(80), false), now)
	weekly, _ := parseCodexQuota(codexResponse(nil, intPtr(80), false), now)
	for i := 1; i <= 4; i++ {
		err := checkCodexWindowContinuity(previous, weekly)
		if err == nil {
			t.Fatal("disappearing 5h window accepted")
		}
		previous = failedQuota(previous, err, 3, now)
		got, _ := chooseTier(defaultSettings(), authFile{Provider: "codex"}, tierBackup, previous, now)
		if got != tierBackup {
			t.Fatalf("failure %d promoted account: %s", i, got)
		}
	}
	if previous.Status != quotaUnknown || previous.Remaining != nil {
		t.Fatalf("bad failure state: %+v", previous)
	}
	restored, _ := parseCodexQuota(codexResponse(intPtr(80), intPtr(80), false), now)
	if err := checkCodexWindowContinuity(previous, restored); err != nil {
		t.Fatal(err)
	}
	got, _ := chooseTier(defaultSettings(), authFile{Provider: "codex"}, tierPaused, restored, now)
	if got != tierPrimary {
		t.Fatalf("quota reset did not restore tier: %s", got)
	}
}

func TestCodexLegacyCacheDoesNotInventWindows(t *testing.T) {
	now := time.Now()
	quota := failedQuota(readyQuota(30, nil, now), fmt.Errorf("offline"), 3, now)
	got, _ := chooseTier(defaultSettings(), authFile{Provider: "codex"}, tierBackup, quota, now)
	if got != tierBackup {
		t.Fatalf("legacy cache changed tier: %s", got)
	}
}

func TestCodexSettingsValidationAndMigration(t *testing.T) {
	for _, raw := range []string{"", `{"strategy":"quota_bands"}`, "strategy: quota_bands\n"} {
		cfg, err := parsePluginConfig([]byte(raw))
		if err != nil || cfg.Codex5hPrimaryMin != 50 || cfg.Codex5hRegularMin != 20 || cfg.CodexWeeklyPrimaryMin != 20 || cfg.CodexWeeklyRegularMin != 5 {
			t.Fatalf("defaults: %+v %v", cfg, err)
		}
	}
	for _, raw := range []string{`{"codex_weekly_primary_min":40,"codex_weekly_regular_min":10}`, "codex_weekly_primary_min: 40\ncodex_weekly_regular_min: 10"} {
		cfg, err := parsePluginConfig([]byte(raw))
		if err != nil || cfg.CodexWeeklyPrimaryMin != 40 || cfg.CodexWeeklyRegularMin != 10 {
			t.Fatalf("custom: %+v %v", cfg, err)
		}
	}
	for _, raw := range []string{`{"codex_weekly_regular_min":0}`, `{"codex_5h_primary_min":20}`, `{"codex_5h_primary_min":101}`, `{"codex_weekly_primary_min":20.5}`, "codex_weekly_regular_min: invalid"} {
		if _, err := parsePluginConfig([]byte(raw)); err == nil {
			t.Fatalf("accepted invalid settings: %s", raw)
		}
	}
	store := stateStore{path: filepath.Join(t.TempDir(), "state.json")}
	// Mimic an old persisted state without the four new settings.
	cfgRaw, _ := json.Marshal(defaultSettings())
	var old map[string]any
	_ = json.Unmarshal(cfgRaw, &old)
	for key := range old {
		if strings.HasPrefix(key, "codex_") {
			delete(old, key)
		}
	}
	raw, _ := json.Marshal(map[string]any{"settings": old})
	if err := os.WriteFile(store.path, raw, 0600); err != nil {
		t.Fatal(err)
	}
	state, err := store.load()
	if err != nil || state.Settings.CodexWeeklyRegularMin != 5 {
		t.Fatalf("migration: %+v %v", state, err)
	}
}

func TestPreviewDraftDoesNotSaveSettingsOrCredentials(t *testing.T) {
	host := &fakeHost{
		files:     []authFile{{Name: "a.json", AuthIndex: "a", Provider: "codex", Priority: 400}},
		documents: map[string]authDocument{"a": {Name: "a.json", JSON: json.RawMessage(`{"access_token":"test"}`)}},
		responses: map[string]hostHTTPResponse{"a": {StatusCode: 200, Body: codexResponse(intPtr(80), intPtr(30), false)}},
	}
	r := newRuntime(host)
	r.store.path = filepath.Join(t.TempDir(), "state.json")
	draft := defaultSettings()
	draft.AutoApply = true
	draft.CodexWeeklyPrimaryMin = 40
	body, _ := json.Marshal(draft)
	request, _ := json.Marshal(map[string]any{"Method": "POST", "Path": "/plugins/credential-tier-router/preview", "Body": body})
	response, err := r.handleManagement(context.Background(), request)
	if err != nil || response.StatusCode != http.StatusOK {
		t.Fatalf("preview: %+v %v", response, err)
	}
	var result plan
	if err := json.Unmarshal(response.Body, &result); err != nil {
		t.Fatal(err)
	}
	if len(result.Credentials) != 1 || result.Credentials[0].ProposedTier != tierRegular {
		t.Fatalf("draft not used: %+v", result)
	}
	state, err := r.store.load()
	if err != nil || state.Settings.AutoApply || state.Settings.CodexWeeklyPrimaryMin != 20 || len(host.saved) != 0 || r.cancel != nil {
		t.Fatalf("preview mutated live settings: %+v %v", state, err)
	}
	if len(r.latest.Credentials) != 0 {
		t.Fatal("draft tiers leaked into the saved-settings dashboard")
	}
	// Saving old-client settings still supplies default thresholds.
	request, _ = json.Marshal(map[string]any{"Method": "PUT", "Path": "/plugins/credential-tier-router/settings", "Body": []byte(`{"strategy":"quota_bands","auto_apply":false}`)})
	response, err = r.handleManagement(context.Background(), request)
	defer r.shutdown()
	if err != nil || response.StatusCode != http.StatusOK || r.state.Settings.CodexWeeklyPrimaryMin != 20 {
		t.Fatalf("old settings client: %+v %v", response, err)
	}
}
