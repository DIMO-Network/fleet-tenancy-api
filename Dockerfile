FROM golang:1.26 AS build

ARG COMMIT_HASH=dev

RUN useradd -u 10001 dimo

WORKDIR /go/src/github.com/DIMO-Network/fleet-tenancy-api/
COPY . /go/src/github.com/DIMO-Network/fleet-tenancy-api/

ENV CGO_ENABLED=0
ENV GOOS=linux

RUN go mod download
RUN make build COMMIT=${COMMIT_HASH}

# busybox, not distroless, and the shell is the reason.
#
# Every DIMO Job and CronJob has to shut the linkerd proxy down by hand —
# `wget --post-data '' http://localhost:4191/shutdown` — because an injected
# proxy outlives the main container and the pod would otherwise never
# terminate. Meshing is not optional either: identity-api authorises on mesh
# identity and returns 403 to an unmeshed caller. A distroless image has neither
# a shell nor wget, which leaves any Job here with no way to finish. The backfill
# has to run as one, so this matters in practice, not in theory.
#
# The trade is a shell in the image. That is the standing org trade-off, matching
# kaufmann-oracle and fleet-lite-app, and consistency is worth more here than a
# smaller attack surface on a service with no ingress.
FROM busybox AS package

LABEL maintainer="DIMO <hello@dimo.zone>"

WORKDIR /

COPY --from=build /etc/ssl/certs/ca-certificates.crt /etc/ssl/certs/
COPY --from=build /etc/passwd /etc/passwd
COPY --from=build /go/src/github.com/DIMO-Network/fleet-tenancy-api/target/bin/fleet-tenancy-api .
COPY --from=build /go/src/github.com/DIMO-Network/fleet-tenancy-api/internal/db/migrations /internal/db/migrations

USER dimo

EXPOSE 8084
EXPOSE 8085

CMD ["/fleet-tenancy-api"]
