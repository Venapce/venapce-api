# Multi-stage build for the venapce-api binary.
FROM golang:1.26 AS build
# The release tag, stamped into the binary (startup log + /api/version).
ARG VERSION=dev
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
RUN CGO_ENABLED=0 GOOS=linux go build \
    -ldflags="-s -w -X github.com/Venapce/venapce-api/internal/version.Version=${VERSION}" \
    -o /out/venapce-api ./cmd/api

FROM gcr.io/distroless/static-debian12:nonroot
COPY --from=build /out/venapce-api /venapce-api
EXPOSE 8080
USER nonroot:nonroot
ENTRYPOINT ["/venapce-api"]
