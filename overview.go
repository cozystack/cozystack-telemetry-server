package main

import (
	"encoding/json"
	"fmt"
	"io"
	"log"
	"math"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
)

// Snapshot represents a monthly telemetry snapshot.
type Snapshot struct {
	Month         string         `json:"month"`          // "2026-03"
	CollectedAt   time.Time      `json:"collected_at"`
	Clusters      int            `json:"clusters"`
	TotalNodes    int            `json:"total_nodes"`
	TotalTenants  int            `json:"total_tenants"`
	Apps          map[string]int `json:"apps"`
}

// PeriodStats represents aggregated statistics for a time period.
type PeriodStats struct {
	Label              string         `json:"label"`
	Start              string         `json:"start"`
	End                string         `json:"end"`
	Clusters           int            `json:"clusters"`
	TotalNodes         int            `json:"total_nodes"`
	AvgNodesPerCluster float64        `json:"avg_nodes_per_cluster"`
	TotalTenants       int            `json:"total_tenants"`
	AvgTenantsPerCluster float64      `json:"avg_tenants_per_cluster"`
	Apps               map[string]int `json:"apps"`
}

// OverviewResponse is the JSON returned by /api/overview.
type OverviewResponse struct {
	GeneratedAt string                 `json:"generated_at"`
	Periods     map[string]PeriodStats `json:"periods"`
}

// vmQueryResult represents a Prometheus instant query response.
type vmQueryResult struct {
	Status string `json:"status"`
	Data   struct {
		ResultType string `json:"resultType"`
		Result     []struct {
			Metric map[string]string `json:"metric"`
			Value  [2]interface{}    `json:"value"`
		} `json:"result"`
	} `json:"data"`
}

// githubContent represents a GitHub API directory entry.
type githubContent struct {
	Name string `json:"name"`
	Type string `json:"type"`
}

// OverviewManager handles snapshot collection, storage, and serving.
type OverviewManager struct {
	vmSelectURL string
	snapshotDir string
	mu          sync.RWMutex
	snapshots   []Snapshot
}

// NewOverviewManager creates a new OverviewManager.
func NewOverviewManager(vmSelectURL, snapshotDir string) *OverviewManager {
	return &OverviewManager{
		vmSelectURL: vmSelectURL,
		snapshotDir: snapshotDir,
	}
}

// Start loads existing snapshots and begins the monthly scheduler.
// It also collects an initial snapshot if none exists for the current period.
func (m *OverviewManager) Start() {
	if err := os.MkdirAll(m.snapshotDir, 0755); err != nil {
		log.Printf("Warning: cannot create snapshot dir %s: %v", m.snapshotDir, err)
	}

	m.loadSnapshots()

	// Collect initial snapshot in background
	go func() {
		// Small delay to let the server start and VictoriaMetrics become available
		time.Sleep(30 * time.Second)
		m.collectIfNeeded()
	}()

	go m.scheduleMonthly()
}

// collectIfNeeded checks if a snapshot for the current or previous month exists,
// and collects one if not.
func (m *OverviewManager) collectIfNeeded() {
	now := time.Now().In(pacificLocation())
	currentMonth := now.Format("2006-01")

	m.mu.RLock()
	hasCurrentMonth := false
	for _, s := range m.snapshots {
		if s.Month == currentMonth {
			hasCurrentMonth = true
			break
		}
	}
	m.mu.RUnlock()

	if !hasCurrentMonth {
		log.Println("No snapshot for current month, collecting now...")
		m.collectSnapshot(currentMonth)
	}
}

// scheduleMonthly runs the collection at 00:01 Pacific Time on the 1st of each month.
func (m *OverviewManager) scheduleMonthly() {
	for {
		now := time.Now().In(pacificLocation())
		next := nextFirstOfMonth(now)
		sleepDuration := next.Sub(now)

		log.Printf("Next snapshot collection scheduled at %s (in %s)", next.Format(time.RFC3339), sleepDuration)
		time.Sleep(sleepDuration)

		// On the 1st, we label the snapshot with the previous month
		prevMonth := next.AddDate(0, -1, 0).Format("2006-01")
		m.collectSnapshot(prevMonth)
	}
}

// nextFirstOfMonth returns the next 1st of month at 00:01 Pacific Time.
func nextFirstOfMonth(now time.Time) time.Time {
	year, month, _ := now.Date()
	// Next month's 1st at 00:01
	nextMonth := time.Date(year, month+1, 1, 0, 1, 0, 0, now.Location())
	return nextMonth
}

