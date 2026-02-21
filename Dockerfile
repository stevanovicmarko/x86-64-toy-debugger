# ----- Builder stage -----
FROM golang:1.25-bookworm AS builder
WORKDIR /src

COPY go.mod go.sum ./
RUN go mod download

COPY . .
RUN go build -o /out/toydbg ./cmd/toydbg
RUN go test ./...

# ----- Runtime stage -----
FROM debian:bookworm-slim
COPY --from=builder /out/toydbg /usr/local/bin/toydbg

ENTRYPOINT ["toydbg"]
