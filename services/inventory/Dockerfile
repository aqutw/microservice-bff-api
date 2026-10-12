FROM golang:1.22 AS build
WORKDIR /src
COPY go.mod go.sum .
RUN go mod download
COPY main.go .
RUN CGO_ENABLED=0 go build -trimpath -o /out/inventory .
FROM gcr.io/distroless/static-debian12
WORKDIR /app
COPY --from=build /out/inventory /app/inventory
ENV PORT=8082 DB_PATH=/data/inventory.db
VOLUME ["/data"]
EXPOSE 8082
ENTRYPOINT ["/app/inventory"]
