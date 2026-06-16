package main

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"gopkg.in/yaml.v3"
)

func TestNormalizeConfigDefaults(t *testing.T) {
	cfg := NormalizeConfig(AppConfig{})

	if cfg.MixedPort != 7890 || cfg.HTTPPort != 7891 || cfg.SOCKS5Port != 7892 {
		t.Fatalf("unexpected default ports: %+v", cfg)
	}
	if cfg.UpdateIntervalMinutes != 60 {
		t.Fatalf("unexpected default interval: %d", cfg.UpdateIntervalMinutes)
	}
	if cfg.ManagementPort != 9090 {
		t.Fatalf("unexpected management port: %d", cfg.ManagementPort)
	}
	if cfg.Mode != "rule" {
		t.Fatalf("unexpected mode: %q", cfg.Mode)
	}
	if cfg.LogLevel != "info" {
		t.Fatalf("unexpected log level: %q", cfg.LogLevel)
	}
	if cfg.GeoUpdateInterval != 24 {
		t.Fatalf("unexpected geo update interval: %d", cfg.GeoUpdateInterval)
	}
	if !cfg.IPv6 {
		t.Fatalf("ipv6 should default to true")
	}
	if cfg.LastUpdatedAt != "" {
		t.Fatalf("unexpected last update time: %q", cfg.LastUpdatedAt)
	}
}

func TestRenderRuntimeProfileOverridesManagedFields(t *testing.T) {
	profile := []byte("mixed-port: 1111\nport: 2222\nsocks-port: 3333\nmode: direct\nlog-level: debug\ngeo-update-interval: 6\nipv6: true\nexternal-controller-tls: 0.0.0.0:9443\nexternal-ui: ui\nexternal-ui-url: https://example.com/ui.zip\nexternal-ui-name: zashboard\nsniffer:\n  enable: false\n  sniff:\n    TLS:\n      ports:\n        - 443\n    HTTP:\n      ports:\n        - 80\nproxies: []\nrules: []\n")
	rendered, err := RenderRuntimeProfile(profile, AppConfig{
		MixedPort:         7890,
		HTTPPort:          7891,
		SOCKS5Port:        7892,
		Mode:              "global",
		LogLevel:          "warning",
		GeoUpdateInterval: 24,
		IPv6:              false,
	})
	if err != nil {
		t.Fatal(err)
	}

	raw := map[string]any{}
	if err := yaml.Unmarshal(rendered, &raw); err != nil {
		t.Fatal(err)
	}

	assertNumber(t, raw, "mixed-port", 7890)
	assertNumber(t, raw, "port", 7891)
	assertNumber(t, raw, "socks-port", 7892)
	assertNumber(t, raw, "geo-update-interval", 24)
	if raw["geo-auto-update"] != false {
		t.Fatalf("mihomo geo auto update should be disabled for tool-managed scheduling: %v", raw["geo-auto-update"])
	}
	if raw["mode"] != "global" {
		t.Fatalf("mode was not overridden: %v", raw["mode"])
	}
	if raw["log-level"] != "warning" {
		t.Fatalf("log-level was not overridden: %v", raw["log-level"])
	}
	if raw["ipv6"] != false {
		t.Fatalf("ipv6 was not overridden: %v", raw["ipv6"])
	}
	if raw["allow-lan"] != true {
		t.Fatalf("allow-lan was not enabled: %v", raw["allow-lan"])
	}
	if raw["bind-address"] != "*" {
		t.Fatalf("bind-address was not opened for LAN proxy use: %v", raw["bind-address"])
	}
	if _, ok := raw["external-controller"]; ok {
		t.Fatalf("external controller should not be exposed on TCP: %v", raw["external-controller"])
	}
	if _, ok := raw["external-controller-tls"]; ok {
		t.Fatalf("external controller should not be exposed on TLS: %v", raw["external-controller-tls"])
	}
	if raw["external-controller-unix"] != coreControllerSocketRelPath {
		t.Fatalf("external controller unix mismatch: %v", raw["external-controller-unix"])
	}
	if _, ok := raw["secret"]; ok {
		t.Fatalf("controller secret should be removed: %v", raw["secret"])
	}
	for _, key := range []string{"external-ui", "external-ui-url", "external-ui-name"} {
		if _, ok := raw[key]; ok {
			t.Fatalf("%s should be removed when dashboard is served by admin tool: %v", key, raw[key])
		}
	}
	if _, ok := raw["proxies"]; !ok {
		t.Fatalf("unmanaged subscription fields were not preserved: %s", string(rendered))
	}
	sniffer := assertMap(t, raw, "sniffer")
	if sniffer["enable"] != true {
		t.Fatalf("sniffer was not enabled: %v", sniffer["enable"])
	}
	if sniffer["override-destination"] != true {
		t.Fatalf("sniffer override-destination was not enabled: %v", sniffer["override-destination"])
	}
	sniff := assertMap(t, sniffer, "sniff")
	tlsSniff := assertMap(t, sniff, "TLS")
	if _, ok := tlsSniff["ports"]; !ok {
		t.Fatalf("existing TLS sniffer config was not preserved: %#v", tlsSniff)
	}
	if _, ok := sniff["HTTP"]; !ok {
		t.Fatalf("existing non-TLS sniffer config was not preserved: %#v", sniff)
	}
}

