FROM golang:1.26 AS build
WORKDIR /src
COPY go.mod go.sum* ./
RUN go mod download
COPY . .
RUN CGO_ENABLED=0 go build -ldflags "-X main.commitHash=$(git rev-parse HEAD 2>/dev/null || echo unknown)" \
    -o /out/fleet-tenancy-api ./cmd/fleet-tenancy-api

FROM gcr.io/distroless/static-debian12
COPY --from=build /out/fleet-tenancy-api /fleet-tenancy-api
COPY --from=build /src/internal/db/migrations /internal/db/migrations
USER nonroot:nonroot
ENTRYPOINT ["/fleet-tenancy-api"]
