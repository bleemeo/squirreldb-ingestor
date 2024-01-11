#!/bin/sh

set -e


GORELEASER_VERSION="v1.23.0"
USER_UID=$(id -u)

case "$1" in
   "")
      ;;
   "go")
      ONLY_GO=1
      ;;
   "race")
      ONLY_GO=1
      WITH_RACE=1
      ;;
   *)
      echo "Usage: $0 [go|race]"
      echo "  go: only compile Go"
      echo "  race: only compile Go with race detector"
      exit 1
esac

if docker volume ls | grep -q squirreldb-ingestor-buildcache; then
   GO_MOUNT_CACHE="-v squirreldb-ingestor-buildcache:/go/pkg"
fi

if [ -z "${SQUIRRELDB_INGESTOR_VERSION}" ]; then
   SQUIRRELDB_INGESTOR_VERSION=$(date -u +%y.%m.%d.%H%M%S)
fi

if [ -z "${SQUIRRELDB_INGESTOR_BUILDX_OPTION}" ]; then
   SQUIRRELDB_INGESTOR_BUILDX_OPTION="-t squirreldb-ingestor:latest --load"
fi

COMMIT=`git rev-parse --short HEAD || echo "unknown"`

if [ "${ONLY_GO}" = "1" -a "${WITH_RACE}" != "1" ]; then
   docker run --rm -e HOME=/go/pkg -e CGO_ENABLED=0 \
      -v $(pwd):/src -w /src ${GO_MOUNT_CACHE} \
      --entrypoint '' \
      goreleaser/goreleaser:${GORELEASER_VERSION} \
      tini -g -- sh -exc "
      go build -ldflags='-X main.version=${SQUIRRELDB_INGESTOR_VERSION} -X main.commit=${COMMIT}' .
      chown $USER_UID squirreldb-ingestor
      "
elif [ "${ONLY_GO}" = "1" -a "${WITH_RACE}" = "1"  ]; then
   docker run --rm -e HOME=/go/pkg -e CGO_ENABLED=1 \
      -v $(pwd):/src -w /src ${GO_MOUNT_CACHE} \
      --entrypoint '' \
      goreleaser/goreleaser:${GORELEASER_VERSION} \
      tini -g -- sh -exc "
      go build -ldflags='-X main.version=${SQUIRRELDB_INGESTOR_VERSION} -X main.commit=${COMMIT} -linkmode external -extldflags=-static' -race .
      chown $USER_UID squirreldb-ingestor
      "
else
   docker run --rm -e HOME=/go/pkg -e CGO_ENABLED=0 \
      -v $(pwd):/src -w /src ${GO_MOUNT_CACHE} \
      -v /var/run/docker.sock:/var/run/docker.sock \
      --entrypoint '' \
      -e GORELEASER_PREVIOUS_TAG=0.1.0 \
      -e GORELEASER_CURRENT_TAG=0.1.1 \
      goreleaser/goreleaser:${GORELEASER_VERSION} \
      tini -g -- sh -exc "
      mkdir -p /go/pkg
      git config --global --add safe.directory /src
      goreleaser check
      go test ./...
      goreleaser --rm-dist --snapshot --parallelism 2
      chown -R $USER_UID dist
      "

   echo $SQUIRRELDB_INGESTOR_VERSION > dist/VERSION

   # Build Docker image using buildx. We use docker buildx instead of goreleaser because
   # goreleaser use "docker manifest" which require to push image to a registry. This means we ends with 4 tags:
   # 3 for each of the 3 supported architectures and 1 for the multi-architecture image.
   # Using buildx only generate 1 tag on the Docker Hub.
   docker buildx build ${SQUIRRELDB_INGESTOR_BUILDX_OPTION} .
fi
