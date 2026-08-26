FROM golang:1.26 AS builder

WORKDIR /app

COPY go.mod go.sum ./
RUN go mod download

COPY . .

RUN CGO_ENABLED=0 GOOS=linux go build \
    -o worker ./cmd/worker

# Install the playwright CLI at the exact version pinned in go.mod, used
# below to install the matching driver + browsers.
RUN PWGO_VER=$(grep -oE "playwright-go v\S+" go.mod | sed 's/playwright-go //g') \
    && go install github.com/mxschmitt/playwright-go/cmd/playwright@${PWGO_VER}

# -----------------------------------

FROM ubuntu:noble

WORKDIR /app

COPY --from=builder /app/worker .
COPY --from=builder /go/bin/playwright /usr/local/bin/playwright

RUN apt-get update && apt-get install -y ca-certificates tzdata xvfb \
    && playwright install --with-deps chromium \
    && rm -rf /var/lib/apt/lists/*

# The worker launches Chromium headed (WhoScored blocks/challenges headless
# browsers), so it needs a virtual display to run inside a container.
# xvfb-run's SIGUSR1 readiness handshake isn't reliable in all container
# runtimes, so start Xvfb directly instead and give it a moment to come up.
CMD Xvfb :99 -screen 0 1280x1024x24 -nolisten tcp & sleep 1 && DISPLAY=:99 ./worker