func TestDownloadWithRetryRetriesTwice(t *testing.T) {
	var attempts atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if attempts.Add(1) < 3 {
			http.Error(w, "temporary failure", http.StatusBadGateway)
			return
		}
		_, _ = w.Write([]byte("mixed-port: 7890\n"))
	}))
	defer server.Close()

	body, gotAttempts, err := DownloadWithRetry(context.Background(), server.URL, 2, time.Millisecond)
	if err != nil {
		t.Fatal(err)
	}
	if gotAttempts != 3 || attempts.Load() != 3 {
		t.Fatalf("expected 3 attempts, got return=%d server=%d", gotAttempts, attempts.Load())
	}
	if !strings.Contains(string(body), "mixed-port") {
		t.Fatalf("unexpected body: %q", string(body))
	}
}

func TestLoadProxyGroups(t *testing.T) {
	path := filepath.Join(t.TempDir(), "profile.yml")
	err := os.WriteFile(path, []byte(`
proxy-groups:
  - name: Auto
    type: url-test
    proxies:
      - HK 01
      - SG 01
  - name: Provider Group
    type: select
    use:
      - airport
`), 0o644)
	if err != nil {
		t.Fatal(err)
	}

	groups := LoadProxyGroups(path)
	if len(groups) != 2 {
		t.Fatalf("expected 2 groups, got %d", len(groups))
	}
	if groups[0].Name != "Auto" || groups[0].Type != "url-test" || len(groups[0].Proxies) != 2 {
		t.Fatalf("unexpected first group: %+v", groups[0])
	}
	if groups[1].Name != "Provider Group" || len(groups[1].Use) != 1 || groups[1].Use[0] != "airport" {
		t.Fatalf("unexpected second group: %+v", groups[1])
	}
}

func TestShouldRefreshSubscriptionOnlyWhenURLChanges(t *testing.T) {
	current := AppConfig{
		SubscriptionURL:       "https://example.com/a",
		MixedPort:             7890,
		HTTPPort:              7891,
		SOCKS5Port:            7892,
		UpdateIntervalMinutes: 60,
	}
	portsOnly := current
	portsOnly.MixedPort = 17890

	if ShouldRefreshSubscription(current, portsOnly) {
		t.Fatal("port-only changes should not refresh subscription")
	}

	next := current
	next.SubscriptionURL = "https://example.com/b"
	if !ShouldRefreshSubscription(current, next) {
		t.Fatal("subscription URL changes should refresh subscription")
	}
}

func TestGeoUpdateDurationUsesHours(t *testing.T) {
	got := geoUpdateDuration(AppConfig{GeoUpdateInterval: 2})
	if got != 2*time.Hour {
		t.Fatalf("unexpected geo update duration: %v", got)
	}
}

