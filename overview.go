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
	Month        string         `json:"month"` // "2026-03"
	CollectedAt  time.Time      `json:"collected_at"`
	Clusters     int            `json:"clusters"`
	TotalNodes   int            `json:"total_nodes"`
	TotalTenants int            `json:"total_tenants"`
	Apps         map[string]int `json:"apps"`
}

// PeriodStats represents aggregated statistics for a time period.
type PeriodStats struct {
	Label                string         `json:"label"`
	Start                string         `json:"start"`
	End                  string         `json:"end"`
	Clusters             int            `json:"clusters"`
	TotalNodes           int            `json:"total_nodes"`
	AvgNodesPerCluster   float64        `json:"avg_nodes_per_cluster"`
	TotalTenants         int            `json:"total_tenants"`
	AvgTenantsPerCluster float64        `json:"avg_tenants_per_cluster"`
	Apps                 map[string]int `json:"apps"`
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
	httpClient  *http.Client
	mu          sync.RWMutex
	snapshots   []Snapshot
}

// NewOverviewManager creates a new OverviewManager and loads any cached snapshots.
func NewOverviewManager(vmSelectURL, snapshotDir string) *OverviewManager {
	m := &OverviewManager{
		vmSelectURL: vmSelectURL,
		snapshotDir: snapshotDir,
		httpClient:  &http.Client{Timeout: 30 * time.Second},
	}
	if err := os.MkdirAll(snapshotDir, 0755); err != nil {
		log.Printf("Warning: cannot create snapshot dir %s: %v", snapshotDir, err)
	}
	m.loadSnapshots()
	return m
}

// ensureSnapshot guarantees the snapshot for monthLabel is in memory.
// It tries memory first, then disk cache, then generates it from VictoriaMetrics.
func (m *OverviewManager) ensureSnapshot(monthLabel string) {
	m.mu.RLock()
	for _, s := range m.snapshots {
		if s.Month == monthLabel {
			m.mu.RUnlock()
			return
		}
	}
	m.mu.RUnlock()

	// Try loading from disk cache (another replica may have written it)
	filename := filepath.Join(m.snapshotDir, monthLabel+".json")
	if data, err := os.ReadFile(filename); err == nil {
		var s Snapshot
		if json.Unmarshal(data, &s) == nil {
			m.mu.Lock()
			// Double-check under write lock
			for _, existing := range m.snapshots {
				if existing.Month == monthLabel {
					m.mu.Unlock()
					return
				}
			}
			m.snapshots = append(m.snapshots, s)
			sort.Slice(m.snapshots, func(i, j int) bool {
				return m.snapshots[i].Month > m.snapshots[j].Month
			})
			m.mu.Unlock()
			return
		}
	}

	// Generate from VictoriaMetrics
	m.collectSnapshot(monthLabel)
}

