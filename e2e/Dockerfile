# Stage 1: Build ddld binary and e2e test binary
FROM golang:1.22-alpine AS build

RUN apk add --no-cache gcc musl-dev

WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download

COPY . .

# Build the daemon binary (CGO required for sqlite3)
RUN CGO_ENABLED=1 go build -o /ddld ./cmd/ddld

# Build the e2e test binary
RUN CGO_ENABLED=1 go test -c -tags e2e -o /e2e.test ./e2e/

# Stage 2: Runtime with Docker-in-Docker
FROM docker:27-dind

RUN apk add --no-cache bash

COPY --from=build /ddld /usr/local/bin/ddld
COPY --from=build /e2e.test /usr/local/bin/e2e.test
COPY e2e/entrypoint.sh /entrypoint.sh
RUN chmod +x /entrypoint.sh

ENV DOCKER_TLS_CERTDIR=""

ENTRYPOINT ["/entrypoint.sh"]
