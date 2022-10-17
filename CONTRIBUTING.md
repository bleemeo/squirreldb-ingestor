## Build cache (optional)

Enable the cache to speed-up build and lint.
```sh
docker volume create squirreldb-ingestor-buildcache
```

## Test and Develop

To build binary you may use the `build.sh` script. For example to just compile a Go binary:
```sh
./build.sh go
```

Then run SquirrelDB Ingestor:
```sh
./squirreldb-ingestor
```

SquirrelDB Ingestor uses golangci-lint as linter. You may run it with:
```sh
./lint.sh
```

## Build a release

Our release version will be set from the current date.

The release build will
* Compile the Go binary for supported systems
* Build Docker images using Docker buildx

A builder needs to be created to build multi-arch images if it doesn't exist.
```sh
docker buildx create --name squirreldb-ingestor-builder
```

### Test release

To do a test release, run:
```sh
export INGESTOR_VERSION="$(date -u +%y.%m.%d.%H%M%S)"
export INGESTOR_BUILDX_OPTION="--builder squirreldb-ingestor-builder -t squirreldb-ingestor:latest --load"

./build.sh
unset INGESTOR_VERSION INGESTOR_BUILDX_OPTION
```

The release files are created in the `dist/` folder and a Docker image named `squirreldb-ingestor:latest` is built.

### Production release

For production releases, you will want to build the Docker image for multiple architectures, which requires to
push the image into a registry. Set image tags ("-t" options) to the wanted destination and ensure you
are authorized to push to the destination registry:
```sh
export INGESTOR_VERSION="$(date -u +%y.%m.%d.%H%M%S)"
export INGESTOR_BUILDX_OPTION="--builder squirreldb-ingestor-builder --platform linux/amd64,linux/arm64/v8,linux/arm/v7 -t squirreldb-ingestor:latest -t squirreldb-ingestor:${INGESTOR_VERSION} --push"

./build.sh
unset INGESTOR_VERSION INGESTOR_BUILDX_OPTION
```

## Run SquirrelDB Ingestor

On Linux amd64, after building the release you may run it with:

```sh
./dist/squirreldb-ingestor_linux_amd64_v1/squirreldb-ingestor
```
