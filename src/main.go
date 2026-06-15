package main

import (
	"bytes"
	"context"
	"embed"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"log"
	"net"
	"net/http"
	"net/http/httputil"
	"os"
	"path/filepath"
	"runtime/debug"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/metacubex/mihomo/component/updater"
	mihomoConfig "github.com/metacubex/mihomo/config"
	C "github.com/metacubex/mihomo/constant"
	"github.com/metacubex/mihomo/hub"
	"github.com/metacubex/mihomo/hub/executor"
	"gopkg.in/yaml.v3"
)

const (
	configFileName              = "config.yml"
	profileFileName             = "profile.yml"
	coreControllerSocketRelPath = "mihomo-controller.sock"
	mihomoModulePath            = "github.com/metacubex/mihomo"
)

//go:embed static/index.html static/bootstrap.min.css static/vue.global.min.js
var assetFS embed.FS

type AppConfig struct {
	SubscriptionURL       string `yaml:"subscription_url" json:"subscription_url"`
	MixedPort             int    `yaml:"mixed_port" json:"mixed_port"`
	HTTPPort              int    `yaml:"http_port" json:"http_port"`
	SOCKS5Port            int    `yaml:"socks5_port" json:"socks5_port"`
	UpdateIntervalMinutes int    `yaml:"update_interval_minutes" json:"update_interval_minutes"`
	Mode                  string `yaml:"mode" json:"mode"`
	LogLevel              string `yaml:"log_level" json:"log_level"`
	GeoUpdateInterval     int    `yaml:"geo_update_interval" json:"geo_update_interval"`
	IPv6                  bool   `yaml:"ipv6" json:"ipv6"`
	ManagementPort        int    `yaml:"management_port" json:"management_port"`
	LastUpdatedAt         string `yaml:"last_updated_at" json:"last_updated_at"`
}

type Alert struct {
	Type      string
	Message   string
	UpdatedAt time.Time
}

type App struct {
	dataDir     string
	configPath  string
	profilePath string
	core        *MihomoCore
	coreProxy   http.Handler
	scheduleCh  chan struct{}
	refreshMu   sync.Mutex
	geoUpdateMu sync.Mutex
	geoUpdater  func() error
	subInterval func(AppConfig) time.Duration
	geoInterval func(AppConfig) time.Duration

	mu     sync.RWMutex
	config AppConfig
	alert  Alert
}

type MihomoCore struct {
	mu          sync.Mutex
	dataDir     string
	profilePath string
	ready       bool
}

type ProxyGroup struct {
	Name    string   `json:"name"`
	Type    string   `json:"type"`
	Proxies []string `json:"proxies"`
	Use     []string `json:"use"`
}

type actionResponse struct {
	OK                bool   `json:"ok"`
	Message           string `json:"message,omitempty"`
	CoreRunning       bool   `json:"core_running"`
	SubscriptionError string `json:"subscription_error"`
}

type configResponse struct {
	Config            AppConfig    `json:"config"`
	CoreRunning       bool         `json:"core_running"`
	ClashVersion      string       `json:"clash_version"`
	ProxyGroups       []ProxyGroup `json:"proxy_groups"`
	SubscriptionError string       `json:"subscription_error"`
}

func main() {
	app, err := NewApp()
	if err != nil {
		log.Fatalf("init app: %v", err)
	}

	ctx := context.Background()
	go app.RunScheduler(ctx)
	go func() {
		if err := app.RefreshWithSubscription(ctx); err != nil {
			app.SetAlert("subscription-error", fmt.Sprintf("subscription update failed: %v", err))
		}
	}()

	mux := http.NewServeMux()
	app.RegisterRoutes(mux)

	adminAddr := fmt.Sprintf(":%d", app.CurrentConfig().ManagementPort)
	log.Printf("admin UI listening on http://127.0.0.1%s/ui/", adminAddr)
	if err := http.ListenAndServe(adminAddr, mux); err != nil {
		log.Fatal(err)
	}
}

