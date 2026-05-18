FROM golang:1.26-alpine AS builder

WORKDIR /app
RUN apk add --no-cache git
COPY go.mod go.sum ./
RUN go mod download
COPY . .
RUN CGO_ENABLED=0 go build \
    -trimpath \
    -buildvcs=true \
    -ldflags "-s -w" \
    -o /out/runnerd \
    ./cmd/runnerd

FROM gcr.io/distroless/static:nonroot

ENV HTTP_ADDR=:8080 \
    STATE_DIR=/tmp/e2b-github-runner/runners

COPY --from=builder /out/runnerd /usr/local/bin/runnerd

EXPOSE 8080
ENTRYPOINT ["/usr/local/bin/runnerd"]
