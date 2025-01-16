FROM golang:1.23-alpine AS builder
WORKDIR /app
COPY go.mod go.sum ./
RUN go mod download
COPY main.go main.go
RUN CGO_ENABLED=0 GOOS=linux go build -a -installsuffix cgo -o /cozy-telemetry-server

FROM scratch

COPY --from=builder /cozy-telemetry-server /cozy-telemetry-server

ENTRYPOINT ["/cozy-telemetry-server"]
