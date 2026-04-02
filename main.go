package main

import (
	"bufio"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"html/template"
	"io"
	"io/fs"
	"log"
	"math"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"

	"gopkg.in/yaml.v3"
)

type Config struct {
	Server struct {
		Port int `yaml:"port"`
	} `yaml:"server"`
	Agents struct {
		Copilot struct {
			Enabled bool `yaml:"enabled"`
		} `yaml:"copilot"`
		Codex struct {
			Enabled bool `yaml:"enabled"`
		} `yaml:"codex"`
	} `yaml:"agents"`
}

type UsageBar struct {
	Label      string
	Percentage int
	Reset      string
}

type MeterSegment struct {
	Filled bool
}

type AgentStats struct {
	Name      string
	Bars      []UsageBar
	Detail    string
	IsRunning bool
}

type ProviderBar struct {
	Label       string
	PercentText string
	Segments    []MeterSegment
	Meta        string
	Available   bool
}

type ProviderPanel struct {
	Name   string
	Detail string
	Bars   []ProviderBar
}

type ExchangeRateRow struct {
	Pair  string
	Value string
}

type DashboardData struct {
	Providers []ProviderPanel
	Rates     []ExchangeRateRow
	Time      string
	Runtime   string
}

type codexRateLimitEntry struct {
	UsedPercent     float64 `json:"used_percent"`
	WindowMinutes   int     `json:"window_minutes"`
	ResetsAt        int64   `json:"resets_at"`
	ResetsInSeconds int64   `json:"resets_in_seconds"`
}

type codexCredits struct {
	Balance    string `json:"balance"`
	HasCredits bool   `json:"has_credits"`
	Unlimited  bool   `json:"unlimited"`
}

type codexRateLimits struct {
	Primary   codexRateLimitEntry `json:"primary"`
	Secondary codexRateLimitEntry `json:"secondary"`
	Credits   *codexCredits       `json:"credits"`
	PlanType  string              `json:"plan_type"`
}

type codexSessionEvent struct {
	Type    string `json:"type"`
	Payload struct {
		Type       string          `json:"type"`
		RateLimits codexRateLimits `json:"rate_limits"`
	} `json:"payload"`
}

type copilotAppEntry struct {
	OAuthToken string `json:"oauth_token"`
}

type copilotUsageResponse struct {
	CopilotPlan       string                          `json:"copilot_plan"`
	QuotaResetDate    string                          `json:"quota_reset_date"`
	QuotaResetDateUTC string                          `json:"quota_reset_date_utc"`
	QuotaSnapshots    map[string]copilotQuotaSnapshot `json:"quota_snapshots"`
}

type copilotQuotaSnapshot struct {
	Entitlement      flexFloat `json:"entitlement"`
	Remaining        flexFloat `json:"remaining"`
	PercentRemaining flexFloat `json:"percent_remaining"`
	QuotaID          string    `json:"quota_id"`
	Unlimited        bool      `json:"unlimited"`
}

type flexFloat struct {
	Value float64
	Valid bool
}

type currencyPair struct {
	Base  string
	Quote string
}

const (
	indexTemplatePath = "index.html"
	meterSegments     = 48
)

var defaultExchangePairs = []currencyPair{
	{Base: "BTC", Quote: "USD"},
	{Base: "USD", Quote: "CNY"},
	{Base: "AUD", Quote: "CNY"},
}

// expandPath resolves shell-style home and environment references in local file paths.
func expandPath(path string) string {
	if strings.HasPrefix(path, "~/") {
		home, _ := os.UserHomeDir()
		return filepath.Join(home, path[2:])
	}
	return os.ExpandEnv(path)
}

// loadConfig decodes the YAML configuration file into the server's runtime config.
func loadConfig(path string) (*Config, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	var cfg Config
	err = yaml.NewDecoder(f).Decode(&cfg)
	return &cfg, err
}

// fetchCodexStats loads the latest local Codex rate-limit snapshot and maps it into dashboard bars.
func fetchCodexStats() (AgentStats, error) {
	stats := AgentStats{Name: "CodeX", IsRunning: true}

	rateLimits, err := latestCodexRateLimits()
	if err != nil {
		return stats, err
	}

	if detail := codexDetail(rateLimits); detail != "" {
		stats.Detail = detail
	}

	stats.Bars = append(stats.Bars, codexBar("Primary", rateLimits.Primary))
	stats.Bars = append(stats.Bars, codexBar("Secondary", rateLimits.Secondary))
	return stats, nil
}

