FROM golang:1.26-alpine AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
RUN CGO_ENABLED=0 go build -ldflags "-s -w" -o /out/hpscan ./cmd/hpscan

FROM alpine:3.20
COPY --from=build /out/hpscan /usr/local/bin/hpscan
# Config is mounted at /config/config.yaml, scans are written to /scans.
ENV HPSCAN_CONFIG=/config/config.yaml
VOLUME ["/config", "/scans"]
ENTRYPOINT ["hpscan"]
CMD ["run"]
