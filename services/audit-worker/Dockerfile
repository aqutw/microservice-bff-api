FROM golang:1.27.2 AS build
WORKDIR /src/audit-worker
COPY audit-worker/go.mod audit-worker/go.sum ./
COPY security ../security
RUN go mod download
COPY audit-worker/*.go ./
RUN CGO_ENABLED=0 go build -trimpath -o /out/audit-worker .
FROM gcr.io/distroless/static-debian12
WORKDIR /app
COPY --from=build /out/audit-worker /app/audit-worker
ENV PORT=8084 DB_PATH=/data/audit.db
VOLUME ["/data"]
EXPOSE 8084
ENTRYPOINT ["/app/audit-worker"]