// fetchCopilotStats calls GitHub's Copilot usage endpoint and converts the response into dashboard bars.
func fetchCopilotStats() (AgentStats, error) {
	stats := AgentStats{Name: "GitHub Copilot", IsRunning: true}

	token, err := resolveCopilotToken()
	if err != nil {
		return stats, err
	}

	req, err := http.NewRequest("GET", "https://api.github.com/copilot_internal/user", nil)
	if err != nil {
		return stats, err
	}
	req.Header.Set("Authorization", "token "+token)
	req.Header.Set("Accept", "application/json")
	req.Header.Set("Editor-Version", "vscode/1.96.2")
	req.Header.Set("Editor-Plugin-Version", "copilot-chat/0.26.7")
	req.Header.Set("User-Agent", "GitHubCopilotChat/0.26.7")
	req.Header.Set("X-Github-Api-Version", "2025-04-01")

	client := &http.Client{Timeout: 10 * time.Second}
	resp, err := client.Do(req)
	if err != nil {
		return stats, fmt.Errorf("copilot request failed: %v", err)
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return stats, err
	}

	if resp.StatusCode == http.StatusUnauthorized || resp.StatusCode == http.StatusForbidden {
		return stats, fmt.Errorf("copilot token is invalid or expired")
	}
	if resp.StatusCode != http.StatusOK {
		return stats, fmt.Errorf("copilot request returned %s: %s", resp.Status, strings.TrimSpace(string(body)))
	}

	var usage copilotUsageResponse
	if err := json.Unmarshal(body, &usage); err != nil {
		return stats, fmt.Errorf("decode copilot usage: %v", err)
	}

	if usage.CopilotPlan != "" {
		stats.Detail = "Plan: " + titleCase(usage.CopilotPlan)
	}

	bars, err := copilotUsageBars(usage)
	if err != nil {
		return stats, err
	}
	stats.Bars = bars
	return stats, nil
}

