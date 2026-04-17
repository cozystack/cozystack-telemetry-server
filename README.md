# Cozystack Telemetry Server

This server collects anonymous statistics from [Cozystack](https://cozystack.io/) instances and sends them to a Prometheus-compatible database.

Client source code:
- https://github.com/cozystack/cozystack/tree/main/internal/telemetry

Read more about how telemetry is collected from docs:
- https://cozystack.io/docs/telemetry/

## Public Statistics API

The server exposes a `GET /api/overview?year=YYYY&month=MM` endpoint that returns aggregated usage statistics in JSON format. This data is used by the [Cozystack website](https://cozystack.io/) to display public telemetry on the [Telemetry page](https://cozystack.io/oss-health/telemetry/).

### How it works

1. The endpoint requires `year` and `month` query parameters (e.g. `/api/overview?year=2026&month=03`). Requests without them return 400.
2. On first request for a given month, the server queries VictoriaMetrics at the end of that month, writes a snapshot to `--snapshot-dir`, and caches it in memory. Subsequent requests for the same month are served from cache.
3. Concurrent requests for the same uncached month are coalesced into a single VictoriaMetrics query (per-month singleflight).
4. The app list is fetched from [cozystack/cozystack packages/apps](https://github.com/cozystack/cozystack/tree/main/packages/apps) so newly added applications are picked up automatically; a built-in fallback list is used if GitHub is unreachable.
5. The response aggregates snapshots into three time periods relative to the requested month: **that month**, **last quarter** (3 months), and **last 12 months**.

The snapshot directory is backed by an `emptyDir` volume — cache is per-pod and is rebuilt on restart from VictoriaMetrics on demand.

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