func TestRunSchedulerUpdatesGeoDatabasesOnConfiguredInterval(t *testing.T) {
	app := newTestApp(t)
	app.subInterval = func(AppConfig) time.Duration {
		return time.Hour
	}
	app.geoInterval = func(AppConfig) time.Duration {
		return 10 * time.Millisecond
	}

	called := make(chan struct{}, 1)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	app.geoUpdater = func() error {
		select {
		case called <- struct{}{}:
		default:
		}
		cancel()
		return nil
	}

	go app.RunScheduler(ctx)

	select {
	case <-called:
	case <-time.After(time.Second):
		t.Fatal("geo updater was not called by scheduler")
	}
}

func TestConfigFromJSONAcceptsCoreFields(t *testing.T) {
	body := `{"subscription_url":"https://example.com/sub.yaml","mixed_port":7890,"http_port":7891,"socks5_port":7892,"update_interval_minutes":60,"mode":"global","log_level":"debug","geo_update_interval":12,"ipv6":false}`
	req := httptest.NewRequest(http.MethodPost, "/api/admin/config", strings.NewReader(body))

	cfg, err := ConfigFromJSON(req)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Mode != "global" || cfg.LogLevel != "debug" || cfg.GeoUpdateInterval != 12 || cfg.IPv6 {
		t.Fatalf("unexpected core config: %+v", cfg)
	}
}

func TestConfigFromJSONRejectsInvalidCoreFields(t *testing.T) {
	cases := []struct {
		name string
		body string
	}{
		{
			name: "mode",
			body: `{"subscription_url":"","mixed_port":7890,"http_port":7891,"socks5_port":7892,"update_interval_minutes":60,"mode":"bad","log_level":"info","geo_update_interval":24,"ipv6":true}`,
		},
		{
			name: "log level",
			body: `{"subscription_url":"","mixed_port":7890,"http_port":7891,"socks5_port":7892,"update_interval_minutes":60,"mode":"rule","log_level":"trace","geo_update_interval":24,"ipv6":true}`,
		},
		{
			name: "geo update interval",
			body: `{"subscription_url":"","mixed_port":7890,"http_port":7891,"socks5_port":7892,"update_interval_minutes":60,"mode":"rule","log_level":"info","geo_update_interval":0,"ipv6":true}`,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			req := httptest.NewRequest(http.MethodPost, "/api/admin/config", strings.NewReader(tc.body))
			if _, err := ConfigFromJSON(req); err == nil {
				t.Fatal("expected validation error")
			}
		})
	}
}

