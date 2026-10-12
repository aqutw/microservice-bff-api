FROM golang:1.22 AS build
WORKDIR /src
COPY go.mod ./
RUN go mod download
COPY main.go ./
RUN CGO_ENABLED=0 go build -trimpath -o /out/audit-worker .
FROM gcr.io/distroless/static-debian12
WORKDIR /app
COPY --from=build /out/audit-worker /app/audit-worker
ENV PORT=8084 DB_PATH=/data/audit.db
VOLUME ["/data"]
EXPOSE 8084
ENTRYPOINT ["/app/audit-worker"]