// resolveCopilotToken finds a Copilot token from the environment first, then from the local Copilot app state.
func resolveCopilotToken() (string, error) {
	if token := strings.TrimSpace(os.Getenv("COPILOT_API_TOKEN")); token != "" {
		return token, nil
	}

	appsPath := expandPath("~/.config/github-copilot/apps.json")
	data, err := os.ReadFile(appsPath)
	if err != nil {
		if os.IsNotExist(err) {
			return "", fmt.Errorf("no Copilot token found; set COPILOT_API_TOKEN or sign in via GitHub Copilot")
		}
		return "", err
	}

	var apps map[string]copilotAppEntry
	if err := json.Unmarshal(data, &apps); err != nil {
		return "", fmt.Errorf("decode %s: %v", appsPath, err)
	}

	keys := make([]string, 0, len(apps))
	for key := range apps {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	for _, key := range keys {
		if token := strings.TrimSpace(apps[key].OAuthToken); token != "" {
			return token, nil
		}
	}

	return "", fmt.Errorf("no Copilot token found; set COPILOT_API_TOKEN or sign in via GitHub Copilot")
}

// copilotUsageBars selects the supported Copilot quota buckets and renders each one as a usage bar.
func copilotUsageBars(usage copilotUsageResponse) ([]UsageBar, error) {
	reset := copilotResetText(usage)
	var bars []UsageBar

	if snapshot := selectCopilotQuota(usage.QuotaSnapshots, "premium_interactions", "premium", "completions", "code"); snapshot != nil {
		if percent, ok := copilotUsedPercent(*snapshot); ok {
			bars = append(bars, UsageBar{
				Label:      "Premium",
				Percentage: percent,
				Reset:      reset,
			})
		}
	}

	if snapshot := selectCopilotQuota(usage.QuotaSnapshots, "chat"); snapshot != nil {
		if percent, ok := copilotUsedPercent(*snapshot); ok {
			bars = append(bars, UsageBar{
				Label:      "Chat",
				Percentage: percent,
				Reset:      reset,
			})
		}
	}

	if len(bars) == 0 {
		return nil, fmt.Errorf("no Copilot quota snapshots found")
	}
	return bars, nil
}

// selectCopilotQuota returns the first quota snapshot whose key or quota ID matches one of the requested names.
func selectCopilotQuota(quotas map[string]copilotQuotaSnapshot, names ...string) *copilotQuotaSnapshot {
	if len(quotas) == 0 {
		return nil
	}

	for _, name := range names {
		for key, snapshot := range quotas {
			if strings.EqualFold(key, name) || strings.EqualFold(snapshot.QuotaID, name) {
				return &snapshot
			}
		}
	}
	return nil
}

// copilotUsedPercent derives used quota percent from either percent-remaining or raw entitlement fields.
func copilotUsedPercent(snapshot copilotQuotaSnapshot) (int, bool) {
	switch {
	case snapshot.PercentRemaining.Valid:
		return clampPercent(100 - snapshot.PercentRemaining.Value), true
	case snapshot.Entitlement.Valid && snapshot.Entitlement.Value > 0 && snapshot.Remaining.Valid:
		return clampPercent(100 - ((snapshot.Remaining.Value / snapshot.Entitlement.Value) * 100)), true
	default:
		return 0, false
	}
}

// copilotResetText formats the Copilot quota reset timestamp using the first parseable reset field.
func copilotResetText(usage copilotUsageResponse) string {
	for _, value := range []string{usage.QuotaResetDateUTC, usage.QuotaResetDate} {
		value = strings.TrimSpace(value)
		if value == "" {
			continue
		}
		for _, layout := range []string{time.RFC3339Nano, time.RFC3339, "2006-01-02"} {
			if parsed, err := time.Parse(layout, value); err == nil {
				if layout == "2006-01-02" {
					return parsed.Format("2006-01-02")
				}
				return parsed.Local().Format("2006-01-02 15:04")
			}
		}
		return value
	}
	return "Unknown"
}

// titleCase converts dash, underscore, and space separated identifiers into title case for display.
func titleCase(value string) string {
	value = strings.TrimSpace(value)
	if value == "" {
		return ""
	}
	parts := strings.FieldsFunc(value, func(r rune) bool {
		return r == '-' || r == '_' || r == ' '
	})
	for i, part := range parts {
		lower := strings.ToLower(part)
		parts[i] = strings.ToUpper(lower[:1]) + lower[1:]
	}
	return strings.Join(parts, " ")
}

// clampPercent bounds percentage values to the dashboard's expected 0-100 range.
func clampPercent(value float64) int {
	if value < 0 {
		value = 0
	}
	if value > 100 {
		value = 100
	}
	return int(math.Round(value))
}

// summarizeAgent converts raw provider stats into the display-ready panel structure used by the template.
func summarizeAgent(stats AgentStats) ProviderPanel {
	panel := ProviderPanel{
		Name:   displayAgentName(stats.Name),
		Detail: strings.ToUpper(strings.TrimSpace(stats.Detail)),
	}

	if len(stats.Bars) == 0 {
		panel.Bars = []ProviderBar{{
			Label:       "STATUS",
			PercentText: "--",
			Segments:    meterFill(0, meterSegments),
			Meta:        "DATA UNAVAILABLE",
		}}
		return panel
	}

	panel.Bars = make([]ProviderBar, 0, len(stats.Bars))
	for _, bar := range stats.Bars {
		panel.Bars = append(panel.Bars, ProviderBar{
			Label:       strings.ToUpper(strings.TrimSpace(bar.Label)),
			PercentText: strconv.Itoa(bar.Percentage),
			Segments:    meterFill(bar.Percentage, meterSegments),
			Meta:        providerMeta(bar),
			Available:   true,
		})
	}
	return panel
}

// displayAgentName normalizes provider names into the compact labels used in the UI.
func displayAgentName(name string) string {
	switch strings.ToLower(strings.TrimSpace(name)) {
	case "github copilot":
		return "COPILOT"
	case "codex", "codex cli", "codex app", "codex desktop":
		return "CODEX"
	default:
		return strings.ToUpper(strings.TrimSpace(name))
	}
}

// providerMeta builds the small metadata label shown beneath each usage meter.
func providerMeta(bar UsageBar) string {
	reset := compactResetText(bar.Reset)
	switch {
	case reset != "":
		return "RESET " + reset
	default:
		return ""
	}
}

// compactResetText shortens reset timestamps so they fit the narrow Kindle layout.
func compactResetText(value string) string {
	value = strings.TrimSpace(value)
	if value == "" || value == "Unknown" {
		return ""
	}

	layouts := []string{
		"2006-01-02 15:04",
		time.RFC3339Nano,
		time.RFC3339,
		"2006-01-02",
	}

	for _, layout := range layouts {
		if parsed, err := time.Parse(layout, value); err == nil {
			local := parsed.Local()
			if local.Format("2006-01-02") == time.Now().Format("2006-01-02") {
				return local.Format("15:04")
			}
			return local.Format("01-02 15:04")
		}
	}

	return value
}

// meterFill expands a percent value into a fixed-width list of filled and empty meter segments.
func meterFill(percentage int, total int) []MeterSegment {
	segments := make([]MeterSegment, total)
	filled := int(math.Round((float64(clampPercent(float64(percentage))) / 100) * float64(total)))
	for i := 0; i < total; i++ {
		segments[i] = MeterSegment{Filled: i < filled}
	}
	return segments
}

// UnmarshalJSON accepts numeric quota fields that may arrive as either JSON numbers or strings.
func (f *flexFloat) UnmarshalJSON(data []byte) error {
	text := strings.TrimSpace(string(data))
	if text == "" || text == "null" {
		*f = flexFloat{}
		return nil
	}

	if number, err := strconv.ParseFloat(strings.Trim(text, `"`), 64); err == nil {
		f.Value = number
		f.Valid = true
		return nil
	}

	return fmt.Errorf("invalid number: %s", text)
}

// fetchExchangeRates collects the configured exchange rows while tolerating individual upstream failures.
func fetchExchangeRates() ([]ExchangeRateRow, error) {
	client := &http.Client{Timeout: 8 * time.Second}
	cache := make(map[string]map[string]float64)
	rows := make([]ExchangeRateRow, 0, len(defaultExchangePairs))

	var lastErr error
	for _, pair := range defaultExchangePairs {
		base := strings.ToLower(pair.Base)
		quote := strings.ToLower(pair.Quote)

		rates, ok := cache[base]
		if !ok {
			var err error
			rates, err = fetchBaseExchangeRates(client, base)
			if err != nil {
				lastErr = err
				continue
			}
			cache[base] = rates
		}

		value, found := rates[quote]
		if !found {
			lastErr = fmt.Errorf("missing %s/%s exchange rate", pair.Base, pair.Quote)
			continue
		}

		rows = append(rows, ExchangeRateRow{
			Pair:  strings.ToUpper(pair.Base + "/" + pair.Quote),
			Value: formatRateValue(value),
		})
	}

	if len(rows) == 0 {
		if lastErr == nil {
			lastErr = fmt.Errorf("no exchange rates available")
		}
		return nil, lastErr
	}

	return rows, lastErr
}

// fetchBaseExchangeRates retrieves all quote rates for one base currency from the fallback exchange endpoints.
func fetchBaseExchangeRates(client *http.Client, base string) (map[string]float64, error) {
	endpoints := []string{
		fmt.Sprintf("https://cdn.jsdelivr.net/npm/@fawazahmed0/currency-api@latest/v1/currencies/%s.json", base),
		fmt.Sprintf("https://latest.currency-api.pages.dev/v1/currencies/%s.json", base),
	}

	var lastErr error
	for _, endpoint := range endpoints {
		req, err := http.NewRequest("GET", endpoint, nil)
		if err != nil {
			return nil, err
		}
		req.Header.Set("Accept", "application/json")

		resp, err := client.Do(req)
		if err != nil {
			lastErr = err
			continue
		}

		body, readErr := io.ReadAll(resp.Body)
		resp.Body.Close()
		if readErr != nil {
			lastErr = readErr
			continue
		}
		if resp.StatusCode != http.StatusOK {
			lastErr = fmt.Errorf("exchange request returned %s", resp.Status)
			continue
		}

		var payload map[string]json.RawMessage
		if err := json.Unmarshal(body, &payload); err != nil {
			lastErr = fmt.Errorf("decode exchange payload: %v", err)
			continue
		}

		ratesPayload, ok := payload[base]
		if !ok {
			lastErr = fmt.Errorf("exchange payload missing %s base", base)
			continue
		}

		var rates map[string]float64
		if err := json.Unmarshal(ratesPayload, &rates); err != nil {
			lastErr = fmt.Errorf("decode %s rates: %v", base, err)
			continue
		}

		return rates, nil
	}

	if lastErr == nil {
		lastErr = fmt.Errorf("exchange request failed for %s", base)
	}
	return nil, lastErr
}

// formatRateValue chooses a precision that keeps each exchange value readable without wasting space.
func formatRateValue(value float64) string {
	abs := math.Abs(value)
	switch {
	case abs >= 1000:
		return formatGroupedFloat(value, 2)
	case abs >= 1:
		return fmt.Sprintf("%.4f", value)
	case abs >= 0.01:
		return fmt.Sprintf("%.6f", value)
	default:
		return fmt.Sprintf("%.8f", value)
	}
}

// formatGroupedFloat formats large numbers with thousands separators for compact display.
func formatGroupedFloat(value float64, precision int) string {
	text := fmt.Sprintf("%.*f", precision, value)
	parts := strings.SplitN(text, ".", 2)

	sign := ""
	integer := parts[0]
	if strings.HasPrefix(integer, "-") {
		sign = "-"
		integer = integer[1:]
	}

	for i := len(integer) - 3; i > 0; i -= 3 {
		integer = integer[:i] + "," + integer[i:]
	}

	if len(parts) == 2 {
		return sign + integer + "." + parts[1]
	}
	return sign + integer
}

// latestCodexRateLimits scans recent local Codex session files and returns the newest valid rate-limit payload.
func latestCodexRateLimits() (codexRateLimits, error) {
	var zero codexRateLimits
	roots := []string{
		expandPath("~/.codex/sessions"),
		expandPath("~/.codex/archived_sessions"),
	}

	type fileInfo struct {
		path    string
		modTime time.Time
	}

	var files []fileInfo
	for _, root := range roots {
		err := filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
			if err != nil {
				return nil
			}
			if d.IsDir() || filepath.Ext(path) != ".jsonl" {
				return nil
			}
			info, statErr := d.Info()
			if statErr != nil {
				return nil
			}
			files = append(files, fileInfo{path: path, modTime: info.ModTime()})
			return nil
		})
		if err != nil && !os.IsNotExist(err) {
			return zero, err
		}
	}

	if len(files) == 0 {
		return zero, fmt.Errorf("no Codex session files found")
	}

	sort.Slice(files, func(i, j int) bool {
		return files[i].modTime.After(files[j].modTime)
	})

	for _, file := range files {
		rateLimits, err := codexRateLimitsFromFile(file.path)
		if err == nil {
			return rateLimits, nil
		}
	}

	return zero, fmt.Errorf("no Codex rate limits found in recent sessions")
}