// pacificLocation returns the US Pacific timezone.
func pacificLocation() *time.Location {
	loc, err := time.LoadLocation("America/Los_Angeles")
	if err != nil {
		log.Printf("Warning: cannot load Pacific timezone, using UTC: %v", err)
		return time.UTC
	}
	return loc
}

// collectSnapshot queries VictoriaMetrics and GitHub, then stores the snapshot.
func (m *OverviewManager) collectSnapshot(monthLabel string) {
	log.Printf("Collecting snapshot for %s...", monthLabel)

	snapshot := Snapshot{
		Month:       monthLabel,
		CollectedAt: time.Now().UTC(),
		Apps:        make(map[string]int),
	}

	// Query cluster count
	clusters, err := m.queryScalar(`count(count by (cluster_id) (cozy_cluster_info))`)
	if err != nil {
		log.Printf("Error querying cluster count: %v", err)
	} else {
		snapshot.Clusters = int(clusters)
	}

	// Query total nodes
	nodes, err := m.queryScalar(`sum(cozy_nodes_count)`)
	if err != nil {
		log.Printf("Error querying total nodes: %v", err)
	} else {
		snapshot.TotalNodes = int(nodes)
	}

	// Query total tenants (tenant is an application kind)
	tenants, err := m.queryScalar(`sum(cozy_application_count{kind="tenant"})`)
	if err != nil {
		log.Printf("Error querying total tenants: %v", err)
	} else {
		snapshot.TotalTenants = int(tenants)
	}

	// Fetch app list from GitHub
	appList := m.fetchAppList()

	// Query application counts by kind
	appCounts, err := m.queryVector(`sum by (kind) (cozy_application_count)`)
	if err != nil {
		log.Printf("Error querying application counts: %v", err)
	}

	// Build app counts map using GitHub app list as reference
	appCountMap := make(map[string]float64)
	for _, r := range appCounts {
		if kind, ok := r.Metric["kind"]; ok {
			appCountMap[kind] = r.Value
		}
	}
	for _, app := range appList {
		count := 0
		if v, ok := appCountMap[app]; ok {
			count = int(v)
		}
		snapshot.Apps[app] = count
	}

	// Also include any tracked apps not in the GitHub list
	for kind, val := range appCountMap {
		if _, exists := snapshot.Apps[kind]; !exists {
			snapshot.Apps[kind] = int(val)
		}
	}

	// Save snapshot
	m.saveSnapshot(snapshot)
	log.Printf("Snapshot for %s collected: %d clusters, %d nodes, %d tenants, %d app types",
		monthLabel, snapshot.Clusters, snapshot.TotalNodes, snapshot.TotalTenants, len(snapshot.Apps))
}

type vectorResult struct {
	Metric map[string]string
	Value  float64
}

// queryScalar executes a PromQL query and returns a single scalar value.
func (m *OverviewManager) queryScalar(query string) (float64, error) {
	results, err := m.queryVector(query)
	if err != nil {
		return 0, err
	}
	if len(results) == 0 {
		return 0, nil
	}
	return results[0].Value, nil
}

// queryVector executes a PromQL instant query and returns results.
func (m *OverviewManager) queryVector(query string) ([]vectorResult, error) {
	queryURL := fmt.Sprintf("%s/select/0/prometheus/api/v1/query?query=%s",
		strings.TrimRight(m.vmSelectURL, "/"),
		url.QueryEscape(query))

	client := &http.Client{Timeout: 30 * time.Second}
	resp, err := client.Get(queryURL)
	if err != nil {
		return nil, fmt.Errorf("VM query error: %v", err)
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("reading VM response: %v", err)
	}

	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("VM returned status %d: %s", resp.StatusCode, string(body))
	}

	var qr vmQueryResult
	if err := json.Unmarshal(body, &qr); err != nil {
		return nil, fmt.Errorf("parsing VM response: %v", err)
	}

	if qr.Status != "success" {
		return nil, fmt.Errorf("VM query status: %s", qr.Status)
	}

	var results []vectorResult
	for _, r := range qr.Data.Result {
		valStr, ok := r.Value[1].(string)
		if !ok {
			continue
		}
		val, err := strconv.ParseFloat(valStr, 64)
		if err != nil {
			continue
		}
		results = append(results, vectorResult{
			Metric: r.Metric,
			Value:  val,
		})
	}
	return results, nil
}

