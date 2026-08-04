FROM golang:1.24.6-alpine3.22 AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
RUN CGO_ENABLED=0 GOOS=linux go build -trimpath -ldflags="-s -w" -o /out/remember-server ./cmd/remember-server \
    && mkdir -p /out/data/sqlite /out/data/blobs /out/data/staging \
    && chmod 700 /out/data/sqlite /out/data/blobs /out/data/staging

FROM gcr.io/distroless/static-debian12:nonroot
WORKDIR /app
COPY --from=build --chown=65532:65532 /out/remember-server /app/remember-server
COPY --from=build --chown=65532:65532 /out/data /data
ENV REMEMBER_LISTEN_ADDR=0.0.0.0:8080 \
    REMEMBER_DB_PATH=/data/sqlite/remember.db \
    REMEMBER_BLOB_ROOT=/data/blobs \
    REMEMBER_STAGING_PATH=/data/staging
EXPOSE 8080
USER 65532:65532
ENTRYPOINT ["/app/remember-server"]
