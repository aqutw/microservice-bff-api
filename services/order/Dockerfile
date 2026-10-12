FROM golang:1.22 AS build
WORKDIR /src
COPY go.mod go.sum .
RUN go mod download
COPY main.go .
RUN CGO_ENABLED=0 go build -trimpath -o /out/order .
FROM gcr.io/distroless/static-debian12
WORKDIR /app
COPY --from=build /out/order /app/order
ENV PORT=8081 DB_PATH=/data/order.db
VOLUME ["/data"]
EXPOSE 8081
ENTRYPOINT ["/app/order"]
