FROM gcr.io/distroless/base

LABEL maintainer="Bleemeo Docker Maintainers <packaging-team@bleemeo.com>"

COPY squirreldb-ingestor /usr/sbin/squirreldb-ingestor

CMD ["/usr/sbin/squirreldb-ingestor"]
