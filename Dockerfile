FROM golang:1.26 AS build
WORKDIR /src
COPY go.mod go.sum* ./
RUN go mod download
COPY . .
# Passed by the build workflow. The previous `git rev-parse` here resolved
# against whatever .git happened to be in the build context, so /version
# reported "unknown" in any build that excluded it — including the image the
# workflow actually ships.
ARG COMMIT_HASH=unknown
RUN CGO_ENABLED=0 go build -ldflags "-X main.commitHash=${COMMIT_HASH}" \
    -o /out/fleet-tenancy-api ./cmd/fleet-tenancy-api

FROM gcr.io/distroless/static-debian12
COPY --from=build /out/fleet-tenancy-api /fleet-tenancy-api
COPY --from=build /src/internal/db/migrations /internal/db/migrations
USER nonroot:nonroot
ENTRYPOINT ["/fleet-tenancy-api"]