func NewApp() (*App, error) {
	wd, err := os.Getwd()
	if err != nil {
		return nil, err
	}

	dataDir := filepath.Join(wd, "data")
	configPath := filepath.Join(dataDir, configFileName)
	profilePath := filepath.Join(dataDir, profileFileName)

	if err := os.MkdirAll(dataDir, 0o755); err != nil {
		return nil, err
	}

	cfg, err := LoadAppConfig(configPath)
	if err != nil {
		return nil, err
	}
	app := &App{
		dataDir:     dataDir,
		configPath:  configPath,
		profilePath: profilePath,
		core:        NewMihomoCore(dataDir, profilePath),
		scheduleCh:  make(chan struct{}, 1),
		geoUpdater:  updater.UpdateGeoDatabases,
		config:      cfg,
	}

	if err := SaveAppConfig(configPath, cfg); err != nil {
		return nil, err
	}

	return app, nil
}

func DefaultAppConfig() AppConfig {
	return AppConfig{
		MixedPort:             7890,
		HTTPPort:              7891,
		SOCKS5Port:            7892,
		UpdateIntervalMinutes: 60,
		Mode:                  "rule",
		LogLevel:              "info",
		GeoUpdateInterval:     24,
		IPv6:                  true,
		ManagementPort:        9090,
	}
}

func LoadAppConfig(path string) (AppConfig, error) {
	cfg := DefaultAppConfig()
	b, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return cfg, nil
	}
	if err != nil {
		return cfg, err
	}
	if len(bytes.TrimSpace(b)) == 0 {
		return cfg, nil
	}
	if err := yaml.Unmarshal(b, &cfg); err != nil {
		return cfg, err
	}
	cfg = normalizeConfig(cfg, false)
	return cfg, nil
}

func SaveAppConfig(path string, cfg AppConfig) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	b, err := yaml.Marshal(normalizeConfig(cfg, false))
	if err != nil {
		return err
	}
	return os.WriteFile(path, b, 0o644)
}

func NormalizeConfig(cfg AppConfig) AppConfig {
	return normalizeConfig(cfg, true)
}

func normalizeConfig(cfg AppConfig, defaultIPv6 bool) AppConfig {
	if cfg.MixedPort == 0 {
		cfg.MixedPort = 7890
	}
	if cfg.HTTPPort == 0 {
		cfg.HTTPPort = 7891
	}
	if cfg.SOCKS5Port == 0 {
		cfg.SOCKS5Port = 7892
	}
	if cfg.UpdateIntervalMinutes <= 0 {
		cfg.UpdateIntervalMinutes = 60
	}
	cfg.Mode = strings.ToLower(strings.TrimSpace(cfg.Mode))
	if cfg.Mode == "" {
		cfg.Mode = "rule"
	}
	cfg.LogLevel = strings.ToLower(strings.TrimSpace(cfg.LogLevel))
	if cfg.LogLevel == "" {
		cfg.LogLevel = "info"
	}
	if cfg.GeoUpdateInterval <= 0 {
		cfg.GeoUpdateInterval = 24
	}
	if defaultIPv6 && !cfg.IPv6 {
		cfg.IPv6 = true
	}
	if cfg.ManagementPort == 0 {
		cfg.ManagementPort = 9090
	}
	cfg.SubscriptionURL = strings.TrimSpace(cfg.SubscriptionURL)
	cfg.LastUpdatedAt = strings.TrimSpace(cfg.LastUpdatedAt)
	return cfg
}

func (a *App) RegisterRoutes(mux *http.ServeMux) {
	staticFS, err := fs.Sub(assetFS, "static")
	if err != nil {
		panic(err)
	}

	mux.HandleFunc("/api/admin", a.HandleAPIConfig)
	mux.HandleFunc("/api/admin/config", a.HandleAPIConfigSave)
	mux.HandleFunc("/api/admin/proxy-groups", a.HandleAPIProxyGroups)
	mux.HandleFunc("/api/admin/subscription/refresh", a.HandleAPIRefreshSubscription)
	mux.HandleFunc("/api/admin/core/start", a.HandleAPIStartCore)
	mux.HandleFunc("/api/admin/core/stop", a.HandleAPIStopCore)
	mux.HandleFunc("/api/admin/core/restart", a.HandleAPIRestartCore)
	mux.Handle("/ui/static/", http.StripPrefix("/ui/static/", http.FileServer(http.FS(staticFS))))
	mux.Handle("/ui/dashboard/", http.StripPrefix("/ui/dashboard/", http.FileServer(http.Dir(DashboardStaticDir()))))
	mux.HandleFunc("/ui/dashboard", redirectTo("/ui/dashboard/"))
	mux.HandleFunc("/ui", redirectTo("/ui/"))
	mux.HandleFunc("/ui/", func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/ui/" {
			a.coreProxyHandler().ServeHTTP(w, r)
			return
		}
		body, err := assetFS.ReadFile("static/index.html")
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		_, _ = w.Write(body)
	})
	mux.Handle("/", a.coreProxyHandler())
}

