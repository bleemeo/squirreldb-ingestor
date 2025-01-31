FROM --platform=$BUILDPLATFORM alpine:3.21 AS build

ARG TARGETARCH

COPY dist/squirreldb-ingestor_linux_amd64_v1/squirreldb-ingestor /squirreldb-ingestor.amd64
COPY dist/squirreldb-ingestor_linux_arm64_v8.0/squirreldb-ingestor /squirreldb-ingestor.arm64
COPY dist/squirreldb-ingestor_linux_arm_6/squirreldb-ingestor /squirreldb-ingestor.arm

RUN cp -p /squirreldb-ingestor.$TARGETARCH /squirreldb-ingestor

FROM gcr.io/distroless/base

LABEL maintainer="Bleemeo Docker Maintainers <packaging-team@bleemeo.com>"

COPY --from=build /squirreldb-ingestor /usr/sbin/squirreldb-ingestor

CMD ["/usr/sbin/squirreldb-ingestor"]
