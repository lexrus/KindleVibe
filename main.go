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
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"gopkg.in/yaml.v3"
)

type Config struct {
	Server struct {
		Port int `yaml:"port"`
	} `yaml:"server"`
	Agents struct {
		Copilot struct {
			Enabled bool   `yaml:"enabled"`
			Token   string `yaml:"token"`
		} `yaml:"copilot"`
		Codex struct {
			Enabled bool `yaml:"enabled"`
		} `yaml:"codex"`
		Linear struct {
			Enabled bool   `yaml:"enabled"`
			Token   string `yaml:"token"`
		} `yaml:"linear"`
		Gemini struct {
			Enabled bool `yaml:"enabled"`
		} `yaml:"gemini"`
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
	Lists     []AgentList
	Detail    string
	IsRunning bool
}

type ListItem struct {
	Identifier  string
	Text        string
	Detail      string
	IssueID     string
	TeamName    string
	StateName   string
	Priority    string
	CreatedAt   string
	UpdatedAt   string
	URL         string
	Description string
}

type AgentList struct {
	Title string
	Items []ListItem
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
	Lists  []ProviderList
}

type ProviderList struct {
	Title string
	Items []ListItem
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

type dashboardCache struct {
	mu         sync.RWMutex
	data       DashboardData
	completed  map[string]time.Time
	fetchedAt  time.Time
	refreshing bool
	ready      chan struct{}
}

func (c *dashboardCache) markLinearIssueDone(issueID string) {
	issueID = strings.TrimSpace(issueID)
	if issueID == "" {
		return
	}

	c.mu.Lock()
	defer c.mu.Unlock()

	if c.completed == nil {
		c.completed = make(map[string]time.Time)
	}
	now := time.Now()
	c.completed[issueID] = now.Add(linearCompletedCacheTTL)
	c.filterCompletedLinearLocked(&c.data, now)
	c.fetchedAt = now
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

type linearGraphQLRequest struct {
	Query     string         `json:"query"`
	Variables map[string]any `json:"variables,omitempty"`
}

type linearTransitionResponse struct {
	Data struct {
		IssueUpdate struct {
			Success bool `json:"success"`
		} `json:"issueUpdate"`
	} `json:"data"`
	Errors []struct {
		Message string `json:"message"`
	} `json:"errors"`
}

type linearGraphQLResponse struct {
	Data struct {
		Viewer struct {
			Name string `json:"name"`
		} `json:"viewer"`
		Todo       linearIssueConnection `json:"todo"`
		InProgress linearIssueConnection `json:"inProgress"`
	} `json:"data"`
	Errors []struct {
		Message string `json:"message"`
	} `json:"errors"`
}

type linearIssueConnection struct {
	Nodes []linearIssueNode `json:"nodes"`
}

type linearIssueNode struct {
	ID          string  `json:"id"`
	Identifier  string  `json:"identifier"`
	Title       string  `json:"title"`
	Description string  `json:"description"`
	Priority    float64 `json:"priority"`
	URL         string  `json:"url"`
	UpdatedAt   string  `json:"updatedAt"`
	CreatedAt   string  `json:"createdAt"`
	State       struct {
		Name string `json:"name"`
	} `json:"state"`
	Team struct {
		Name string `json:"name"`
	} `json:"team"`
}

const (
	indexTemplatePath       = "index.html"
	meterSegments           = 10
	linearListMax           = 10
	linearCompletedCacheTTL = time.Minute
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
func fetchCopilotStats(cfg *Config) (AgentStats, error) {
	stats := AgentStats{Name: "GitHub Copilot", IsRunning: true}

	token, err := resolveCopilotToken(cfg.Agents.Copilot.Token)
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
		stats.Detail = titleCase(usage.CopilotPlan)
	}

	bars, err := copilotUsageBars(usage)
	if err != nil {
		return stats, err
	}
	stats.Bars = bars
	return stats, nil
}

// fetchLinearStats calls Linear GraphQL and returns compact Todo/In Progress issue lists for the current assignee.
func fetchLinearStats(cfg *Config) (AgentStats, error) {
	stats := AgentStats{Name: "Linear", IsRunning: true}

	token, err := resolveLinearToken(cfg.Agents.Linear.Token)
	if err != nil {
		return stats, err
	}

	query := `query KindleVibeLinearIssues($first: Int!) {
  todo: issues(
    first: $first
    orderBy: updatedAt
    filter: {assignee: {isMe: {eq: true}}, state: {type: {eq: "unstarted"}}}
  ) {
    nodes {
      id
      identifier
      title
      description
      priority
      url
      updatedAt
      createdAt
      state {
        name
      }
      team {
        name
      }
    }
  }
  inProgress: issues(
    first: $first
    orderBy: updatedAt
    filter: {assignee: {isMe: {eq: true}}, state: {type: {eq: "started"}}}
  ) {
    nodes {
      id
      identifier
      title
      description
      priority
      url
      updatedAt
      createdAt
      state {
        name
      }
      team {
        name
      }
    }
  }
}`

	payload := linearGraphQLRequest{
		Query: query,
		Variables: map[string]any{
			"first": linearListMax,
		},
	}

	body, err := json.Marshal(payload)
	if err != nil {
		return stats, err
	}

	req, err := http.NewRequest("POST", "https://api.linear.app/graphql", strings.NewReader(string(body)))
	if err != nil {
		return stats, err
	}
	req.Header.Set("Authorization", token)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json")

	client := &http.Client{Timeout: 10 * time.Second}
	resp, err := client.Do(req)
	if err != nil {
		return stats, fmt.Errorf("linear request failed: %v", err)
	}
	defer resp.Body.Close()

	responseBody, err := io.ReadAll(resp.Body)
	if err != nil {
		return stats, err
	}

	if resp.StatusCode == http.StatusUnauthorized || resp.StatusCode == http.StatusForbidden {
		return stats, fmt.Errorf("linear token is invalid or expired")
	}
	if resp.StatusCode != http.StatusOK {
		return stats, fmt.Errorf("linear request returned %s: %s", resp.Status, strings.TrimSpace(string(responseBody)))
	}

	var result linearGraphQLResponse
	if err := json.Unmarshal(responseBody, &result); err != nil {
		return stats, fmt.Errorf("decode linear response: %v", err)
	}
	if len(result.Errors) > 0 {
		return stats, fmt.Errorf("linear query error: %s", strings.TrimSpace(result.Errors[0].Message))
	}

	// Detail intentionally left blank – assignee is not shown.

	todoNodes := result.Data.Todo.Nodes
	inProgressNodes := result.Data.InProgress.Nodes

	todoLimit := 5
	inProgressLimit := 5

	if len(todoNodes) > 0 && len(inProgressNodes) > 0 {
		todoLimit = 4
		inProgressLimit = 4
	}

	if len(inProgressNodes) > 0 {
		stats.Lists = append(stats.Lists, linearIssueList("In Progress", inProgressNodes, inProgressLimit))
	}
	if len(todoNodes) > 0 {
		stats.Lists = append(stats.Lists, linearIssueList("Todo", todoNodes, todoLimit))
	}

	if len(stats.Lists) == 0 {
		stats.Lists = append(stats.Lists, linearIssueList("Todo", nil, 5))
	}

	return stats, nil
}

type geminiQuotaBucket struct {
	RemainingFraction float64 `json:"remainingFraction"`
	ResetTime         string  `json:"resetTime"`
	ModelID           string  `json:"modelId"`
}

type geminiQuotaResponse struct {
	Buckets []geminiQuotaBucket `json:"buckets"`
}

type geminiModelQuota struct {
	ModelID          string
	PercentLeft      float64
	ResetDescription string
}

type geminiOAuthCredentials struct {
	AccessToken  string  `json:"access_token"`
	RefreshToken string  `json:"refresh_token"`
	IDToken      string  `json:"id_token"`
	ExpiryDate   float64 `json:"expiry_date"`
}

type geminiRefreshResponse struct {
	AccessToken string    `json:"access_token"`
	ExpiresIn   flexFloat `json:"expires_in"`
	IDToken     string    `json:"id_token"`
}

// fetchGeminiStats reads Gemini CLI OAuth credentials and fetches user quota via Google Cloud Code API.
func fetchGeminiStats() (AgentStats, error) {
	stats := AgentStats{Name: "Gemini", IsRunning: true}

	home, err := os.UserHomeDir()
	if err != nil {
		return stats, err
	}

	credsPath := filepath.Join(home, ".gemini", "oauth_creds.json")
	data, err := os.ReadFile(credsPath)
	if err != nil {
		return stats, fmt.Errorf("gemini credentials not found: %w", err)
	}

	var creds geminiOAuthCredentials
	if err := json.Unmarshal(data, &creds); err != nil {
		return stats, fmt.Errorf("decode gemini credentials: %w", err)
	}

	if creds.AccessToken == "" {
		return stats, fmt.Errorf("gemini access token missing")
	}

	accessToken := creds.AccessToken
	if creds.ExpiryDate > 0 && float64(time.Now().UnixMilli()) > creds.ExpiryDate {
		if creds.RefreshToken == "" {
			return stats, fmt.Errorf("gemini access token expired")
		}
		refreshedToken, err := refreshGeminiAccessToken(credsPath, creds.RefreshToken, 10*time.Second)
		if err != nil {
			return stats, err
		}
		accessToken = refreshedToken
	}

	projectID := loadGeminiProjectID(accessToken, 10*time.Second)
	body := []byte("{}")
	if projectID != "" {
		if payload, err := json.Marshal(map[string]string{"project": projectID}); err == nil {
			body = payload
		}
	}

	req, err := http.NewRequest("POST", "https://cloudcode-pa.googleapis.com/v1internal:retrieveUserQuota", strings.NewReader(string(body)))
	if err != nil {
		return stats, err
	}
	req.Header.Set("Authorization", "Bearer "+accessToken)
	req.Header.Set("Content-Type", "application/json")

	client := &http.Client{Timeout: 10 * time.Second}
	resp, err := client.Do(req)
	if err != nil {
		return stats, err
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return stats, fmt.Errorf("gemini api status: %s", resp.Status)
	}

	var quotaResp geminiQuotaResponse
	if err := json.NewDecoder(resp.Body).Decode(&quotaResp); err != nil {
		return stats, fmt.Errorf("decode gemini quota: %w", err)
	}

	proMin := selectGeminiQuotaBucket(quotaResp.Buckets, func(id string) bool {
		return strings.Contains(id, "pro")
	})
	flashMin := selectGeminiQuotaBucket(quotaResp.Buckets, func(id string) bool {
		return strings.Contains(id, "flash") && !strings.Contains(id, "flash-lite")
	})
	flashLiteMin := selectGeminiQuotaBucket(quotaResp.Buckets, func(id string) bool {
		return strings.Contains(id, "flash-lite")
	})

	if proMin != nil {
		stats.Bars = append(stats.Bars, UsageBar{
			Label:      "Pro",
			Percentage: clampPercent((1.0 - proMin.RemainingFraction) * 100),
			Reset:      formatGeminiReset(proMin.ResetTime),
		})
	}
	if flashMin != nil {
		stats.Bars = append(stats.Bars, UsageBar{
			Label:      "Flash",
			Percentage: clampPercent((1.0 - flashMin.RemainingFraction) * 100),
			Reset:      formatGeminiReset(flashMin.ResetTime),
		})
	}
	if flashLiteMin != nil {
		stats.Bars = append(stats.Bars, UsageBar{
			Label:      "Flash Lite",
			Percentage: clampPercent((1.0 - flashLiteMin.RemainingFraction) * 100),
			Reset:      formatGeminiReset(flashLiteMin.ResetTime),
		})
	}

	return stats, nil
}

func refreshGeminiAccessToken(credsPath string, refreshToken string, timeout time.Duration) (string, error) {
	oauthCreds, err := extractGeminiOAuthClientCredentials()
	if err != nil {
		return "", err
	}

	form := url.Values{}
	form.Set("client_id", oauthCreds.ClientID)
	form.Set("client_secret", oauthCreds.ClientSecret)
	form.Set("refresh_token", refreshToken)
	form.Set("grant_type", "refresh_token")

	req, err := http.NewRequest("POST", "https://oauth2.googleapis.com/token", strings.NewReader(form.Encode()))
	if err != nil {
		return "", err
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")

	client := &http.Client{Timeout: timeout}
	resp, err := client.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return "", err
	}

	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("gemini token refresh failed: %s", resp.Status)
	}

	var refreshResp geminiRefreshResponse
	if err := json.Unmarshal(body, &refreshResp); err != nil {
		return "", fmt.Errorf("decode gemini refresh response: %w", err)
	}
	if refreshResp.AccessToken == "" {
		return "", fmt.Errorf("gemini token refresh returned no access token")
	}

	if err := updateGeminiCredentials(credsPath, refreshResp); err != nil {
		log.Printf("Gemini Warning: could not update credentials: %v", err)
	}

	return refreshResp.AccessToken, nil
}

type geminiOAuthClientCredentials struct {
	ClientID     string
	ClientSecret string
}

func extractGeminiOAuthClientCredentials() (geminiOAuthClientCredentials, error) {
	binaryPath, err := exec.LookPath("gemini")
	if err != nil {
		return geminiOAuthClientCredentials{}, fmt.Errorf("gemini CLI not found on PATH: %w", err)
	}

	realPath := binaryPath
	if resolved, err := filepath.EvalSymlinks(binaryPath); err == nil && resolved != "" {
		realPath = resolved
	}

	binDir := filepath.Dir(realPath)
	baseDir := filepath.Dir(binDir)
	oauthFile := "dist/src/code_assist/oauth2.js"

	candidates := []string{
		filepath.Join(baseDir, "libexec/lib/node_modules/@google/gemini-cli/node_modules/@google/gemini-cli-core", oauthFile),
		filepath.Join(baseDir, "lib/node_modules/@google/gemini-cli/node_modules/@google/gemini-cli-core", oauthFile),
		filepath.Join(baseDir, "share/gemini-cli/node_modules/@google/gemini-cli-core", oauthFile),
		filepath.Join(baseDir, "../gemini-cli-core", oauthFile),
		filepath.Join(baseDir, "node_modules/@google/gemini-cli-core", oauthFile),
	}

	for _, candidate := range candidates {
		content, err := os.ReadFile(filepath.Clean(candidate))
		if err != nil {
			continue
		}
		if creds, ok := parseGeminiOAuthClientCredentials(string(content)); ok {
			return creds, nil
		}
	}

	if creds, ok := scanGeminiBundleForOAuthCredentials(binDir); ok {
		return creds, nil
	}

	return geminiOAuthClientCredentials{}, fmt.Errorf("gemini OAuth client credentials not found")
}

func scanGeminiBundleForOAuthCredentials(bundleDir string) (geminiOAuthClientCredentials, bool) {
	info, err := os.Stat(bundleDir)
	if err != nil || !info.IsDir() {
		return geminiOAuthClientCredentials{}, false
	}

	var found geminiOAuthClientCredentials
	walkErr := filepath.WalkDir(bundleDir, func(path string, d os.DirEntry, err error) error {
		if err != nil {
			return nil
		}
		if d.IsDir() || !strings.HasSuffix(strings.ToLower(d.Name()), ".js") {
			return nil
		}
		content, err := os.ReadFile(path)
		if err != nil {
			return nil
		}
		if creds, ok := parseGeminiOAuthClientCredentials(string(content)); ok {
			found = creds
			return fs.SkipAll
		}
		return nil
	})
	if walkErr != nil && !errors.Is(walkErr, fs.SkipAll) {
		return geminiOAuthClientCredentials{}, false
	}
	if found.ClientID != "" && found.ClientSecret != "" {
		return found, true
	}
	return geminiOAuthClientCredentials{}, false
}

func parseGeminiOAuthClientCredentials(content string) (geminiOAuthClientCredentials, bool) {
	clientIDPattern := `OAUTH_CLIENT_ID\s*=\s*['"]([\w\-\.]+)['"]\s*;`
	clientSecretPattern := `OAUTH_CLIENT_SECRET\s*=\s*['"]([\w\-]+)['"]\s*;`

	clientIDRegex, err := regexp.Compile(clientIDPattern)
	if err != nil {
		return geminiOAuthClientCredentials{}, false
	}
	clientSecretRegex, err := regexp.Compile(clientSecretPattern)
	if err != nil {
		return geminiOAuthClientCredentials{}, false
	}

	clientIDMatch := clientIDRegex.FindStringSubmatch(content)
	clientSecretMatch := clientSecretRegex.FindStringSubmatch(content)
	if len(clientIDMatch) < 2 || len(clientSecretMatch) < 2 {
		return geminiOAuthClientCredentials{}, false
	}

	return geminiOAuthClientCredentials{
		ClientID:     clientIDMatch[1],
		ClientSecret: clientSecretMatch[1],
	}, true
}

func updateGeminiCredentials(credsPath string, refreshResp geminiRefreshResponse) error {
	existing, err := os.ReadFile(credsPath)
	if err != nil {
		return err
	}

	var jsonData map[string]any
	if err := json.Unmarshal(existing, &jsonData); err != nil {
		return err
	}

	jsonData["access_token"] = refreshResp.AccessToken
	if refreshResp.IDToken != "" {
		jsonData["id_token"] = refreshResp.IDToken
	}
	if refreshResp.ExpiresIn.Valid {
		jsonData["expiry_date"] = (time.Now().Add(time.Duration(refreshResp.ExpiresIn.Value) * time.Second).UnixMilli())
	}

	updated, err := json.MarshalIndent(jsonData, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(credsPath, append(updated, '\n'), 0o600)
}

func loadGeminiProjectID(accessToken string, timeout time.Duration) string {
	if projectID := loadGeminiCodeAssistProjectID(accessToken, timeout); projectID != "" {
		return projectID
	}
	return discoverGeminiProjectID(accessToken, timeout)
}

func loadGeminiCodeAssistProjectID(accessToken string, timeout time.Duration) string {
	req, err := http.NewRequest("POST", "https://cloudcode-pa.googleapis.com/v1internal:loadCodeAssist", strings.NewReader(`{"metadata":{"ideType":"GEMINI_CLI","pluginType":"GEMINI"}}`))
	if err != nil {
		return ""
	}
	req.Header.Set("Authorization", "Bearer "+accessToken)
	req.Header.Set("Content-Type", "application/json")

	client := &http.Client{Timeout: timeout}
	resp, err := client.Do(req)
	if err != nil {
		return ""
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return ""
	}

	var payload map[string]any
	if err := json.NewDecoder(resp.Body).Decode(&payload); err != nil {
		return ""
	}

	if project, ok := payload["cloudaicompanionProject"].(string); ok {
		if project = strings.TrimSpace(project); project != "" {
			return project
		}
	}
	if project, ok := payload["cloudaicompanionProject"].(map[string]any); ok {
		if id, ok := project["id"].(string); ok {
			if id = strings.TrimSpace(id); id != "" {
				return id
			}
		}
		if id, ok := project["projectId"].(string); ok {
			if id = strings.TrimSpace(id); id != "" {
				return id
			}
		}
	}

	return ""
}

func discoverGeminiProjectID(accessToken string, timeout time.Duration) string {
	req, err := http.NewRequest("GET", "https://cloudresourcemanager.googleapis.com/v1/projects", nil)
	if err != nil {
		return ""
	}
	req.Header.Set("Authorization", "Bearer "+accessToken)
	req.Header.Set("Accept", "application/json")

	client := &http.Client{Timeout: timeout}
	resp, err := client.Do(req)
	if err != nil {
		return ""
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return ""
	}

	var payload struct {
		Projects []map[string]any `json:"projects"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&payload); err != nil {
		return ""
	}

	for _, project := range payload.Projects {
		projectID, _ := project["projectId"].(string)
		projectID = strings.TrimSpace(projectID)
		if projectID == "" {
			continue
		}
		if strings.HasPrefix(projectID, "gen-lang-client") {
			return projectID
		}
		if labels, ok := project["labels"].(map[string]any); ok && labels["generative-language"] != nil {
			return projectID
		}
	}

	return ""
}

func selectGeminiQuotaBucket(buckets []geminiQuotaBucket, match func(string) bool) *geminiQuotaBucket {
	var selected *geminiQuotaBucket
	for i := range buckets {
		b := &buckets[i]
		id := strings.ToLower(strings.TrimSpace(b.ModelID))
		if id == "" || !match(id) {
			continue
		}
		if selected == nil || b.RemainingFraction < selected.RemainingFraction {
			selected = b
		}
	}
	return selected
}

func formatGeminiReset(iso string) string {
	if iso == "" {
		return ""
	}
	// Try parsing with fractional seconds first (standard for this API)
	layouts := []string{time.RFC3339Nano, time.RFC3339}
	for _, layout := range layouts {
		if t, err := time.Parse(layout, iso); err == nil {
			return t.Local().Format("2006-01-02 15:04")
		}
	}
	return iso
}

// resolveLinearToken finds a Linear API token from local environment variables.
func resolveLinearToken(configToken string) (string, error) {
	if token := strings.TrimSpace(configToken); token != "" {
		return token, nil
	}
	for _, key := range []string{"LINEAR_API_TOKEN", "LINEAR_TOKEN"} {
		if token := strings.TrimSpace(os.Getenv(key)); token != "" {
			return token, nil
		}
	}
	return "", fmt.Errorf("no Linear token found; set agents.linear.token or LINEAR_API_TOKEN")
}

// linearIssueList converts Linear issues into compact rows suitable for Kindle width.
func linearIssueList(title string, nodes []linearIssueNode, max int) AgentList {
	list := AgentList{Title: title}
	for _, node := range nodes {
		text := strings.TrimSpace(node.Title)
		if id := strings.TrimSpace(node.Identifier); id != "" {
			text = id + " " + text
		}
		if text == "" {
			continue
		}

		issueID := strings.TrimSpace(node.Identifier)
		teamName := strings.TrimSpace(node.Team.Name)
		stateName := strings.TrimSpace(node.State.Name)
		priority := ""
		if node.Priority > 0 {
			priority = fmt.Sprintf("%.0f", node.Priority)
		}
		createdAt := formatGeminiReset(strings.TrimSpace(node.CreatedAt))
		updatedAt := formatGeminiReset(strings.TrimSpace(node.UpdatedAt))
		url := strings.TrimSpace(node.URL)
		description := strings.TrimSpace(node.Description)

		detailParts := []string{}
		if issueID != "" {
			detailParts = append(detailParts, "Issue: "+issueID)
		}
		if teamName != "" {
			detailParts = append(detailParts, "Team: "+teamName)
		}
		if stateName != "" {
			detailParts = append(detailParts, "State: "+stateName)
		}
		if priority != "" {
			detailParts = append(detailParts, "Priority: "+priority)
		}
		if createdAt != "" {
			detailParts = append(detailParts, "Created: "+createdAt)
		}
		if updatedAt != "" {
			detailParts = append(detailParts, "Updated: "+updatedAt)
		}
		if url != "" {
			detailParts = append(detailParts, "URL: "+url)
		}
		if description != "" {
			detailParts = append(detailParts, "Description: "+description)
		}

		detailText := strings.Join(detailParts, "\n")
		if detailText == "" {
			detailText = "No additional details available."
		}

		list.Items = append(list.Items, ListItem{
			Identifier:  strings.TrimSpace(node.ID),
			Text:        truncateText(text, 54),
			Detail:      detailText,
			IssueID:     issueID,
			TeamName:    teamName,
			StateName:   stateName,
			Priority:    priority,
			CreatedAt:   createdAt,
			UpdatedAt:   updatedAt,
			URL:         url,
			Description: description,
		})
		if len(list.Items) >= max {
			break
		}
	}
	if len(list.Items) == 0 {
		list.Items = []ListItem{{Text: "(none)"}}
	}
	return list
}

// truncateText clips long lines to a fixed width and appends an ellipsis marker.
func truncateText(text string, maxLen int) string {
	text = strings.TrimSpace(text)
	if maxLen <= 0 || len(text) <= maxLen {
		return text
	}
	if maxLen <= 3 {
		return text[:maxLen]
	}
	return text[:maxLen-3] + "..."
}

// resolveCopilotToken finds a Copilot token from the environment first, then from the local Copilot app state.
func resolveCopilotToken(configToken string) (string, error) {
	if token := strings.TrimSpace(configToken); token != "" {
		return token, nil
	}
	if token := strings.TrimSpace(os.Getenv("COPILOT_API_TOKEN")); token != "" {
		return token, nil
	}

	appsPath := expandPath("~/.config/github-copilot/apps.json")
	data, err := os.ReadFile(appsPath)
	if err != nil {
		if os.IsNotExist(err) {
			return "", fmt.Errorf("no Copilot token found; set agents.copilot.token, COPILOT_API_TOKEN, or sign in via GitHub Copilot")
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

	return "", fmt.Errorf("no Copilot token found; set agents.copilot.token, COPILOT_API_TOKEN, or sign in via GitHub Copilot")
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

	if len(stats.Bars) == 0 && len(stats.Lists) == 0 {
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

	panel.Lists = make([]ProviderList, 0, len(stats.Lists))
	for _, list := range stats.Lists {
		title := strings.ToUpper(strings.TrimSpace(list.Title))
		if title == "" {
			continue
		}

		items := make([]ListItem, 0, len(list.Items))
		for _, item := range list.Items {
			text := strings.TrimSpace(item.Text)
			if text == "" {
				continue
			}
			item.Text = text
			items = append(items, item)
		}
		if len(items) == 0 {
			items = []ListItem{{Text: "(none)"}}
		}

		panel.Lists = append(panel.Lists, ProviderList{Title: title, Items: items})
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
	case "linear":
		return "LINEAR"
	case "gemini":
		return "GEMINI"
	default:
		return strings.ToUpper(strings.TrimSpace(name))
	}
}

// providerMeta builds the small metadata label shown beneath each usage meter.
func providerMeta(bar UsageBar) string {
	reset := compactResetText(bar.Reset)
	switch {
	case reset != "":
		return "↻ " + reset
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
	if total <= 0 {
		total = meterSegments
	}
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
		details = append(details, strings.ToUpper(rateLimits.PlanType[:1])+rateLimits.PlanType[1:])
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

// linearDoneHandler returns an HTTP handler that transitions a Linear issue to the "Done" state.
func linearDoneHandler(cfg *Config, cache *dashboardCache) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}

		issueID := strings.TrimSpace(r.FormValue("id"))
		if issueID == "" {
			http.Error(w, "missing issue id", http.StatusBadRequest)
			return
		}

		token, err := resolveLinearToken(cfg.Agents.Linear.Token)
		if err != nil {
			log.Printf("Linear Done Error: %v", err)
			http.Redirect(w, r, "/", http.StatusSeeOther)
			return
		}

		doneStateID, err := linearDoneStateID(token, issueID)
		if err != nil {
			log.Printf("Linear Done Error: %v", err)
			http.Redirect(w, r, "/", http.StatusSeeOther)
			return
		}

		mutation := linearGraphQLRequest{
			Query: `mutation MarkDone($issueId: String!, $stateId: String!) {
  issueUpdate(id: $issueId, input: {stateId: $stateId}) {
    success
  }
}`,
			Variables: map[string]any{
				"issueId": issueID,
				"stateId": doneStateID,
			},
		}

		body, _ := json.Marshal(mutation)
		req, err := http.NewRequest("POST", "https://api.linear.app/graphql", strings.NewReader(string(body)))
		if err != nil {
			log.Printf("Linear Done Error: %v", err)
			http.Redirect(w, r, "/", http.StatusSeeOther)
			return
		}
		req.Header.Set("Authorization", token)
		req.Header.Set("Content-Type", "application/json")

		client := &http.Client{Timeout: 10 * time.Second}
		resp, err := client.Do(req)
		if err != nil {
			log.Printf("Linear Done Error: %v", err)
			http.Redirect(w, r, "/", http.StatusSeeOther)
			return
		}
		defer resp.Body.Close()

		respBody, _ := io.ReadAll(resp.Body)
		var result linearTransitionResponse
		if err := json.Unmarshal(respBody, &result); err != nil {
			log.Printf("Linear Done Error: decode: %v", err)
		} else if len(result.Errors) > 0 {
			log.Printf("Linear Done Error: %s", result.Errors[0].Message)
		} else if result.Data.IssueUpdate.Success {
			log.Printf("Linear: marked issue %s as Done", issueID)
			cache.markLinearIssueDone(issueID)
			go cache.startRefresh(cfg)
		}

		http.Redirect(w, r, "/", http.StatusSeeOther)
	}
}

// linearDoneStateID finds the ID of the "Done" (completed-type) workflow state for the team that owns the given issue.
func linearDoneStateID(token string, issueID string) (string, error) {
	query := linearGraphQLRequest{
		Query: `query DoneState($issueId: String!) {
  issue(id: $issueId) {
    team {
      states {
        nodes {
          id
          name
          type
        }
      }
    }
  }
}`,
		Variables: map[string]any{
			"issueId": issueID,
		},
	}

	body, _ := json.Marshal(query)
	req, err := http.NewRequest("POST", "https://api.linear.app/graphql", strings.NewReader(string(body)))
	if err != nil {
		return "", err
	}
	req.Header.Set("Authorization", token)
	req.Header.Set("Content-Type", "application/json")

	client := &http.Client{Timeout: 10 * time.Second}
	resp, err := client.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()

	respBody, _ := io.ReadAll(resp.Body)

	var result struct {
		Data struct {
			Issue struct {
				Team struct {
					States struct {
						Nodes []struct {
							ID   string `json:"id"`
							Name string `json:"name"`
							Type string `json:"type"`
						} `json:"nodes"`
					} `json:"states"`
				} `json:"team"`
			} `json:"issue"`
		} `json:"data"`
	}
	if err := json.Unmarshal(respBody, &result); err != nil {
		return "", fmt.Errorf("decode workflow states: %v", err)
	}

	for _, state := range result.Data.Issue.Team.States.Nodes {
		if state.Type == "completed" {
			return state.ID, nil
		}
	}
	return "", fmt.Errorf("no completed-type workflow state found for issue %s", issueID)
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

		tmpl, err := template.New(filepath.Base(candidate)).Funcs(template.FuncMap{
			"slice": func() []ProviderPanel { return nil },
			"append": func(s []ProviderPanel, v ProviderPanel) []ProviderPanel {
				return append(s, v)
			},
			"splitLines": func(s string) []string {
				return strings.Split(s, "\n")
			},
			"hasPrefix": func(s string, prefix string) bool {
				return strings.HasPrefix(s, prefix)
			},
			"trimPrefix": func(s string, prefix string) string {
				return strings.TrimPrefix(s, prefix)
			},
		}).ParseFiles(candidate)
		if err != nil {
			return nil, fmt.Errorf("error parsing template %q: %w", candidate, err)
		}
		return tmpl, nil
	}

	return nil, fmt.Errorf("template file not found: %s", indexTemplatePath)
}

// main parses startup flags, validates configuration, and serves the single-page dashboard.
func buildDashboardData(cfg *Config) DashboardData {
	var agents []AgentStats

	if cfg.Agents.Copilot.Enabled {
		s, err := fetchCopilotStats(cfg)
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
	if cfg.Agents.Linear.Enabled {
		s, err := fetchLinearStats(cfg)
		if err != nil {
			log.Printf("Linear Error: %v", err)
		}
		agents = append(agents, s)
	}
	if cfg.Agents.Gemini.Enabled {
		s, err := fetchGeminiStats()
		if err != nil {
			log.Printf("Gemini Error: %v", err)
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

	return DashboardData{
		Providers: providers,
		Rates:     rates,
		Time:      time.Now().Format("15:04:05"),
		Runtime:   "RUNTIME_01",
	}
}

func newDashboardCache() *dashboardCache {
	return &dashboardCache{
		completed: make(map[string]time.Time),
		ready:     make(chan struct{}),
	}
}

func (c *dashboardCache) pruneCompletedLocked(now time.Time) {
	for issueID, expiresAt := range c.completed {
		if !expiresAt.After(now) {
			delete(c.completed, issueID)
		}
	}
}

func (c *dashboardCache) filterCompletedLinearLocked(data *DashboardData, now time.Time) {
	c.pruneCompletedLocked(now)
	if len(c.completed) == 0 {
		return
	}

	for providerIndex := range data.Providers {
		provider := &data.Providers[providerIndex]
		if provider.Name != "Linear" {
			continue
		}

		for listIndex := range provider.Lists {
			list := &provider.Lists[listIndex]
			filtered := list.Items[:0]
			for _, item := range list.Items {
				if _, hidden := c.completed[item.Identifier]; hidden {
					continue
				}
				if _, hidden := c.completed[item.IssueID]; hidden {
					continue
				}
				filtered = append(filtered, item)
			}
			if len(filtered) == 0 {
				filtered = append(filtered, ListItem{Text: "(none)"})
			}
			list.Items = filtered
		}
	}
}

func (c *dashboardCache) getOrRefresh(cfg *Config) DashboardData {
	c.mu.RLock()
	data := c.data
	fetchedAt := c.fetchedAt
	refreshing := c.refreshing
	ready := c.ready
	c.mu.RUnlock()

	if !fetchedAt.IsZero() && time.Since(fetchedAt) < 5*time.Second {
		return data
	}

	if fetchedAt.IsZero() {
		c.startRefresh(cfg)
		<-ready
		c.mu.RLock()
		defer c.mu.RUnlock()
		return c.data
	}

	if !refreshing {
		c.startRefresh(cfg)
	}

	return data
}

func (c *dashboardCache) startRefresh(cfg *Config) {
	c.mu.Lock()
	if c.refreshing {
		c.mu.Unlock()
		return
	}
	c.refreshing = true
	c.mu.Unlock()

	go func() {
		data := buildDashboardData(cfg)
		now := time.Now()

		c.mu.Lock()
		c.filterCompletedLinearLocked(&data, now)
		c.data = data
		c.fetchedAt = now
		if c.ready != nil {
			close(c.ready)
			c.ready = nil
		}
		c.refreshing = false
		c.mu.Unlock()
	}()
}

func main() {
	configPath := flag.String("config", "config.yaml", "Path to config file")
	flag.Parse()

	if flag.NArg() > 0 && flag.Arg(0) == "start" {
		cfg, err := loadConfig(*configPath)
		if err != nil {
			log.Fatalf("Error loading config: %v", err)
		}

		tmpl, err := loadIndexTemplate()
		if err != nil {
			log.Fatalf("Error loading template: %v", err)
		}

		cache := newDashboardCache()
		cache.startRefresh(cfg)

		handler := func(w http.ResponseWriter, r *http.Request) {
			data := cache.getOrRefresh(cfg)
			if err := tmpl.Execute(w, data); err != nil {
				log.Printf("Template Execute Error: %v", err)
			}
		}

		http.HandleFunc("/", handler)
		http.HandleFunc("/linear/done", linearDoneHandler(cfg, cache))
		addr := fmt.Sprintf(":%d", cfg.Server.Port)
		fmt.Printf("Starting KindleVibe server on http://localhost%s\n", addr)
		if err := http.ListenAndServe(addr, nil); err != nil {
			log.Fatal(err)
		}
	} else {
		fmt.Println("Usage: kindlevibe start [--config config.yaml]")
	}
}
