FROM golang:1.26-alpine AS builder
WORKDIR /app
COPY server/ .
RUN go build -o diskdashboard-server .

FROM alpine:3.21
RUN apk add --no-cache ca-certificates smartmontools
WORKDIR /app
COPY --from=builder /app/diskdashboard-server .
EXPOSE 3000
CMD ["./diskdashboard-server"]