// codexRateLimitsFromFile extracts the last usable token-count rate-limit event from one session file.
func codexRateLimitsFromFile(path string) (codexRateLimits, error) {
	var zero codexRateLimits

	f, err := os.Open(path)
	if err != nil {
		return zero, err
	}
	defer f.Close()

	scanner := bufio.NewScanner(f)
	buf := make([]byte, 0, 64*1024)
	scanner.Buffer(buf, 1024*1024)

	var last codexRateLimits
	found := false

	for scanner.Scan() {
		line := scanner.Bytes()
		var event codexSessionEvent
		if err := json.Unmarshal(line, &event); err != nil {
			continue
		}
		if event.Type != "event_msg" || event.Payload.Type != "token_count" {
			continue
		}
		if event.Payload.RateLimits.Primary.WindowMinutes == 0 && event.Payload.RateLimits.Secondary.WindowMinutes == 0 {
			continue
		}
		last = event.Payload.RateLimits
		found = true
	}

	if err := scanner.Err(); err != nil {
		return zero, err
	}
	if !found {
		return zero, fmt.Errorf("no token_count events in %s", path)
	}
	return last, nil
}

// codexDetail assembles the optional Codex plan and credit summary line for the UI.
func codexDetail(rateLimits codexRateLimits) string {
	var details []string
	if rateLimits.PlanType != "" {
		details = append(details, "Plan: "+strings.ToUpper(rateLimits.PlanType[:1])+rateLimits.PlanType[1:])
	}
	if rateLimits.Credits != nil {
		switch {
		case rateLimits.Credits.Unlimited:
			details = append(details, "Credits: Unlimited")
		case rateLimits.Credits.Balance != "":
			details = append(details, "Credits: "+rateLimits.Credits.Balance)
		}
	}
	return strings.Join(details, " | ")
}

