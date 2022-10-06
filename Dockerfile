FROM busybox

LABEL MAINTAINER="Bleemeo Docker Maintainers <packaging-team@bleemeo.com>"

COPY consumer /usr/sbin/consumer

CMD ["/usr/sbin/consumer"]
