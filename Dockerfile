FROM golang:1.22-alpine AS builder

WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .

RUN CGO_ENABLED=0 GOOS=linux go build -ldflags="-s -w" -o /agent   ./cmd/agent
RUN CGO_ENABLED=0 GOOS=linux go build -ldflags="-s -w" -o /controller ./cmd/controller

# --- Agent image ---
FROM gcr.io/distroless/static:nonroot AS agent
COPY --from=builder /agent /agent
ENTRYPOINT ["/agent"]

# --- Controller image ---
FROM gcr.io/distroless/static:nonroot AS controller
COPY --from=builder /controller /controller
ENTRYPOINT ["/controller"]