func TestAPIConfigSavePersistsCoreFields(t *testing.T) {
	app := newTestApp(t)
	if err := os.WriteFile(app.profilePath, []byte(":\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	body := `{"subscription_url":"","mixed_port":7890,"http_port":7891,"socks5_port":7892,"update_interval_minutes":60,"mode":"direct","log_level":"error","geo_update_interval":8,"ipv6":false}`
	req := httptest.NewRequest(http.MethodPost, "/api/admin/config", strings.NewReader(body))
	rec := httptest.NewRecorder()

	app.HandleAPIConfigSave(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("unexpected status: %d body=%q", rec.Code, rec.Body.String())
	}
	var got configResponse
	if err := json.NewDecoder(rec.Body).Decode(&got); err != nil {
		t.Fatal(err)
	}
	if got.Config.Mode != "direct" || got.Config.LogLevel != "error" || got.Config.GeoUpdateInterval != 8 || got.Config.IPv6 {
		t.Fatalf("unexpected saved config: %+v", got.Config)
	}
}

func TestAPIConfigSaveRejectsInvalidCoreFields(t *testing.T) {
	app := newTestApp(t)
	body := `{"subscription_url":"","mixed_port":7890,"http_port":7891,"socks5_port":7892,"update_interval_minutes":60,"mode":"bad","log_level":"info","geo_update_interval":24,"ipv6":true}`
	req := httptest.NewRequest(http.MethodPost, "/api/admin/config", strings.NewReader(body))
	rec := httptest.NewRecorder()

	app.HandleAPIConfigSave(rec, req)

	if rec.Code != http.StatusBadRequest {
		t.Fatalf("unexpected status: %d body=%q", rec.Code, rec.Body.String())
	}
}

func TestAPIConfigReturnsConfigStatusAndProxyGroups(t *testing.T) {
	app := newTestApp(t)
	err := os.WriteFile(app.profilePath, []byte(`
proxy-groups:
  - name: Auto
    type: url-test
    proxies:
      - HK 01
`), 0o644)
	if err != nil {
		t.Fatal(err)
	}
	app.SetAlert("subscription-error", "subscription update failed")

	req := httptest.NewRequest(http.MethodGet, "/api/config", nil)
	rec := httptest.NewRecorder()
	app.HandleAPIConfig(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("unexpected status: %d", rec.Code)
	}

	var got configResponse
	if err := json.NewDecoder(rec.Body).Decode(&got); err != nil {
		t.Fatal(err)
	}
	if got.Config.MixedPort != 7890 {
		t.Fatalf("unexpected config: %+v", got.Config)
	}
	if got.ClashVersion != "1.19.27" {
		t.Fatalf("unexpected clash version: %q", got.ClashVersion)
	}
	if got.SubscriptionError != "subscription update failed" {
		t.Fatalf("unexpected subscription error: %q", got.SubscriptionError)
	}
	if len(got.ProxyGroups) != 1 || got.ProxyGroups[0].Name != "Auto" {
		t.Fatalf("unexpected proxy groups: %+v", got.ProxyGroups)
	}
}

func TestAdminRoutesAndCoreProxyRouting(t *testing.T) {
	ensureDashboardIndexForTest(t)

	app := newTestApp(t)
	app.coreProxy = http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("X-Core-Proxy", "1")
		_, _ = w.Write([]byte("core:" + r.URL.Path))
	})
	mux := http.NewServeMux()
	app.RegisterRoutes(mux)

	indexReq := httptest.NewRequest(http.MethodGet, "/ui/", nil)
	indexRec := httptest.NewRecorder()
	mux.ServeHTTP(indexRec, indexReq)
	if indexRec.Code != http.StatusOK {
		t.Fatalf("unexpected index status: %d", indexRec.Code)
	}
	if !strings.Contains(indexRec.Body.String(), "Clash Server") {
		t.Fatalf("index did not contain page title")
	}
	if !strings.Contains(indexRec.Body.String(), "/api/admin/subscription/refresh") {
		t.Fatalf("index did not contain refresh subscription API call")
	}
	if !strings.Contains(indexRec.Body.String(), "/ui/dashboard/?") {
		t.Fatalf("index did not contain dashboard setup URL")
	}
	for _, param := range []string{"hostname", "port: dashboardPort", "secret: ''", "theme: 'auto'", "title: 'Clash Dashboard'"} {
		if !strings.Contains(indexRec.Body.String(), param) {
			t.Fatalf("index did not contain dashboard setup param %q", param)
		}
	}
	if !strings.Contains(indexRec.Body.String(), "setTimeout(() =>") || !strings.Contains(indexRec.Body.String(), "this.loadConfig({ silent: true })") {
		t.Fatalf("index should silently refresh after saving config")
	}
	if !strings.Contains(indexRec.Body.String(), "Clash 版本号") || !strings.Contains(indexRec.Body.String(), "clash_version") {
		t.Fatalf("index did not contain clash version field")
	}

	dashboardReq := httptest.NewRequest(http.MethodGet, "/ui/dashboard/", nil)
	dashboardRec := httptest.NewRecorder()
	mux.ServeHTTP(dashboardRec, dashboardReq)
	if dashboardRec.Code != http.StatusOK || dashboardRec.Header().Get("X-Core-Proxy") != "" {
		t.Fatalf("dashboard was not served by static handler: status=%d body=%q core=%q", dashboardRec.Code, dashboardRec.Body.String(), dashboardRec.Header().Get("X-Core-Proxy"))
	}

	assetReq := httptest.NewRequest(http.MethodGet, "/ui/static/vue.global.min.js", nil)
	assetRec := httptest.NewRecorder()
	mux.ServeHTTP(assetRec, assetReq)
	if assetRec.Code != http.StatusOK {
		t.Fatalf("unexpected vue asset status: %d", assetRec.Code)
	}

	adminReq := httptest.NewRequest(http.MethodGet, "/api/admin", nil)
	adminRec := httptest.NewRecorder()
	mux.ServeHTTP(adminRec, adminReq)
	if adminRec.Code != http.StatusOK || adminRec.Header().Get("X-Core-Proxy") != "" {
		t.Fatalf("admin config was not served by admin handler: status=%d body=%q", adminRec.Code, adminRec.Body.String())
	}

	saveReq := httptest.NewRequest(http.MethodPost, "/api/admin/config", strings.NewReader(`{`))
	saveReq.Header.Set("Content-Type", "application/json")
	saveRec := httptest.NewRecorder()
	mux.ServeHTTP(saveRec, saveReq)
	if saveRec.Code != http.StatusBadRequest || saveRec.Header().Get("X-Core-Proxy") != "" {
		t.Fatalf("admin config save was not served by admin handler: status=%d body=%q", saveRec.Code, saveRec.Body.String())
	}

	proxyCases := map[string]string{
		"/":                  "core:/",
		"/api/config":        "core:/api/config",
		"/api/admin/unknown": "core:/api/admin/unknown",
		"/proxies":           "core:/proxies",
		"/configs":           "core:/configs",
		"/ui/unknown":        "core:/ui/unknown",
		"/unknown/path":      "core:/unknown/path",
	}
	for path, want := range proxyCases {
		req := httptest.NewRequest(http.MethodGet, path, nil)
		rec := httptest.NewRecorder()
		mux.ServeHTTP(rec, req)
		if rec.Body.String() != want || rec.Header().Get("X-Core-Proxy") != "1" {
			t.Fatalf("%s routed incorrectly: status=%d body=%q core=%q", path, rec.Code, rec.Body.String(), rec.Header().Get("X-Core-Proxy"))
		}
	}
}