func redirectTo(path string) func(http.ResponseWriter, *http.Request) {
	return func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, path, http.StatusMovedPermanently)
	}
}

func DashboardStaticDir() string {
	for _, dir := range []string{
		filepath.Join("static", "dashboard"),
		filepath.Join("src", "static", "dashboard"),
	} {
		if info, err := os.Stat(dir); err == nil && info.IsDir() {
			return dir
		}
	}
	return filepath.Join("static", "dashboard")
}

func (a *App) coreProxyHandler() http.Handler {
	if a.coreProxy != nil {
		return a.coreProxy
	}
	return NewCoreProxyHandler(filepath.Join(a.dataDir, coreControllerSocketRelPath))
}

func NewCoreProxyHandler(socketPath string) http.Handler {
	return &httputil.ReverseProxy{
		Director: func(r *http.Request) {
			r.URL.Scheme = "http"
			r.URL.Host = "mihomo"
			r.Host = "mihomo"
		},
		Transport: &http.Transport{
			DialContext: func(ctx context.Context, network, addr string) (net.Conn, error) {
				var dialer net.Dialer
				return dialer.DialContext(ctx, "unix", socketPath)
			},
		},
	}
}

func (a *App) HandleAPIConfig(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	respondJSON(w, http.StatusOK, a.ConfigSnapshot())
}

func (a *App) HandleAPIConfigSave(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	a.handleSaveConfig(w, r)
}

func (a *App) handleSaveConfig(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	current := a.CurrentConfig()

	cfg, err := ConfigFromJSON(r)
	if err != nil {
		respondAction(w, http.StatusBadRequest, err.Error(), a)
		return
	}
	subscriptionChanged := ShouldRefreshSubscription(current, cfg)
	cfg.ManagementPort = current.ManagementPort
	cfg.LastUpdatedAt = current.LastUpdatedAt

	if err := SaveAppConfig(a.configPath, cfg); err != nil {
		respondAction(w, http.StatusInternalServerError, "save config failed", a)
		return
	}

	a.mu.Lock()
	a.config = cfg
	a.mu.Unlock()
	a.SignalScheduleChange()

	go func() {
		var err error
		if subscriptionChanged {
			err = a.RefreshWithSubscription(context.Background())
		} else {
			err = a.ReloadLocalProfile()
		}
		if err != nil {
			if subscriptionChanged {
				a.SetAlert("subscription-error", fmt.Sprintf("subscription update failed: %v", err))
			} else {
				a.SetAlert("reload-error", err.Error())
			}
		}
	}()
	respondJSON(w, http.StatusOK, a.ConfigSnapshot())
}

func (a *App) HandleAPIProxyGroups(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	respondJSON(w, http.StatusOK, map[string]any{
		"proxy_groups": LoadProxyGroups(a.profilePath),
	})
}

func (a *App) HandleAPIRefreshSubscription(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}

	if err := a.RefreshWithSubscription(r.Context()); err != nil {
		a.SetAlert("subscription-error", fmt.Sprintf("subscription update failed: %v", err))
		respondAction(w, http.StatusInternalServerError, err.Error(), a)
		return
	}
	respondAction(w, http.StatusOK, "subscription refreshed", a)
}

func (a *App) HandleAPIStartCore(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	if err := a.ReloadLocalProfile(); err != nil {
		respondAction(w, http.StatusInternalServerError, err.Error(), a)
		return
	}
	respondAction(w, http.StatusOK, "started", a)
}

func (a *App) HandleAPIStopCore(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	a.core.Stop()
	respondAction(w, http.StatusOK, "stopped", a)
}

