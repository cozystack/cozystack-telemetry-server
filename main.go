package main

import (
	"bytes"
	"errors"
	"flag"
	"fmt"
	"io"
	"log"
	"net/http"
	"sort"
	"strings"
	"time"

	"github.com/prometheus/prometheus/model/labels"
	"github.com/prometheus/prometheus/model/textparse"
)

func enrichMetrics(input []byte, clusterID string) ([]byte, error) {
	symbolTable := labels.NewSymbolTable()
	parser := textparse.NewPromParser(input, symbolTable)
	var builder bytes.Buffer

	metricsCount := 0

	for {
		entry, err := parser.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			return nil, fmt.Errorf("parsing error: %v", err)
		}

		switch entry {
		case textparse.EntryType:
			name, typ := parser.Type()
			fmt.Fprintf(&builder, "# TYPE %s %s\n", name, typ)

		case textparse.EntryHelp:
			name, help := parser.Help()
			fmt.Fprintf(&builder, "# HELP %s %s\n", name, help)

		case textparse.EntrySeries:
			_, _, value := parser.Series()

			var lbls labels.Labels
			parser.Metric(&lbls)

			// Add cluster_id to the labels
			lbls = append(lbls,
				labels.Label{
					Name:  "cluster_id",
					Value: clusterID,
				},
			)

			sort.Sort(lbls)

			metricName := ""
			for _, lbl := range lbls {
				if lbl.Name == "__name__" {
					metricName = lbl.Value
					break
				}
			}

			var labelStrings []string
			for _, lbl := range lbls {
				if lbl.Name != "__name__" {
					labelStrings = append(labelStrings, fmt.Sprintf("%s=\"%s\"", lbl.Name, lbl.Value))
				}
			}

			labelsStr := ""
			if len(labelStrings) > 0 {
				labelsStr = fmt.Sprintf("{%s}", strings.Join(labelStrings, ","))
			}

			fmt.Fprintf(&builder, "%s%s %g\n", metricName, labelsStr, value)
			metricsCount++
		}
	}

	log.Printf("Processed %d metrics for cluster %s", metricsCount, clusterID)
	return builder.Bytes(), nil
}

func forwardToPrometheus(metrics []byte, forwardURL string) error {
	startTime := time.Now()

	resp, err := http.Post(forwardURL, "text/plain", bytes.NewBuffer(metrics))
	if err != nil {
		return fmt.Errorf("error forwarding to Prometheus: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK && resp.StatusCode != http.StatusNoContent {
		body, _ := io.ReadAll(resp.Body)
		return fmt.Errorf("unexpected status code %d: %s", resp.StatusCode, string(body))
	}

	log.Printf("Successfully forwarded metrics to Prometheus in %v", time.Since(startTime))
	return nil
}

// maxTelemetryBodySize is the maximum accepted size for a telemetry POST body.
const maxTelemetryBodySize = 10 * 1024 * 1024 // 10 MB

// isValidClusterID accepts only alphanumeric characters, hyphens, underscores,
// and dots (standard Kubernetes naming). This prevents Prometheus text-format
// injection via the X-Cluster-ID header (e.g. a value containing '"' or '\n'
// would corrupt the label output forwarded to VictoriaMetrics).
func isValidClusterID(s string) bool {
	if s == "" || len(s) > 253 {
		return false
	}
	for _, c := range s {
		if !((c >= 'a' && c <= 'z') || (c >= 'A' && c <= 'Z') ||
			(c >= '0' && c <= '9') || c == '-' || c == '_' || c == '.') {
			return false
		}
	}
	return true
}

func handleTelemetry(w http.ResponseWriter, r *http.Request, forwardURL string) {
	startTime := time.Now()

	if r.Method != http.MethodPost {
		log.Printf("Method not allowed: %s", r.Method)
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}

	clusterID := r.Header.Get("X-Cluster-ID")
	if !isValidClusterID(clusterID) {
		log.Printf("Request rejected: invalid or missing X-Cluster-ID header")
		http.Error(w, "X-Cluster-ID header is required and must contain only alphanumeric characters, hyphens, underscores, or dots", http.StatusBadRequest)
		return
	}

	r.Body = http.MaxBytesReader(w, r.Body, maxTelemetryBodySize)
	body, err := io.ReadAll(r.Body)
	if err != nil {
		var maxBytesErr *http.MaxBytesError
		if errors.As(err, &maxBytesErr) {
			log.Printf("Request from cluster %s rejected: body exceeded %d bytes limit", clusterID, maxBytesErr.Limit)
			return
		}
		log.Printf("Error reading request body: %v", err)
		http.Error(w, fmt.Sprintf("Error reading request: %v", err), http.StatusBadRequest)
		return
	}

	log.Printf("Received metrics for cluster %s", clusterID)

	enrichedMetrics, err := enrichMetrics(body, clusterID)
	if err != nil {
		log.Printf("Error processing metrics for cluster %s: %v", clusterID, err)
		http.Error(w, fmt.Sprintf("Error processing metrics: %v", err), http.StatusBadRequest)
		return
	}

	if err := forwardToPrometheus(enrichedMetrics, forwardURL); err != nil {
		log.Printf("Error forwarding metrics for cluster %s: %v", clusterID, err)
		http.Error(w, fmt.Sprintf("Error forwarding metrics: %v", err), http.StatusInternalServerError)
		return
	}

	log.Printf("Request from cluster %s processed in %v", clusterID, time.Since(startTime))
	w.WriteHeader(http.StatusOK)
}

func main() {
	// Define flags
	forwardURL := flag.String("forward-url", "http://vminsert-cozy-telemetry:8480/insert/0/prometheus/api/v1/import/prometheus", "URL to forward the metrics to")
	listenAddr := flag.String("listen-addr", ":8081", "Address to listen on for incoming metrics")
	vmSelectURL := flag.String("vmselect-url", "http://vmselect-cozy-telemetry:8481", "VictoriaMetrics vmselect base URL for queries")
	snapshotDir := flag.String("snapshot-dir", "/data/snapshots", "Directory to store monthly snapshots")
	flag.Parse()

	// Initialize overview manager
	overview := NewOverviewManager(*vmSelectURL, *snapshotDir)

	server := &http.Server{
		Addr:         *listenAddr,
		ReadTimeout:  10 * time.Second,
		WriteTimeout: 10 * time.Second,
	}

	// The overview handler may query VictoriaMetrics on cache miss (up to 30s),
	// so it gets its own 55s timeout instead of inheriting the global 10s WriteTimeout.
	http.Handle("/api/overview", http.TimeoutHandler(
		http.HandlerFunc(overview.HandleOverview),
		55*time.Second,
		`{"error":"request timeout"}`,
	))
	http.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		handleTelemetry(w, r, *forwardURL)
	})

	log.Printf("Starting server on %s", *listenAddr)
	log.Printf("Forwarding metrics to %s", *forwardURL)
	log.Printf("VictoriaMetrics select URL: %s", *vmSelectURL)
	log.Printf("Snapshot directory: %s", *snapshotDir)

	if err := server.ListenAndServe(); err != nil {
		log.Fatalf("Server error: %v", err)
	}
}