func ensureDashboardIndexForTest(t *testing.T) {
	t.Helper()

	dir := filepath.Join("src", "static", "dashboard")
	for _, candidate := range []string{
		filepath.Join("static", "dashboard"),
		filepath.Join("src", "static", "dashboard"),
	} {
		if info, err := os.Stat(candidate); err == nil && info.IsDir() {
			dir = candidate
			break
		} else if err != nil && !errors.Is(err, os.ErrNotExist) {
			t.Fatalf("stat dashboard directory: %v", err)
		}
	}

	indexPath := filepath.Join(dir, "index.html")
	if _, err := os.Stat(indexPath); err == nil {
		return
	} else if !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("stat dashboard index: %v", err)
	}

	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatalf("create dashboard test directory: %v", err)
	}
	if err := os.WriteFile(indexPath, []byte("<!doctype html><title>Test Dashboard</title>"), 0o644); err != nil {
		t.Fatalf("create dashboard test index: %v", err)
	}
	t.Cleanup(func() {
		_ = os.Remove(indexPath)
		_ = os.Remove(dir)
	})
}

func newTestApp(t *testing.T) *App {
	t.Helper()
	dataDir := t.TempDir()
	return &App{
		dataDir:     dataDir,
		configPath:  filepath.Join(dataDir, configFileName),
		profilePath: filepath.Join(dataDir, profileFileName),
		core:        NewMihomoCore(dataDir, filepath.Join(dataDir, profileFileName)),
		scheduleCh:  make(chan struct{}, 1),
		geoUpdater:  func() error { return nil },
		config:      DefaultAppConfig(),
	}
}

func assertNumber(t *testing.T, raw map[string]any, key string, want int) {
	t.Helper()
	got, ok := raw[key].(int)
	if !ok {
		t.Fatalf("%s has type %T, want int", key, raw[key])
	}
	if got != want {
		t.Fatalf("%s=%d, want %d", key, got, want)
	}
}

func assertMap(t *testing.T, raw map[string]any, key string) map[string]any {
	t.Helper()
	got, ok := raw[key].(map[string]any)
	if !ok {
		t.Fatalf("%s has type %T, want map[string]any", key, raw[key])
	}
	return got
}