func (a *App) HandleAPIRestartCore(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	a.core.Stop()
	if err := a.ReloadLocalProfile(); err != nil {
		respondAction(w, http.StatusInternalServerError, err.Error(), a)
		return
	}
	respondAction(w, http.StatusOK, "restarted", a)
}

func (a *App) ConfigSnapshot() configResponse {
	a.mu.RLock()
	cfg := a.config
	alert := a.alert
	a.mu.RUnlock()

	subscriptionError := ""
	if alert.Type == "subscription-error" {
		subscriptionError = alert.Message
	}

	return configResponse{
		Config:            cfg,
		CoreRunning:       a.core.Running(),
		ClashVersion:      MihomoLibraryVersion(),
		ProxyGroups:       LoadProxyGroups(a.profilePath),
		SubscriptionError: subscriptionError,
	}
}

func MihomoLibraryVersion() string {
	info, ok := debug.ReadBuildInfo()
	if ok {
		for _, dep := range info.Deps {
			if dep.Path != mihomoModulePath {
				continue
			}
			if dep.Replace != nil && dep.Replace.Version != "" {
				return strings.TrimPrefix(dep.Replace.Version, "v")
			}
			if dep.Version != "" {
				return strings.TrimPrefix(dep.Version, "v")
			}
		}
	}
	return strings.TrimPrefix(C.Version, "v")
}

func respondAction(w http.ResponseWriter, status int, message string, app *App) {
	snapshot := app.ConfigSnapshot()
	respondJSON(w, status, actionResponse{
		OK:                status >= 200 && status < 300,
		Message:           message,
		CoreRunning:       snapshot.CoreRunning,
		SubscriptionError: snapshot.SubscriptionError,
	})
}

func respondJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

func ConfigFromForm(r *http.Request) (AppConfig, error) {
	cfg := AppConfig{
		SubscriptionURL: strings.TrimSpace(r.FormValue("subscription_url")),
	}

	var err error
	if cfg.MixedPort, err = parsePort(r.FormValue("mixed_port"), "Mixed port"); err != nil {
		return cfg, err
	}
	if cfg.HTTPPort, err = parsePort(r.FormValue("http_port"), "HTTP port"); err != nil {
		return cfg, err
	}
	if cfg.SOCKS5Port, err = parsePort(r.FormValue("socks5_port"), "SOCKS5 port"); err != nil {
		return cfg, err
	}

	cfg.UpdateIntervalMinutes, err = strconv.Atoi(strings.TrimSpace(r.FormValue("update_interval_minutes")))
	if err != nil || cfg.UpdateIntervalMinutes <= 0 {
		return cfg, fmt.Errorf("update interval must be a positive number of minutes")
	}
	cfg.Mode = r.FormValue("mode")
	cfg.LogLevel = r.FormValue("log_level")
	cfg.GeoUpdateInterval, err = strconv.Atoi(strings.TrimSpace(r.FormValue("geo_update_interval")))
	if err != nil || cfg.GeoUpdateInterval <= 0 {
		return cfg, fmt.Errorf("geo update interval must be a positive number of hours")
	}
	cfg.IPv6 = r.FormValue("ipv6") == "true" || r.FormValue("ipv6") == "on"

	cfg = normalizeConfig(cfg, false)
	if err := validateCoreConfig(cfg); err != nil {
		return cfg, err
	}
	return cfg, nil
}