// collectSnapshot queries VictoriaMetrics at the end of monthLabel and stores the snapshot.
func (m *OverviewManager) collectSnapshot(monthLabel string) {
	log.Printf("Collecting snapshot for %s...", monthLabel)

	// Query at the last moment of the requested month
	t := parseMonth(monthLabel)
	queryAt := time.Date(t.Year(), t.Month()+1, 0, 23, 59, 59, 0, time.UTC)
	// Don't query into the future
	if queryAt.After(time.Now().UTC()) {
		queryAt = time.Now().UTC()
	}

	snapshot := Snapshot{
		Month:       monthLabel,
		CollectedAt: time.Now().UTC(),
		Apps:        make(map[string]int),
	}

	// Query cluster count
	clusters, err := m.queryScalar(`count(count by (cluster_id) (cozy_cluster_info))`, queryAt)
	if err != nil {
		log.Printf("Error querying cluster count: %v", err)
	} else {
		snapshot.Clusters = int(clusters)
	}

	// Query total nodes
	nodes, err := m.queryScalar(`sum(cozy_nodes_count)`, queryAt)
	if err != nil {
		log.Printf("Error querying total nodes: %v", err)
	} else {
		snapshot.TotalNodes = int(nodes)
	}

	// Query total tenants (tenant is an application kind)
	tenants, err := m.queryScalar(`sum(cozy_application_count{kind="tenant"})`, queryAt)
	if err != nil {
		log.Printf("Error querying total tenants: %v", err)
	} else {
		snapshot.TotalTenants = int(tenants)
	}

	// Fetch app list from GitHub
	appList := m.fetchAppList()

	// Query application counts by kind
	appCounts, err := m.queryVector(`sum by (kind) (cozy_application_count)`, queryAt)
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

	// Skip saving if no meaningful data was collected (e.g. VictoriaMetrics was unreachable)
	if snapshot.Clusters == 0 && snapshot.TotalNodes == 0 && snapshot.TotalTenants == 0 {
		log.Printf("Snapshot for %s has no data (all zeros), skipping save", monthLabel)
		return
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

// queryScalar executes a PromQL query at queryAt and returns a single scalar value.
func (m *OverviewManager) queryScalar(query string, queryAt time.Time) (float64, error) {
	results, err := m.queryVector(query, queryAt)
	if err != nil {
		return 0, err
	}
	if len(results) == 0 {
		return 0, nil
	}
	return results[0].Value, nil
}

// queryVector executes a PromQL instant query at queryAt and returns results.
func (m *OverviewManager) queryVector(query string, queryAt time.Time) ([]vectorResult, error) {
	queryURL := fmt.Sprintf("%s/select/0/prometheus/api/v1/query?query=%s&time=%d",
		strings.TrimRight(m.vmSelectURL, "/"),
		url.QueryEscape(query),
		queryAt.Unix())

	resp, err := m.httpClient.Get(queryURL)
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

	req, err := http.NewRequest("GET", apiURL, nil)
	if err != nil {
		log.Printf("Error creating GitHub request: %v", err)
		return defaultAppList()
	}
	req.Header.Set("Accept", "application/vnd.github.v3+json")

	resp, err := m.httpClient.Do(req)
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

	// Atomic write: temp file + rename to prevent partial files on crash
	tmpFile, err := os.CreateTemp(m.snapshotDir, s.Month+".*.tmp")
	if err != nil {
		log.Printf("Error creating temp snapshot for %s: %v", filename, err)
		return
	}
	tmpName := tmpFile.Name()
	defer os.Remove(tmpName) // clean up on any failure path

	if _, err := tmpFile.Write(data); err != nil {
		tmpFile.Close()
		log.Printf("Error writing temp snapshot %s: %v", tmpName, err)
		return
	}
	if err := tmpFile.Sync(); err != nil {
		tmpFile.Close()
		log.Printf("Error syncing temp snapshot %s: %v", tmpName, err)
		return
	}
	if err := tmpFile.Close(); err != nil {
		log.Printf("Error closing temp snapshot %s: %v", tmpName, err)
		return
	}
	if err := os.Rename(tmpName, filename); err != nil {
		log.Printf("Error replacing snapshot %s: %v", filename, err)
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
// Both year and month query parameters are required to prevent abuse.
// Example: GET /api/overview?year=2024&month=04
func (m *OverviewManager) HandleOverview(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}

	yearStr := r.URL.Query().Get("year")
	monthStr := r.URL.Query().Get("month")
	if yearStr == "" || monthStr == "" {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusBadRequest)
		fmt.Fprintln(w, `{"error":"year and month query parameters are required"}`)
		return
	}

	year, err := strconv.Atoi(yearStr)
	if err != nil || year < 2024 || year > 2100 {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusBadRequest)
		fmt.Fprintln(w, `{"error":"invalid year parameter"}`)
		return
	}

	month, err := strconv.Atoi(monthStr)
	if err != nil || month < 1 || month > 12 {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusBadRequest)
		fmt.Fprintln(w, `{"error":"invalid month parameter"}`)
		return
	}

	monthLabel := fmt.Sprintf("%04d-%02d", year, month)

	// Ensure snapshot exists (loads from cache or generates on the fly)
	m.ensureSnapshot(monthLabel)

	// Collect all snapshots up to and including the requested month
	m.mu.RLock()
	var snapshots []Snapshot
	for _, s := range m.snapshots {
		if s.Month <= monthLabel {
			snapshots = append(snapshots, s)
		}
	}
	m.mu.RUnlock()

	if len(snapshots) == 0 {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusServiceUnavailable)
		fmt.Fprintln(w, `{"error":"no telemetry data available"}`)
		return
	}

	overview := m.buildOverview(snapshots)

	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Access-Control-Allow-Origin", "*")
	if err := json.NewEncoder(w).Encode(overview); err != nil {
		log.Printf("Error encoding overview response: %v", err)
	}
}

// buildOverview constructs the overview response from stored snapshots.
// snapshots must be sorted descending by month; index 0 is the most recent.
func (m *OverviewManager) buildOverview(snapshots []Snapshot) OverviewResponse {
	resp := OverviewResponse{
		GeneratedAt: snapshots[0].CollectedAt.Format(time.RFC3339),
		Periods:     make(map[string]PeriodStats),
	}

	// Month: latest snapshot
	resp.Periods["month"] = aggregateSnapshots(snapshots[:1], false)

	// Quarter: last 3 calendar months
	resp.Periods["quarter"] = aggregateSnapshots(filterSnapshotsByMonths(snapshots, 3), true)

	// Year: last 12 calendar months
	resp.Periods["year"] = aggregateSnapshots(filterSnapshotsByMonths(snapshots, 12), true)

	return resp
}

// filterSnapshotsByMonths returns snapshots within the last N calendar months
// relative to the latest snapshot. This ensures correct ranges even with gaps.
func filterSnapshotsByMonths(snapshots []Snapshot, months int) []Snapshot {
	if len(snapshots) == 0 {
		return nil
	}

	latest := parseMonth(snapshots[0].Month)
	cutoff := latest.AddDate(0, -(months - 1), 0)

	var filtered []Snapshot
	for _, s := range snapshots {
		t := parseMonth(s.Month)
		if !t.Before(cutoff) {
			filtered = append(filtered, s)
		}
	}
	return filtered
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
		log.Printf("Warning: failed to parse month %q: %v, falling back to current time", month, err)
		return time.Now()
	}
	return t
}

func roundTo(val float64, places int) float64 {
	pow := math.Pow(10, float64(places))
	return math.Round(val*pow) / pow
}