// fetchAppList fetches the list of apps from the cozystack GitHub repository.
func (m *OverviewManager) fetchAppList() []string {
	apiURL := "https://api.github.com/repos/cozystack/cozystack/contents/packages/apps"
	client := &http.Client{Timeout: 15 * time.Second}

	req, err := http.NewRequest("GET", apiURL, nil)
	if err != nil {
		log.Printf("Error creating GitHub request: %v", err)
		return defaultAppList()
	}
	req.Header.Set("Accept", "application/vnd.github.v3+json")

	resp, err := client.Do(req)
	if err != nil {
		log.Printf("Error fetching app list from GitHub: %v", err)
		return defaultAppList()
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		log.Printf("GitHub API returned status %d", resp.StatusCode)
		return defaultAppList()
	}

	var entries []githubContent
	if err := json.NewDecoder(resp.Body).Decode(&entries); err != nil {
		log.Printf("Error parsing GitHub response: %v", err)
		return defaultAppList()
	}

	var apps []string
	for _, e := range entries {
		if e.Type == "dir" {
			apps = append(apps, e.Name)
		}
	}

	if len(apps) == 0 {
		return defaultAppList()
	}

	sort.Strings(apps)
	log.Printf("Fetched %d apps from GitHub", len(apps))
	return apps
}

// defaultAppList returns a fallback list of known apps.
func defaultAppList() []string {
	return []string{
		"bucket", "clickhouse", "foundationdb", "harbor", "http-cache",
		"kafka", "kubernetes", "mariadb", "mongodb", "nats", "openbao",
		"opensearch", "postgres", "qdrant", "rabbitmq", "redis",
		"tcp-balancer", "tenant", "vm-disk", "vm-instance", "vpc", "vpn",
	}
}

// saveSnapshot writes a snapshot to disk and updates the in-memory list.
func (m *OverviewManager) saveSnapshot(s Snapshot) {
	data, err := json.MarshalIndent(s, "", "  ")
	if err != nil {
		log.Printf("Error marshaling snapshot: %v", err)
		return
	}

	filename := filepath.Join(m.snapshotDir, s.Month+".json")
	if err := os.WriteFile(filename, data, 0644); err != nil {
		log.Printf("Error writing snapshot to %s: %v", filename, err)
		return
	}

	m.mu.Lock()
	defer m.mu.Unlock()

	// Replace existing snapshot for this month or append
	found := false
	for i, existing := range m.snapshots {
		if existing.Month == s.Month {
			m.snapshots[i] = s
			found = true
			break
		}
	}
	if !found {
		m.snapshots = append(m.snapshots, s)
	}

	// Sort by month descending
	sort.Slice(m.snapshots, func(i, j int) bool {
		return m.snapshots[i].Month > m.snapshots[j].Month
	})
}

// loadSnapshots reads all snapshot files from disk.
func (m *OverviewManager) loadSnapshots() {
	entries, err := os.ReadDir(m.snapshotDir)
	if err != nil {
		log.Printf("Warning: cannot read snapshot dir: %v", err)
		return
	}

	m.mu.Lock()
	defer m.mu.Unlock()

	m.snapshots = nil
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".json") {
			continue
		}
		data, err := os.ReadFile(filepath.Join(m.snapshotDir, e.Name()))
		if err != nil {
			log.Printf("Warning: cannot read snapshot %s: %v", e.Name(), err)
			continue
		}
		var s Snapshot
		if err := json.Unmarshal(data, &s); err != nil {
			log.Printf("Warning: cannot parse snapshot %s: %v", e.Name(), err)
			continue
		}
		m.snapshots = append(m.snapshots, s)
	}

	sort.Slice(m.snapshots, func(i, j int) bool {
		return m.snapshots[i].Month > m.snapshots[j].Month
	})

	log.Printf("Loaded %d snapshots from disk", len(m.snapshots))
}

// HandleOverview serves the /api/overview endpoint.
func (m *OverviewManager) HandleOverview(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}

	m.mu.RLock()
	snapshots := make([]Snapshot, len(m.snapshots))
	copy(snapshots, m.snapshots)
	m.mu.RUnlock()

	if len(snapshots) == 0 {
		http.Error(w, "No telemetry data available yet", http.StatusServiceUnavailable)
		return
	}

	overview := m.buildOverview(snapshots)

	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Access-Control-Allow-Origin", "*")
	json.NewEncoder(w).Encode(overview)
}