func ConfigFromJSON(r *http.Request) (AppConfig, error) {
	body, err := io.ReadAll(io.LimitReader(r.Body, 1<<20))
	if err != nil {
		return AppConfig{}, fmt.Errorf("read JSON config failed: %w", err)
	}

	var cfg AppConfig
	if err := json.Unmarshal(body, &cfg); err != nil {
		return cfg, fmt.Errorf("invalid JSON config: %w", err)
	}
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(body, &fields); err != nil {
		return cfg, fmt.Errorf("invalid JSON config: %w", err)
	}

	cfg.SubscriptionURL = strings.TrimSpace(cfg.SubscriptionURL)
	if _, err := validatePort(cfg.MixedPort, "Mixed port"); err != nil {
		return cfg, err
	}
	if _, err := validatePort(cfg.HTTPPort, "HTTP port"); err != nil {
		return cfg, err
	}
	if _, err := validatePort(cfg.SOCKS5Port, "SOCKS5 port"); err != nil {
		return cfg, err
	}
	if cfg.UpdateIntervalMinutes <= 0 {
		return cfg, fmt.Errorf("update interval must be a positive number of minutes")
	}
	if _, ok := fields["geo_update_interval"]; ok && cfg.GeoUpdateInterval <= 0 {
		return cfg, fmt.Errorf("geo update interval must be a positive number of hours")
	}
	if cfg.ManagementPort != 0 {
		if _, err := validatePort(cfg.ManagementPort, "Management port"); err != nil {
			return cfg, err
		}
	}
	cfg = normalizeConfig(cfg, false)
	if err := validateCoreConfig(cfg); err != nil {
		return cfg, err
	}

	return cfg, nil
}

func ShouldRefreshSubscription(current, next AppConfig) bool {
	return current.SubscriptionURL != next.SubscriptionURL
}

func LoadProxyGroups(path string) []ProxyGroup {
	body, err := os.ReadFile(path)
	if err != nil || len(bytes.TrimSpace(body)) == 0 {
		return nil
	}

	var raw struct {
		ProxyGroups []struct {
			Name    string   `yaml:"name"`
			Type    string   `yaml:"type"`
			Proxies []string `yaml:"proxies"`
			Use     []string `yaml:"use"`
		} `yaml:"proxy-groups"`
	}
	if err := yaml.Unmarshal(body, &raw); err != nil {
		return nil
	}

	groups := make([]ProxyGroup, 0, len(raw.ProxyGroups))
	for _, group := range raw.ProxyGroups {
		groups = append(groups, ProxyGroup{
			Name:    group.Name,
			Type:    group.Type,
			Proxies: group.Proxies,
			Use:     group.Use,
		})
	}
	return groups
}

func parsePort(raw, name string) (int, error) {
	port, err := strconv.Atoi(strings.TrimSpace(raw))
	if err != nil || port < 1 || port > 65535 {
		return 0, fmt.Errorf("%s must be between 1 and 65535", name)
	}
	return port, nil
}

func validatePort(port int, name string) (int, error) {
	if port < 1 || port > 65535 {
		return 0, fmt.Errorf("%s must be between 1 and 65535", name)
	}
	return port, nil
}

func validateCoreConfig(cfg AppConfig) error {
	if !isAllowedValue(cfg.Mode, []string{"rule", "global", "direct"}) {
		return fmt.Errorf("mode must be one of: rule, global, direct")
	}
	if !isAllowedValue(cfg.LogLevel, []string{"silent", "error", "warning", "info", "debug"}) {
		return fmt.Errorf("log level must be one of: silent, error, warning, info, debug")
	}
	if cfg.GeoUpdateInterval <= 0 {
		return fmt.Errorf("geo update interval must be a positive number of hours")
	}
	return nil
}

func isAllowedValue(value string, allowed []string) bool {
	for _, item := range allowed {
		if value == item {
			return true
		}
	}
	return false
}

func (a *App) RefreshWithSubscription(ctx context.Context) error {
	a.refreshMu.Lock()
	defer a.refreshMu.Unlock()
	return a.refreshWithSubscriptionLocked(ctx)
}

func (a *App) refreshWithSubscriptionLocked(ctx context.Context) error {
	cfg := a.CurrentConfig()
	if cfg.SubscriptionURL != "" {
		body, attempts, err := DownloadWithRetry(ctx, cfg.SubscriptionURL, 2, 10*time.Second)
		if err != nil {
			return fmt.Errorf("subscription download failed after %d attempts: %w", attempts, err)
		}
		if err := a.core.ReloadWithProfile(body, cfg); err != nil {
			return fmt.Errorf("mihomo refresh failed after subscription download: %w", err)
		}
		_ = attempts
		a.MarkUpdated()
		return nil
	}
	return a.reloadLocalProfileLocked()
}

func (a *App) ReloadLocalProfile() error {
	a.refreshMu.Lock()
	defer a.refreshMu.Unlock()
	return a.reloadLocalProfileLocked()
}

