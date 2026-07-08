# SquirrelDB Ingestor

[![Go Report Card](https://goreportcard.com/badge/github.com/bleemeo/squirreldb-ingestor)](https://goreportcard.com/report/github.com/bleemeo/squirreldb-ingestor)
[![License](https://img.shields.io/badge/license-Apache%202.0-blue.svg)](https://github.com/bleemeo/squirreldb-ingestor/blob/main/LICENSE)
[![Docker Image Version](https://img.shields.io/docker/v/bleemeo/squirreldb-ingestor)](https://hub.docker.com/r/bleemeo/squirreldb-ingestor/tags)
[![Docker Image Size](https://img.shields.io/docker/image-size/bleemeo/squirreldb-ingestor)](https://hub.docker.com/r/bleemeo/squirreldb-ingestor)

SquirrelDB Ingestor is the bridge of a **push-based monitoring pipeline**: it reads the metrics
that [Glouton](https://github.com/bleemeo/glouton) agents publish over MQTT and writes them to
[SquirrelDB](https://github.com/bleemeo/squirreldb) — or any other storage compatible with the
Prometheus remote write API.

![Architecture](docs/architecture.svg)

## Why push metrics over MQTT?

A classic Prometheus setup *pulls*: the server must be able to reach every agent, discover them,
and scrape each one on an interval. This pipeline reverses the flow — agents *push* their metrics
to a message broker, and the ingestor drains the broker into the storage. This changes a few
things that matter when you monitor a large or heterogeneous fleet:

- **Works behind NAT and firewalls.** Agents only open a single outbound MQTT connection (TLS
  supported). No inbound port to expose, no service discovery to maintain, no VPN to reach
  servers sitting at the edge, in branch offices or at customers' premises.

- **Scales with the number of agents.** A NATS cluster handles a very large number of persistent
  MQTT connections, and the write path scales horizontally: NATS subject mapping partitions the
  agents across as many ingestors as needed (see [Scaling the ingestor](#scaling-the-ingestor)).
  The ingestor itself is stateless and batches points before writing them.

- **Fresh data.** A point is pushed as soon as the agent produces it and written to the storage
  within about a second of its arrival. There is no scrape interval between a value being
  produced and being queryable — end-to-end latency is dominated by the agent's own collection
  interval, not by the pipeline.

- **No data loss.** Messages are published with MQTT QoS 1 and persisted by NATS JetStream. The
  ingestor acknowledges a message only once its points are written to the storage, so metrics
  survive an ingestor or storage outage: they are buffered in NATS and delivered when the
  pipeline recovers (see [Reliability](#reliability)).

- **Spoofing-resistant.** Each agent is only allowed to publish to its own topic (NATS
  authorization), and the ingestor overwrites the `instance` label of every point with the
  identity taken from the authenticated topic. A compromised agent can lie about its values, but
  not impersonate another server.

- **Storage agnostic.** The ingestor speaks standard Prometheus remote write: SquirrelDB,
  Prometheus (with the remote write receiver enabled), Mimir, Cortex, VictoriaMetrics, Thanos…
  Multi-tenant storages are supported through custom headers.

The main thing the pull model gives you that push does not is the free `up` metric: with push, a
silent agent produces no data instead of a scrape failure. Alert on the absence of data instead,
for example with `absent()` or a staleness expression such as:

```promql
time() - max by (instance) (timestamp(node_load1)) > 120
```

## Quick start

A complete Docker Compose with Glouton, NATS, SquirrelDB Ingestor, SquirrelDB and Grafana is
available in the [setup example](#setup-example). To run the ingestor alone:

```sh
docker run -d --name="squirreldb-ingestor" \
    -e INGESTOR_MQTT_BROKER_URL=tcp://<your-broker>:1883 \
    -e INGESTOR_REMOTE_WRITE_URL=http://<your-storage>:9201/api/v1/write \
    bleemeo/squirreldb-ingestor
```

Or grab the latest binary [release](https://github.com/bleemeo/squirreldb-ingestor/releases/latest)
and run it:

```sh
./squirreldb-ingestor --mqtt-broker-url tcp://<your-broker>:1883 \
    --remote-write-url http://<your-storage>:9201/api/v1/write
```

## Configuration

The options can be set with environment variables or command line arguments.

-  `--log-level`, env: `INGESTOR_LOG_LEVEL` , default: info  
Set the log level. The available levels are: trace, debug, info, warn, error, fatal, panic and disabled.  

### Remote storage

-  `--remote-write-url`, env: `INGESTOR_REMOTE_WRITE_URL`, default: http://localhost:9201/api/v1/write  
The Prometheus remote write API URL.

-  `--remote-write-username`, `--remote-write-password`, env: `INGESTOR_REMOTE_WRITE_USERNAME`, `INGESTOR_REMOTE_WRITE_PASSWORD`  
The credentials used for basic authentication with the remote storage. Credentials embedded in
the URL (`http://user:pass@host/api/v1/write`) are also supported. Prefer the environment
variable over the command line argument for the password.

-  `--remote-write-bearer-token`, env: `INGESTOR_REMOTE_WRITE_BEARER_TOKEN`  
The bearer token used to authenticate with the remote storage, exclusive with the basic
authentication.

-  `--remote-write-header`, env: `INGESTOR_REMOTE_WRITE_HEADER`  
An additional header sent with the write requests, of the form `Name=Value`. The argument can
be repeated (in the environment variable, separate the headers with commas). For example, to
write to a multi-tenant storage like Cortex or Mimir: `--remote-write-header X-Scope-OrgID=tenant1`.

### MQTT

-  `--mqtt-broker-url`, env: `INGESTOR_MQTT_BROKER_URL`, default: tcp://localhost:1883  
The MQTT Broker URL, must begin by `tcp://`, or `ssl://`.

-  `--mqtt-username`, `--mqtt-password`, env: `INGESTOR_MQTT_USERNAME`, `INGESTOR_MQTT_PASSWORD`  
The credentials used to authenticate with MQTT. Prefer the environment variable over the
command line argument for the password: command line arguments are visible to the other users
of the machine.

-  `--mqtt-client-id`, env: `INGESTOR_MQTT_CLIENT_ID`, default: the MQTT username  
The MQTT client ID. Two clients connecting with the same client ID disconnect each other,
so this option must be set when multiple ingestors share the same username.

-  `--mqtt-ssl-insecure`, env: `INGESTOR_MQTT_SSL_INSECURE`  
Allow insecure SSL connection.

-  `--mqtt-ca-file`, env: `INGESTOR_MQTT_CA_FILE`  
Provide your own SSL certificate, this should be the path to a PEM file.

- `--mqtt-id`, env: `INGESTOR_MQTT_ID`
If set, the ingestor listens to the MQTT topic `v1/agent/fqdn/data/MQTT_ID`. This is useful only if you use 
[NATS subject mapping](https://docs.nats.io/running-a-nats-service/configuration/configuring_subject_mapping), 
to configure the topic this ingestor should listen on. More details are available in
[Scaling the ingestor](#scaling-the-ingestor).

### Observability

-  `--http-address`, env: `INGESTOR_HTTP_ADDRESS`, default: 0.0.0.0:8080  
The address of the HTTP server exposing the ingestor internals. Set it to an empty string to
disable the HTTP server. Two endpoints are available:
   - `/metrics`: the ingestor metrics in the Prometheus format.
   - `/healthz`: returns HTTP 200 when the MQTT connection is established, and HTTP 503
     otherwise. It can be used as a liveness or readiness probe.

The following metrics are exposed, along with the standard Go and process collectors:

| Metric | Description |
|---|---|
| `ingestor_mqtt_connected` | Whether the MQTT connection is currently established. |
| `ingestor_mqtt_messages_received_total` | Messages received over MQTT. |
| `ingestor_mqtt_messages_dropped_total{reason}` | Messages dropped because they could not be processed. |
| `ingestor_points_received_total` | Points received over MQTT. |
| `ingestor_points_written_total` | Points successfully written to the remote storage. |
| `ingestor_points_dropped_total{reason}` | Points dropped because they could not be written. |
| `ingestor_write_request_duration_seconds` | Duration of the requests to the remote write API. |

## Reliability

The pipeline is designed so that a crash or an outage of any component doesn't lose metrics:

- Points are written in batches: a write request is sent as soon as 10000 points are pending, or
  after at most one second.
- MQTT messages are acknowledged only once their points have been written. If the ingestor is
  stopped or crashes while the storage is down, the unacknowledged messages stay in NATS
  JetStream and are delivered again on the next start (delivery is at-least-once; remote write
  storages handle the resulting duplicate points gracefully).
- Failed writes are retried with an exponential backoff (from 1 second up to 1 minute, with
  jitter so that several ingestors don't retry in lockstep), and the `Retry-After` header is
  honored when the remote storage sends one.
- Requests definitively rejected by the storage (HTTP 4xx) are not retried: their points are
  dropped and counted in `ingestor_points_dropped_total`, so a single malformed payload can
  never block the rest of the ingestion.

## Setup example

In this example, we are going to setup a Grafana dashboard from metrics sent by two Gloutons over MQTT.

A docker compose is available to start a monitoring stack with two Gloutons, SquirrelDB Ingestor, NATS, 
SquirrelDB and Grafana. It provides a NATS cluster with three nodes for high availability, and two ingestors.

```sh
cd examples/ingestor_ha
docker compose up -d
```

Now you can check that the ingestors are connected to MQTT. The logs should say `MQTT connection established`.
```sh
docker compose logs -f squirreldb-ingestor-1 squirreldb-ingestor-2
```

Also check that the agents connected to MQTT, you should see `Open Source MQTT connection established`:
```sh
docker compose logs -f glouton-1 glouton-2
```

Then go to the Grafana dashboard at http://localhost:3000/d/83ceCuenk/, and log in with the user
"admin" and the password "password". You can monitor all your servers from this dashboard.

### High availability

This setup provides a NATS cluster to have high availability for MQTT.

SquirrelDB, Cassandra and Redis are running with a single node in the example, 
but they can also be scaled, an example is available
[here](https://github.com/bleemeo/squirreldb/tree/master/examples/squirreldb_ha).

For true high availability, each component must run on a different node, which is not done by the docker compose.

## Connecting your agents

To connect your agent to the SquirrelDB Ingestor, you need to edit its configuration. Here is an example
configuration, you should change the hosts to the nodes running NATS, and edit the username and password.

```yaml
agent:
  metrics_format: prometheus

bleemeo:
  enable: false

mqtt:
  enable: true
  hosts: 
    - 127.0.0.1
  port: 1883
  username: server1
  password: passw0rd

# Allow node exporter metrics.
metric:
  allow_metrics:
    - node_*
```

To start a new agent with this configuration you can use Docker. The following command supposes
that you wrote the configuration to a file in the current directory called `glouton.conf`.

```sh
docker run -d --name="bleemeo-agent" \
    -v /var/lib/glouton:/var/lib/glouton -v /var/run/docker.sock:/var/run/docker.sock -v /:/hostroot:ro \
    -v $(pwd)/glouton.conf:/etc/glouton/conf.d/90-mqtt.conf \
    -e  GLOUTON_BLEEMEO_ENABLE='false' --pid=host --net=host \
    --cap-add SYS_PTRACE --cap-add SYS_ADMIN bleemeo/bleemeo-agent
```

## Authentication and authorizations with NATS

An example NATS cluster configuration is available in `examples/ingestor_ha/nats.conf`. 
For each new user, you need to add it to the `authorization` block of the configuration with the
right permissions to allow it to only publish to a single topic. 

Let's say we want to monitor two servers, `server1.example.com` and `server2.example.com`. 
The authorizations should look like this:

```conf
authorization {
    # Allow the ingestor to listen on the MQTT topic "v1/agent/+/data".
    # https://docs.nats.io/running-a-nats-service/configuration/mqtt#mqtt-topics-and-nats-subjects
    # We also need to allow "$MQTT.sub.>" because NATS uses this topic to store durable subscriptions.
    # https://docs.nats.io/running-a-nats-service/configuration/mqtt/mqtt_config#special-permissions
    ingestor_perms = {
        subscribe = ["v1.agent.*.data", "$MQTT.sub.>"]
    }

    # Glouton publishes its metrics to the `v1/agent/fqdn/data` topic, with `fqdn` replaced by the host FQDN.
    # `.` are replaced by `,` in the FQDN because NATS doesn't support `.` in MQTT topics.
    # On Linux, you can get your FQDN with "hostname -f".
    server1_perms = {
        publish = ["v1.agent.server1,example,com.data"]
    }

    server2_perms = {
        publish = ["v1.agent.server2,example,com.data"]
    }

    users = [
        {user: ingestor, password: passw0rd, permissions: $ingestor_perms, allowed_connection_types: ["MQTT"]}
        {user: server1, password: passw0rd, permissions: $server1_perms, allowed_connection_types: ["MQTT"]}
        {user: server2, password: passw0rd, permissions: $server2_perms, allowed_connection_types: ["MQTT"]}
    ]
}
```

## Scaling the ingestor

[NATS subject mapping](https://docs.nats.io/running-a-nats-service/configuration/configuring_subject_mapping)
can distribute the messages received on the data topic across several ingestors. Use
deterministic partitioning: each agent is assigned to a partition based on a hash of its FQDN, so
the points of a given server are always processed by the same ingestor and stay in order:

```conf
mappings = {
  # Distribute the agents across two partitions: "v1/agent/<fqdn>/data" is rewritten to
  # "v1/agent/<fqdn>/data/0" or "v1/agent/<fqdn>/data/1" based on a hash of the FQDN.
  v1.agent.*.data: "v1.agent.{{wildcard(1)}}.data.{{partition(2,1)}}"
}
```

Then run one ingestor per partition, with `--mqtt-id 0` and `--mqtt-id 1` (and distinct MQTT
usernames or `--mqtt-client-id` values). This requires nats-server 2.9.5 or later; the example
was tested with NATS 2.12.

Note that running multiple ingestors doesn't give you high availability: if one ingestor is
down, the messages of its partition won't be processed until it's restarted. This is not much of
an issue since the messages are buffered in NATS, so you won't lose any metric. Moreover, the
ingestor is stateless, so it can be restarted with the same configuration, which might be
automated by an orchestrator like Kubernetes (the `/healthz` endpoint can serve as the liveness
probe).

## Contributing

See [CONTRIBUTING.md](CONTRIBUTING.md).