// buildOverview constructs the overview response from stored snapshots.
func (m *OverviewManager) buildOverview(snapshots []Snapshot) OverviewResponse {
	resp := OverviewResponse{
		GeneratedAt: snapshots[0].CollectedAt.Format(time.RFC3339),
		Periods:     make(map[string]PeriodStats),
	}

	// Month: latest snapshot
	resp.Periods["month"] = aggregateSnapshots(snapshots[:1], false)

	// Quarter: last 3 months
	quarterCount := min(3, len(snapshots))
	resp.Periods["quarter"] = aggregateSnapshots(snapshots[:quarterCount], true)

	// Year: last 12 months
	yearCount := min(12, len(snapshots))
	resp.Periods["year"] = aggregateSnapshots(snapshots[:yearCount], true)

	return resp
}

// aggregateSnapshots computes stats from a list of snapshots.
// If avg is true, it computes averages; otherwise uses the single snapshot values.
func aggregateSnapshots(snapshots []Snapshot, avg bool) PeriodStats {
	if len(snapshots) == 0 {
		return PeriodStats{}
	}

	// Snapshots are sorted descending by month. The latest is first.
	latest := snapshots[0]
	oldest := snapshots[len(snapshots)-1]

	stats := PeriodStats{
		Apps: make(map[string]int),
	}

	// Build label and date range
	latestDate := parseMonth(latest.Month)
	oldestDate := parseMonth(oldest.Month)

	if len(snapshots) == 1 {
		stats.Label = latestDate.Format("January 2006")
		stats.Start = latestDate.Format("2006-01-02")
		endOfMonth := latestDate.AddDate(0, 1, -1)
		stats.End = endOfMonth.Format("2006-01-02")
	} else {
		stats.Label = fmt.Sprintf("%s \u2014 %s",
			oldestDate.Format("January 2006"),
			latestDate.Format("January 2006"))
		stats.Start = oldestDate.Format("2006-01-02")
		endOfMonth := latestDate.AddDate(0, 1, -1)
		stats.End = endOfMonth.Format("2006-01-02")
	}

	if !avg || len(snapshots) == 1 {
		// Use the latest snapshot directly
		stats.Clusters = latest.Clusters
		stats.TotalNodes = latest.TotalNodes
		stats.TotalTenants = latest.TotalTenants
		if latest.Clusters > 0 {
			stats.AvgNodesPerCluster = roundTo(float64(latest.TotalNodes)/float64(latest.Clusters), 1)
			stats.AvgTenantsPerCluster = roundTo(float64(latest.TotalTenants)/float64(latest.Clusters), 1)
		}
		for k, v := range latest.Apps {
			stats.Apps[k] = v
		}
		return stats
	}

	// Average across snapshots
	n := float64(len(snapshots))
	var totalClusters, totalNodes, totalTenants float64
	appTotals := make(map[string]float64)

	for _, s := range snapshots {
		totalClusters += float64(s.Clusters)
		totalNodes += float64(s.TotalNodes)
		totalTenants += float64(s.TotalTenants)
		for k, v := range s.Apps {
			appTotals[k] += float64(v)
		}
	}

	stats.Clusters = int(math.Round(totalClusters / n))
	stats.TotalNodes = int(math.Round(totalNodes / n))
	stats.TotalTenants = int(math.Round(totalTenants / n))
	if stats.Clusters > 0 {
		stats.AvgNodesPerCluster = roundTo(float64(stats.TotalNodes)/float64(stats.Clusters), 1)
		stats.AvgTenantsPerCluster = roundTo(float64(stats.TotalTenants)/float64(stats.Clusters), 1)
	}

	for k, v := range appTotals {
		stats.Apps[k] = int(math.Round(v / n))
	}

	return stats
}

// parseMonth parses "2006-01" into a time.Time for the 1st of that month.
func parseMonth(month string) time.Time {
	t, err := time.Parse("2006-01-02", month+"-01")
	if err != nil {
		return time.Now()
	}
	return t
}

func roundTo(val float64, places int) float64 {
	pow := math.Pow(10, float64(places))
	return math.Round(val*pow) / pow
}

func min(a, b int) int {
	if a < b {
		return a
	}
	return b
}
