# Start with busybox, but with libc.so.6
FROM busybox:ubuntu-14.04

MAINTAINER Michael Stapelberg <michael@robustirc.net>

# So that we can run as unprivileged user inside the container.
RUN echo 'nobody:x:99:99:nobody:/:/bin/sh' >> /etc/passwd

USER nobody

ADD ca-certificates.crt /etc/ssl/certs/ca-certificates.crt
ADD throughput /usr/bin/throughput

ENTRYPOINT ["/usr/bin/throughput"]
