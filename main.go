package main

import (
	"bytes"
	"flag"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"sort"
	"strings"
	"time"

	"github.com/oschwald/geoip2-golang"
	"github.com/prometheus/prometheus/model/labels"
	"github.com/prometheus/prometheus/model/textparse"
)

// GeoIPReader wraps the GeoIP2 database reader
type GeoIPReader struct {
	reader *geoip2.Reader
}

// NewGeoIPReader creates a new GeoIP reader instance
func NewGeoIPReader(dbPath string) (*GeoIPReader, error) {
	reader, err := geoip2.Open(dbPath)
	if err != nil {
		return nil, fmt.Errorf("error opening GeoIP database: %v", err)
	}
	return &GeoIPReader{reader: reader}, nil
}

// Close closes the GeoIP reader
func (g *GeoIPReader) Close() {
	if g.reader != nil {
		g.reader.Close()
	}
}

// GetCountry returns the country code for an IP address
func (g *GeoIPReader) GetCountry(ipStr string) string {
	ip := net.ParseIP(ipStr)
	if ip == nil {
		return "unknown"
	}

	record, err := g.reader.Country(ip)
	if err != nil {
		log.Printf("Error getting country for IP %s: %v", ipStr, err)
		return "unknown"
	}

	return record.Country.IsoCode
}

func extractIP(addr string) string {
	host, _, err := net.SplitHostPort(addr)
	if err != nil {
		return addr
	}

	ip := net.ParseIP(host)
	if ip == nil {
		return host
	}

	return ip.String()
}

func enrichMetrics(input []byte, sourceIP string, clusterID string, geoIP *GeoIPReader) ([]byte, error) {
	symbolTable := labels.NewSymbolTable()
	parser := textparse.NewPromParser(input, symbolTable)
	var builder bytes.Buffer

	metricsCount := 0
	country := geoIP.GetCountry(sourceIP)

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

			// Add source_ip, cluster_id and country_code to the labels
			lbls = append(lbls,
				labels.Label{
					Name:  "source_ip",
					Value: sourceIP,
				},
				labels.Label{
					Name:  "cluster_id",
					Value: clusterID,
				},
				labels.Label{
					Name:  "country_code",
					Value: country,
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

	log.Printf("Processed %d metrics for cluster %s from %s (%s)", metricsCount, clusterID, sourceIP, country)
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

func handleTelemetry(w http.ResponseWriter, r *http.Request, geoIP *GeoIPReader, forwardURL string) {
	startTime := time.Now()

	if r.Method != http.MethodPost {
		log.Printf("Method not allowed: %s", r.Method)
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}

	clusterID := r.Header.Get("X-Cluster-ID")
	if clusterID == "" {
		log.Printf("Request rejected: missing X-Cluster-ID header")
		http.Error(w, "X-Cluster-ID header is required", http.StatusBadRequest)
		return
	}

	body, err := io.ReadAll(r.Body)
	if err != nil {
		log.Printf("Error reading request body: %v", err)
		http.Error(w, fmt.Sprintf("Error reading request: %v", err), http.StatusBadRequest)
		return
	}

	sourceIP := extractIP(r.RemoteAddr)
	log.Printf("Received metrics from %s for cluster %s", sourceIP, clusterID)

	enrichedMetrics, err := enrichMetrics(body, sourceIP, clusterID, geoIP)
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
	geoipDBPath := flag.String("geoip-db", "/GeoLite2-Country.mmdb", "Path to GeoLite2 Country database")
	forwardURL := flag.String("forward-url", "http://vminsert-cozy-telemetry:8480/insert/0/prometheus/api/v1/import/prometheus", "URL to forward the metrics to")
	listenAddr := flag.String("listen-addr", ":8081", "Address to listen on for incoming metrics")
	flag.Parse()

	// Initialize GeoIP
	geoIP, err := NewGeoIPReader(*geoipDBPath)
	if err != nil {
		log.Fatalf("Failed to initialize GeoIP database: %v", err)
	}
	defer geoIP.Close()

	server := &http.Server{
		Addr:         *listenAddr,
		ReadTimeout:  10 * time.Second,
		WriteTimeout: 10 * time.Second,
	}

	http.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		handleTelemetry(w, r, geoIP, *forwardURL)
	})

	log.Printf("Starting server on %s", *listenAddr)
	log.Printf("Forwarding metrics to %s", *forwardURL)
	log.Printf("Using GeoIP database at %s", *geoipDBPath)

	if err := server.ListenAndServe(); err != nil {
		log.Fatalf("Server error: %v", err)
	}
}
