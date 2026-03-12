FROM golang:1.25-bookworm

# gcc is needed to compile assembly test targets (.s files).
RUN apt-get update && apt-get install -y --no-install-recommends \
        gcc libc6-dev \
    && rm -rf /var/lib/apt/lists/*

WORKDIR /workspace
