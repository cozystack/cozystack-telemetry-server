package main

import "testing"

// TestEnrichMetrics pins the parsing/enrichment contract: TYPE and HELP lines
// are passed through, and every series gets a cluster_id label added with the
// resulting labels emitted in sorted order (with __name__ rendered as the
// metric name rather than a label).
func TestEnrichMetrics(t *testing.T) {
	input := []byte("# TYPE http_requests_total counter\n" +
		"# HELP http_requests_total Total HTTP requests\n" +
		"http_requests_total{method=\"get\",code=\"200\"} 1027\n")

	got, err := enrichMetrics(input, "test-cluster")
	if err != nil {
		t.Fatalf("enrichMetrics returned error: %v", err)
	}

	want := "# TYPE http_requests_total counter\n" +
		"# HELP http_requests_total Total HTTP requests\n" +
		"http_requests_total{cluster_id=\"test-cluster\",code=\"200\",method=\"get\"} 1027\n"

	if string(got) != want {
		t.Errorf("enrichMetrics output mismatch:\n got: %q\nwant: %q", string(got), want)
	}
}

// TestEnrichMetricsBareSeries verifies a bare metric without braces still gets
// the cluster_id label attached.
func TestEnrichMetricsBareSeries(t *testing.T) {
	input := []byte("up 1\n")

	got, err := enrichMetrics(input, "c1")
	if err != nil {
		t.Fatalf("enrichMetrics returned error: %v", err)
	}

	want := "up{cluster_id=\"c1\"} 1\n"
	if string(got) != want {
		t.Errorf("enrichMetrics output mismatch:\n got: %q\nwant: %q", string(got), want)
	}
}

// TestEnrichMetricsSortsLabels uses original labels that do NOT already
// bracket cluster_id alphabetically, proving the output is re-sorted by name
// (cluster_id is inserted between aaa and zzz) rather than appended.
func TestEnrichMetricsSortsLabels(t *testing.T) {
	input := []byte("metric{zzz=\"1\",aaa=\"2\"} 5\n")

	got, err := enrichMetrics(input, "c1")
	if err != nil {
		t.Fatalf("enrichMetrics returned error: %v", err)
	}

	want := "metric{aaa=\"2\",cluster_id=\"c1\",zzz=\"1\"} 5\n"
	if string(got) != want {
		t.Errorf("enrichMetrics output mismatch:\n got: %q\nwant: %q", string(got), want)
	}
}

// TestEnrichMetricsEmptyClusterID pins the behavior for an empty cluster ID:
// labels.Builder.Set with an empty value is a delete, so no cluster_id label is
// emitted. This input never reaches enrichMetrics in production (isValidClusterID
// rejects empty IDs at the handler), but the contract is pinned here so the
// behavior is explicit.
func TestEnrichMetricsEmptyClusterID(t *testing.T) {
	input := []byte("up 1\n")

	got, err := enrichMetrics(input, "")
	if err != nil {
		t.Fatalf("enrichMetrics returned error: %v", err)
	}

	want := "up 1\n"
	if string(got) != want {
		t.Errorf("enrichMetrics output mismatch:\n got: %q\nwant: %q", string(got), want)
	}
}
