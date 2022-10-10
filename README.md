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

-  `--log-level`, env: `INGESTOR_LOG_LEVEL` , default: info  
Set the log level. The available levels are: trace, debug, info, warn, error, fatal, panic and disabled.  

### Remote storage

-  `--remote-write-url`, env: `INGESTOR_REMOTE_WRITE_URL`, default: http://localhost:9201/api/v1/write  
The Prometheus remote write API URL.

### MQTT

-  `--mqtt-broker-url`, env: `INGESTOR_MQTT_BROKER_URL`, default: tcp://localhost:1883  
The MQTT Broker URL, must begin by `tcp://`, or `ssl://`.

-  `--mqtt-username`, `--mqtt-password`, env: `INGESTOR_MQTT_USERNAME`, `INGESTOR_MQTT_PASSWORD`  
The credentials used to authenticate with MQTT. Note that the username is also used as the client ID, so according to MQTT v3.1 specification, it should not be longer than 23 characters.

-  `--mqtt-ssl-insecure`, env: `INGESTOR_MQTT_SSL_INSECURE`  
Allow insecure SSL connection.

-  `--mqtt-ca-file`, env: `INGESTOR_MQTT_CA_FILE`  
Provide your own SSL certificate, this should be the path to a PEM file.

## Contributing

See [CONTRIBUTING.md](CONTRIBUTING.md).
