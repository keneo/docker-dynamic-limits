FROM golang:1.21-alpine AS build

RUN apk add --no-cache gcc musl-dev

WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download

COPY . .

RUN CGO_ENABLED=1 go build -o /ddld ./cmd/ddld
RUN CGO_ENABLED=0 go build -o /ddl-guest ./cmd/ddl-guest

FROM alpine:latest
RUN apk add --no-cache sqlite-libs curl
RUN mkdir -p /run/ddl
COPY --from=build /ddld /ddld
COPY --from=build /ddl-guest /ddl-guest
VOLUME /data
EXPOSE 7123
ENTRYPOINT ["/ddld"]
