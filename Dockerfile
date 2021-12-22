FROM golang:1.16.5-alpine AS builder
WORKDIR /go/src/github.com/and3rson/paast
COPY go.mod go.sum ./
RUN go mod download -x
COPY *.go .
RUN go build -o /paast

FROM alpine:3.14
RUN apk add tzdata && \
    cp /usr/share/zoneinfo/Europe/Kiev /etc/localtime && \
    echo "Europe/Kiev" > /etc/timezone && \
    rm -rf /var/cacke/apk
COPY --from=builder /paast /usr/bin/
CMD ["paast"]
