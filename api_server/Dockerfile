FROM golang:1.24-alpine AS builder

WORKDIR /build

COPY go.mod go.sum ./
RUN go mod download

COPY . .
RUN CGO_ENABLED=0 go build -o /build/latest/meko-api-server .

FROM alpine:3.21

RUN apk add --no-cache ca-certificates

COPY --from=builder /build/latest/meko-api-server /usr/local/bin/meko-api-server

EXPOSE 25433

ENTRYPOINT ["/usr/local/bin/meko-api-server"]
