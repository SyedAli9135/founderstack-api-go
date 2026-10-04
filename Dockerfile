# syntax=docker/dockerfile:1
FROM golang:1.26-alpine AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
RUN CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o /out/api ./cmd/api \
 && CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o /out/rotatekeys ./cmd/rotatekeys

# Migrations are applied separately (golang-migrate) before a release, never by the API at boot.
FROM gcr.io/distroless/static-debian12:nonroot
COPY --from=build /out/api /out/rotatekeys /app/
USER nonroot
EXPOSE 8000
ENTRYPOINT ["/app/api"]