// codexBar converts a Codex limit window into the shared usage-bar format used by the dashboard.
func codexBar(fallbackLabel string, limit codexRateLimitEntry) UsageBar {
	return UsageBar{
		Label:      codexWindowLabel(fallbackLabel, limit.WindowMinutes),
		Percentage: int(math.Round(limit.UsedPercent)),
		Reset:      codexResetText(limit),
	}
}

// codexWindowLabel maps common Codex window lengths to human-friendly labels.
func codexWindowLabel(fallbackLabel string, windowMinutes int) string {
	switch windowMinutes {
	case 300:
		return "5h Limit"
	case 10080:
		return "Weekly Limit"
	default:
		if windowMinutes > 0 {
			return fmt.Sprintf("%s (%dm)", fallbackLabel, windowMinutes)
		}
		return fallbackLabel
	}
}

// codexResetText formats a Codex reset timestamp from either an absolute or relative reset field.
func codexResetText(limit codexRateLimitEntry) string {
	switch {
	case limit.ResetsAt > 0:
		return time.Unix(limit.ResetsAt, 0).Local().Format("2006-01-02 15:04")
	case limit.ResetsInSeconds > 0:
		return time.Now().Add(time.Duration(limit.ResetsInSeconds) * time.Second).Format("2006-01-02 15:04")
	default:
		return "Unknown"
	}
}