func (a *App) reloadLocalProfileLocked() error {
	cfg := a.CurrentConfig()
	if err := a.core.ReloadWithExistingProfile(cfg); err != nil {
		return fmt.Errorf("mihomo refresh failed: %w", err)
	}
	a.ClearAlert()
	return nil
}

func (a *App) UpdateGeoDatabases() error {
	a.geoUpdateMu.Lock()
	defer a.geoUpdateMu.Unlock()

	update := a.geoUpdater
	if update == nil {
		update = updater.UpdateGeoDatabases
	}
	return update()
}

func (a *App) RunScheduler(ctx context.Context) {
	cfg := a.CurrentConfig()
	subscriptionTimer := time.NewTimer(a.subscriptionUpdateDuration(cfg))
	geoTimer := time.NewTimer(a.geoUpdateDuration(cfg))
	defer subscriptionTimer.Stop()
	defer geoTimer.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case <-a.scheduleCh:
			cfg := a.CurrentConfig()
			resetTimer(subscriptionTimer, a.subscriptionUpdateDuration(cfg))
			resetTimer(geoTimer, a.geoUpdateDuration(cfg))
		case <-subscriptionTimer.C:
			if err := a.RefreshWithSubscription(ctx); err != nil {
				a.SetAlert("subscription-error", fmt.Sprintf("subscription update failed: %v", err))
			}
			resetTimer(subscriptionTimer, a.subscriptionUpdateDuration(a.CurrentConfig()))
		case <-geoTimer.C:
			if err := a.UpdateGeoDatabases(); err != nil {
				a.SetAlert("geo-update-error", fmt.Sprintf("geo update failed: %v", err))
			}
			resetTimer(geoTimer, a.geoUpdateDuration(a.CurrentConfig()))
		}
	}
}

func (a *App) subscriptionUpdateDuration(cfg AppConfig) time.Duration {
	if a.subInterval != nil {
		return a.subInterval(cfg)
	}
	return subscriptionUpdateDuration(cfg)
}

func (a *App) geoUpdateDuration(cfg AppConfig) time.Duration {
	if a.geoInterval != nil {
		return a.geoInterval(cfg)
	}
	return geoUpdateDuration(cfg)
}

func subscriptionUpdateDuration(cfg AppConfig) time.Duration {
	cfg = normalizeConfig(cfg, false)
	return time.Duration(cfg.UpdateIntervalMinutes) * time.Minute
}

func geoUpdateDuration(cfg AppConfig) time.Duration {
	cfg = normalizeConfig(cfg, false)
	return time.Duration(cfg.GeoUpdateInterval) * time.Hour
}

func resetTimer(timer *time.Timer, duration time.Duration) {
	if !timer.Stop() {
		select {
		case <-timer.C:
		default:
		}
	}
	timer.Reset(duration)
}

func (a *App) SignalScheduleChange() {
	select {
	case a.scheduleCh <- struct{}{}:
	default:
	}
}

func (a *App) CurrentConfig() AppConfig {
	a.mu.RLock()
	defer a.mu.RUnlock()
	return a.config
}

func (a *App) SetAlert(kind, message string) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.alert = Alert{Type: kind, Message: message, UpdatedAt: time.Now()}
}

func (a *App) ClearAlert() {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.alert = Alert{}
}

func (a *App) MarkUpdated() {
	a.mu.Lock()
	a.config.LastUpdatedAt = time.Now().Format("2006-01-02 15:04:05")
	cfg := a.config
	a.alert = Alert{}
	a.mu.Unlock()
	if err := SaveAppConfig(a.configPath, cfg); err != nil {
		a.SetAlert("config-error", fmt.Sprintf("save last update time failed: %v", err))
	}
}

func NewMihomoCore(dataDir, profilePath string) *MihomoCore {
	C.SetHomeDir(dataDir)
	C.SetConfig(profilePath)
	return &MihomoCore{dataDir: dataDir, profilePath: profilePath}
}

func (m *MihomoCore) ReloadWithExistingProfile(cfg AppConfig) error {
	body, err := os.ReadFile(m.profilePath)
	if errors.Is(err, os.ErrNotExist) || len(bytes.TrimSpace(body)) == 0 {
		body = []byte("mixed-port: 7890\nmode: rule\nlog-level: info\n")
	} else if err != nil {
		return err
	}
	return m.ReloadWithProfile(body, cfg)
}

