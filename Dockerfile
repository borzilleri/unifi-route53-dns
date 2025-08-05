FROM golang:1.24-alpine as builder

RUN apk add build-base

WORKDIR /app

COPY go.mod ./

ENV GIN_MODE=release

RUN --mount=type=cache,target=/go/pkg/mod \
  --mount=type=cache,target=root/.cache/go-build \
  go mod download

COPY . .

ENV CGO_ENABLED=1
ENV CC=gcc

RUN go build \
  -ldflags="-linkmode external -extldflags -static" \
  -tags netgo \
  -o app

FROM scratch

ENV UNIFI_R53_DNS_APP_PORT=8080

COPY --from=builder /etc/passwd /etc/passwd
COPY --from=builder /app/app /app

USER guest

EXPOSE ${UNIFI_R53_DNS_APP_PORT}

CMD ["./app"]
