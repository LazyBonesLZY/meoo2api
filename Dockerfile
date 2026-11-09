FROM golang:1.22-alpine AS build
WORKDIR /src
COPY go.mod ./
COPY cmd ./cmd
COPY internal ./internal
RUN CGO_ENABLED=0 go build -trimpath -ldflags "-s -w" -o /out/meoo2api ./cmd/server

FROM alpine:3.20
RUN adduser -D -u 10001 meoo
WORKDIR /app
COPY --from=build /out/meoo2api /app/meoo2api
RUN mkdir -p /app/data && chown -R meoo /app
USER meoo
ENV BIND=0.0.0.0 PORT=8791 DATA_DIR=/app/data
VOLUME ["/app/data"]
EXPOSE 8791
ENTRYPOINT ["/app/meoo2api"]
