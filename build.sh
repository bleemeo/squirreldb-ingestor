#!/bin/sh

set -e

GORELEASER_VERSION="v2.17.0"
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

# goreleaser needs tags to compute the version and the changelog. Use the
# git tags when they exist, with a fallback for repositories without tags.
if [ -z "${GORELEASER_CURRENT_TAG}" ]; then
   GORELEASER_CURRENT_TAG=$(git describe --tags --abbrev=0 2>/dev/null || echo 0.1.0)
fi

if [ -z "${GORELEASER_PREVIOUS_TAG}" ]; then
   GORELEASER_PREVIOUS_TAG=$(git describe --tags --abbrev=0 "${GORELEASER_CURRENT_TAG}^" 2>/dev/null || echo "${GORELEASER_CURRENT_TAG}")
fi

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
      -e GORELEASER_PREVIOUS_TAG="${GORELEASER_PREVIOUS_TAG}" \
      -e GORELEASER_CURRENT_TAG="${GORELEASER_CURRENT_TAG}" \
      goreleaser/goreleaser:${GORELEASER_VERSION} \
      tini -g -- sh -exc "
      mkdir -p /go/pkg
      git config --global --add safe.directory /src
      goreleaser check
      go test ./...
      goreleaser --clean --snapshot --parallelism 2
      chown -R $USER_UID dist
      "

   echo $SQUIRRELDB_INGESTOR_VERSION > dist/VERSION

   # Build Docker image using buildx. We use docker buildx instead of goreleaser because
   # goreleaser use "docker manifest" which require to push image to a registry. This means we ends with 4 tags:
   # 3 for each of the 3 supported architectures and 1 for the multi-architecture image.
   # Using buildx only generate 1 tag on the Docker Hub.
   docker buildx build ${SQUIRRELDB_INGESTOR_BUILDX_OPTION} .
fi
