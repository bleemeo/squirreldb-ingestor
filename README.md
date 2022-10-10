# SquirrelDB Ingestor

SquirrelDB Ingestor reads metrics sent by [Glouton](https://github.com/bleemeo/glouton) over MQTT 
and store  them in [SquirrelDB](https://github.com/bleemeo/squirreldb) (or any other component
compatible with Prometheus remote write).

An example Docker Compose with Glouton, SquirrelDB and Grafana is available
[here](https://github.com/bleemeo/glouton/tree/master/examples/mqtt).

## Run

SquirrelDB Ingestor can be run with Docker or as a binary.

### Docker

```sh
docker run -d --name="squirreldb-ingestor" bleemeo/squirreldb-ingestor
```

### Binary

Grab the latest binary [release](https://github.com/bleemeo/squirreldb-ingestor/releases/latest) and run it:

```sh
./squirreldb-ingestor
```

## Configuration

The options can be set with environment variables or command line arguments.

Options:
-  `--remotewriteurl` [default: http://localhost:9201/api/v1/write, env: INGESTOR_REMOTE_WRITE_URL]
-  `--mqttbrokerurl` [default: tcp://localhost:1883, env: INGESTOR_MQTT_BROKER_URL]
-  `--mqttusername` [env: INGESTOR_MQTT_USERNAME]
-  `--mqttpassword` [env: INGESTOR_MQTT_PASSWORD]

## Contributing

See [CONTRIBUTING.md](CONTRIBUTING.md).