// loadIndexTemplate finds and parses the dashboard HTML template from the working directory or binary directory.
func loadIndexTemplate() (*template.Template, error) {
	candidates := []string{indexTemplatePath}
	if executablePath, err := os.Executable(); err == nil {
		candidates = append(candidates, filepath.Join(filepath.Dir(executablePath), indexTemplatePath))
	}

	for _, candidate := range candidates {
		if _, err := os.Stat(candidate); err != nil {
			if errors.Is(err, fs.ErrNotExist) {
				continue
			}
			return nil, fmt.Errorf("error accessing template %q: %w", candidate, err)
		}

		tmpl, err := template.ParseFiles(candidate)
		if err != nil {
			return nil, fmt.Errorf("error parsing template %q: %w", candidate, err)
		}
		return tmpl, nil
	}

	return nil, fmt.Errorf("template file not found: %s", indexTemplatePath)
}

// main parses startup flags, validates configuration, and serves the single-page dashboard.
func main() {
	configPath := flag.String("config", "config.yaml", "Path to config file")
	flag.Parse()

	if flag.NArg() > 0 && flag.Arg(0) == "start" {
		cfg, err := loadConfig(*configPath)
		if err != nil {
			log.Fatalf("Error loading config: %v", err)
		}

		if _, err := loadIndexTemplate(); err != nil {
			log.Fatalf("Error loading template: %v", err)
		}

		handler := func(w http.ResponseWriter, r *http.Request) {
			tmpl, err := loadIndexTemplate()
			if err != nil {
				log.Printf("Template Error: %v", err)
				http.Error(w, "template load failed", http.StatusInternalServerError)
				return
			}

			var agents []AgentStats
			if cfg.Agents.Copilot.Enabled {
				s, err := fetchCopilotStats()
				if err != nil {
					log.Printf("Copilot Error: %v", err)
				}
				agents = append(agents, s)
			}
			if cfg.Agents.Codex.Enabled {
				s, err := fetchCodexStats()
				if err != nil {
					log.Printf("Codex Error: %v", err)
				}
				agents = append(agents, s)
			}

			rates, err := fetchExchangeRates()
			if err != nil {
				log.Printf("Rates Error: %v", err)
			}

			providers := make([]ProviderPanel, 0, len(agents))
			for _, agent := range agents {
				providers = append(providers, summarizeAgent(agent))
			}

			data := DashboardData{
				Providers: providers,
				Rates:     rates,
				Time:      time.Now().Format("15:04:05"),
				Runtime:   "RUNTIME_01",
			}
			if err := tmpl.Execute(w, data); err != nil {
				log.Printf("Template Execute Error: %v", err)
			}
		}

		http.HandleFunc("/", handler)
		addr := fmt.Sprintf(":%d", cfg.Server.Port)
		fmt.Printf("Starting KindleVibe server on http://localhost%s\n", addr)
		if err := http.ListenAndServe(addr, nil); err != nil {
			log.Fatal(err)
		}
	} else {
		fmt.Println("Usage: kindlevibe start [--config config.yaml]")
	}
}