func (m *MihomoCore) Running() bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.ready
}

func (m *MihomoCore) Stop() {
	m.mu.Lock()
	defer m.mu.Unlock()
	if !m.ready {
		return
	}
	executor.Shutdown()
	m.ready = false
}

func (m *MihomoCore) ReloadWithProfile(profile []byte, cfg AppConfig) error {
	m.mu.Lock()
	defer m.mu.Unlock()

	rendered, err := RenderRuntimeProfile(profile, cfg)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(m.dataDir, 0o755); err != nil {
		return err
	}
	if err := os.WriteFile(m.profilePath, rendered, 0o644); err != nil {
		return err
	}

	C.SetHomeDir(m.dataDir)
	C.SetConfig(m.profilePath)
	if err := mihomoConfig.Init(m.dataDir); err != nil {
		return err
	}

	parsed, err := mihomoConfig.Parse(rendered)
	if err != nil {
		return err
	}
	hub.ApplyConfig(parsed)
	m.ready = true
	return nil
}

func RenderRuntimeProfile(profile []byte, cfg AppConfig) ([]byte, error) {
	cfg = normalizeConfig(cfg, false)
	raw := map[string]any{}
	if len(bytes.TrimSpace(profile)) > 0 {
		if err := yaml.Unmarshal(profile, &raw); err != nil {
			return nil, err
		}
	}

	raw["mixed-port"] = cfg.MixedPort
	raw["port"] = cfg.HTTPPort
	raw["socks-port"] = cfg.SOCKS5Port
	raw["allow-lan"] = true
	raw["bind-address"] = "*"
	raw["mode"] = cfg.Mode
	raw["log-level"] = cfg.LogLevel
	raw["geo-auto-update"] = false
	raw["geo-update-interval"] = cfg.GeoUpdateInterval
	raw["ipv6"] = cfg.IPv6
	ensureTLSSniffer(raw)
	delete(raw, "external-controller")
	delete(raw, "external-controller-tls")
	raw["external-controller-unix"] = coreControllerSocketRelPath
	delete(raw, "external-ui")
	delete(raw, "external-ui-url")
	delete(raw, "external-ui-name")
	delete(raw, "secret")

	return yaml.Marshal(raw)
}

func ensureTLSSniffer(raw map[string]any) {
	sniffer, ok := raw["sniffer"].(map[string]any)
	if !ok {
		sniffer = map[string]any{}
		raw["sniffer"] = sniffer
	}
	sniffer["enable"] = true
	sniffer["override-destination"] = true

	sniff, ok := sniffer["sniff"].(map[string]any)
	if !ok {
		sniff = map[string]any{}
		sniffer["sniff"] = sniff
	}
	if _, ok := sniff["TLS"]; !ok {
		sniff["TLS"] = map[string]any{}
	}
}

func DownloadWithRetry(ctx context.Context, url string, retries int, delay time.Duration) ([]byte, int, error) {
	client := &http.Client{Timeout: 30 * time.Second}
	var lastErr error
	total := retries + 1

	for attempt := 1; attempt <= total; attempt++ {
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
		if err != nil {
			return nil, attempt, err
		}
		req.Header.Set("User-Agent", "clash-server/1.0")

		resp, err := client.Do(req)
		if err == nil {
			body, readErr := readSubscriptionBody(resp)
			if readErr == nil {
				return body, attempt, nil
			}
			lastErr = readErr
		} else {
			lastErr = err
		}

		if attempt < total {
			select {
			case <-ctx.Done():
				return nil, attempt, ctx.Err()
			case <-time.After(delay):
			}
		}
	}

	return nil, total, lastErr
}

func readSubscriptionBody(resp *http.Response) ([]byte, error) {
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		_, _ = io.Copy(io.Discard, resp.Body)
		return nil, fmt.Errorf("unexpected HTTP status %s", resp.Status)
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, 32<<20))
	if err != nil {
		return nil, err
	}
	if len(bytes.TrimSpace(body)) == 0 {
		return nil, fmt.Errorf("subscription response is empty")
	}
	return body, nil
}
