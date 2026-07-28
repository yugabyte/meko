# Build the YCP control-plane image (cmd/ycp).
# Built and pushed as 161908593886.dkr.ecr.us-east-2.amazonaws.com/meko/ycp.
#
# The console (web/console.html) is compiled in via go:embed, so the runtime
# image needs nothing but the binary.
FROM golang:1.25-alpine AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
RUN CGO_ENABLED=0 go build -trimpath -ldflags "-s -w" -o /ycp ./cmd/ycp
FROM gcr.io/distroless/static:nonroot
COPY --from=build /ycp /ycp
EXPOSE 8080
ENTRYPOINT ["/ycp"]
