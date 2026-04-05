# Cozystack Telemetry Server

This server collects anonymous statistics from [Cozystack](https://cozystack.io/) instances and sends them to a Prometheus-compatible database.

Client source code:
- https://github.com/cozystack/cozystack/tree/main/internal/telemetry

Read more about how telemetry is collected from docs:
- https://cozystack.io/docs/telemetry/

## Public Statistics API

The server exposes a `GET /api/overview` endpoint that returns aggregated usage statistics in JSON format. This data is used by the [Cozystack website](https://cozystack.io/) to display public telemetry on the [Telemetry page](https://cozystack.io/oss-health/telemetry/).

### How it works

1. On the **1st of each month at 00:01 Pacific Time**, the server queries VictoriaMetrics for the current state of all reporting clusters and stores a monthly snapshot to disk.
2. On first startup (if no snapshot exists for the current month), an initial snapshot is collected automatically.
3. The app list is fetched dynamically from [cozystack/cozystack packages/apps](https://github.com/cozystack/cozystack/tree/main/packages/apps) to ensure newly added applications are always included.
4. The `/api/overview` endpoint aggregates stored snapshots into three time periods: **last month**, **last quarter** (3 months), and **last 12 months**.

### Response format

```json
{
  "generated_at": "2026-04-01T07:01:00Z",
  "periods": {
    "month": {
      "label": "March 2026",
      "start": "2026-03-01",
      "end": "2026-03-31",
      "clusters": 42,
      "total_nodes": 210,
      "avg_nodes_per_cluster": 5.0,
      "total_tenants": 84,
      "avg_tenants_per_cluster": 2.0,
      "apps": {
        "postgres": 120,
        "redis": 85,
        "kubernetes": 30
      }
    },
    "quarter": { "..." : "averaged over 3 months" },
    "year": { "..." : "averaged over 12 months" }
  }
}
```

### Configuration flags

| Flag | Default | Description |
|------|---------|-------------|
| `--forward-url` | `http://vminsert-cozy-telemetry:8480/insert/0/prometheus/api/v1/import/prometheus` | URL to forward ingested metrics to |
| `--listen-addr` | `:8081` | Address to listen on |
| `--vmselect-url` | `http://vmselect-cozy-telemetry:8481` | VictoriaMetrics vmselect base URL for queries |
| `--snapshot-dir` | `/data/snapshots` | Directory to store monthly snapshot JSON files |
