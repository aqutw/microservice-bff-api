FROM golang:1.27.2 AS build
WORKDIR /src/inventory
COPY inventory/go.mod inventory/go.sum ./
COPY security ../security
RUN go mod download
COPY inventory/*.go ./
RUN CGO_ENABLED=0 go build -trimpath -o /out/inventory .
FROM gcr.io/distroless/static-debian12
WORKDIR /app
COPY --from=build /out/inventory /app/inventory
ENV PORT=8082 DB_PATH=/data/inventory.db
VOLUME ["/data"]
EXPOSE 8082
ENTRYPOINT ["/app/inventory"]
