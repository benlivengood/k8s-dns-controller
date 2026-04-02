FROM golang:1.24-alpine AS builder

WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .

RUN CGO_ENABLED=0 GOOS=linux go build -ldflags="-s -w" -o /agent      ./cmd/agent
RUN CGO_ENABLED=0 GOOS=linux go build -ldflags="-s -w" -o /controller ./cmd/controller
RUN CGO_ENABLED=0 GOOS=linux go build -ldflags="-s -w" -o /prober     ./cmd/prober

# --- Agent image ---
FROM gcr.io/distroless/static:nonroot AS agent
COPY --from=builder /agent /agent
ENTRYPOINT ["/agent"]

# --- Controller image ---
FROM gcr.io/distroless/static:nonroot AS controller
COPY --from=builder /controller /controller
ENTRYPOINT ["/controller"]

# --- Prober image ---
FROM gcr.io/distroless/static:nonroot AS prober
COPY --from=builder /prober /prober
ENTRYPOINT ["/prober"]
