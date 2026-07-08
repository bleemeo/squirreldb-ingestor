# Load bench

This directory contains a load generator that simulates a fleet of
[Glouton](https://github.com/bleemeo/glouton) agents: each simulated agent opens its own MQTT
connection and publishes batches of points at a fixed interval, with the same payload format as
the real agent (zlib compressed JSON, QoS 1).

The goal is to characterize the pipeline itself — sustained throughput, end-to-end latency and
recovery after an outage — not to compare it with a pull-based Prometheus setup: the two models
make different trade-offs (see the main README), and such a comparison would mostly measure the
storage.

## Running

Start a NATS server with MQTT enabled, a remote write storage and an ingestor. For a quick local
setup:

```sh
# nats.conf: server_name, jetstream {} and mqtt { port: 1883 } are enough.
docker run -d --name bench-nats -p 1883:1883 -v $(pwd)/nats.conf:/etc/nats/ns.conf:ro nats -c /etc/nats/ns.conf
docker run -d --name bench-prom -p 9090:9090 prom/prometheus --web.enable-remote-write-receiver \
    --config.file=/etc/prometheus/prometheus.yml
./squirreldb-ingestor --mqtt-broker-url tcp://localhost:1883 --remote-write-url http://localhost:9090/api/v1/write
```

Then run the generator, for example 200 agents publishing 200 points every second (40000
points/s offered), with end-to-end latency probes:

```sh
go run ./bench --broker tcp://localhost:1883 --agents 200 --points 200 --interval 1s \
    --duration 60s --query-url http://localhost:9090
```

The generator reports the published points and the latency percentiles. The ingestor side is
observable on its `/metrics` endpoint (`ingestor_points_written_total`,
`ingestor_write_request_duration_seconds`...).

With `--query-url` set, a probe point is published every second and the generator polls the
storage query API until it is visible: the reported latency is the full path — MQTT publish,
NATS, ingestor batching, remote write and storage ingestion.

## Indicative results

Measured on a single laptop (Apple Silicon, NATS 2.12 and Prometheus in Docker Desktop, one
ingestor, generator on the same machine) — indicative orders of magnitude, not a controlled
benchmark. Real numbers depend mostly on the remote storage.

| Scenario | Result |
|---|---|
| 100 agents × 100 points / 1s (10k points/s) | Sustained, no backlog. Latency p50 181 ms, p95 200 ms. |
| 200 agents × 200 points / 1s (40k points/s) | Sustained. Latency p50 286 ms. |
| 250 agents × 400 points / 1s (100k points/s) | Sustained. Latency p50 503 ms. Ingestor at 4.6% of one core. |
| 250 agents × 800 points / 1s (200k points/s) | Sustained. Ingestor at 7.8% of one core, ~28 MB RSS. Bottleneck was elsewhere in the test rig. |
| 30 s storage outage under 40k points/s | 1.2M points buffered in NATS JetStream, drained in ~5 s after recovery (~320k points/s peak). No point lost. |
| 1000 concurrent agents (50 points / 10s each) | No connection failure. NATS at 0.2% CPU, 115 MB. Latency p50 512 ms. |

Across all the runs, the number of points written to the storage matched the number of points
published: at-least-once delivery held, with no visible duplicates in steady state.

Two caveats worth keeping in mind when scaling further:

- The end-to-end latency is dominated by the ingestor batching (a write is sent when 10000
  points are pending or after at most one second), which is why it grows slightly with the
  batch fill rate. The agent's own collection interval (10 s for Glouton by default) remains
  the largest contribution in a real deployment.
- Above what a single ingestor sustains, partition the agents across several ingestors with
  deterministic NATS subject mapping (see the main README).
