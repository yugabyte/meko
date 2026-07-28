# Build the per-datapack MCP server (dpmcp) image.
FROM golang:1.25-alpine AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
RUN CGO_ENABLED=0 go build -trimpath -ldflags "-s -w" -o /dpmcp ./cmd/dpmcp
FROM gcr.io/distroless/static:nonroot
COPY --from=build /dpmcp /dpmcp
EXPOSE 3000
ENTRYPOINT ["/dpmcp"]
