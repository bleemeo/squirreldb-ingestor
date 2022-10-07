# SquirrelDB Ingestor

SquirrelDB Ingestor reads metrics sent by Glouton over MQTT and store them in SquirrelDB 
(or any other components compatible with Prometheus remote write).

An example Docker Compose with Glouton, SquirrelDB and Grafana is available
[here](https://github.com/bleemeo/glouton/tree/master/examples/mqtt).


## Configuration

The options can be set with environment variables or command line arguments.

Options:
-  `--remotewriteurl` [default: http://localhost:9201/api/v1/write, env: INGESTOR_REMOTE_WRITE_URL]
-  `--mqttbrokerurl` [default: tcp://localhost:1883, env: INGESTOR_MQTT_BROKER_URL]
-  `--mqttusername` [env: INGESTOR_MQTT_USERNAME]
-  `--mqttpassword` [env: INGESTOR_MQTT_PASSWORD]

## Build

Enable cache to speed-up build and lint (optional).
```sh
docker volume create squirreldb-ingestor-buildcache
```

To build the ingestor binary, use the provided script.
```sh
./build.sh
```

## Lint

Glouton uses golangci-lint as linter. You may run it with:
```sh
./lint.sh
```
