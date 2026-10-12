FROM golang:1.22 AS build
WORKDIR /src
COPY go.mod go.sum .
RUN go mod download
COPY main.go .
RUN CGO_ENABLED=0 go build -trimpath -o /out/payment .
FROM gcr.io/distroless/static-debian12
WORKDIR /app
COPY --from=build /out/payment /app/payment
ENV PORT=8083 DB_PATH=/data/payment.db
VOLUME ["/data"]
EXPOSE 8083
ENTRYPOINT ["/app/payment"]
