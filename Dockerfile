FROM golang:1.21-alpine AS build

RUN apk add --no-cache gcc musl-dev

WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download

COPY . .

RUN CGO_ENABLED=1 go build -o /ddld ./cmd/ddld

FROM alpine:latest
RUN apk add --no-cache sqlite-libs
COPY --from=build /ddld /ddld
VOLUME /data
EXPOSE 7123
ENTRYPOINT ["/ddld"]
