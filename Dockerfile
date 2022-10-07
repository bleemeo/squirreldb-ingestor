FROM gcr.io/distroless/base

LABEL maintainer="Bleemeo Docker Maintainers <packaging-team@bleemeo.com>"

COPY consumer /usr/sbin/consumer

CMD ["/usr/sbin/consumer"]
