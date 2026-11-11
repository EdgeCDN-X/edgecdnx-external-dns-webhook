FROM golang:1.25-alpine AS build

RUN apk add --no-cache git
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY *.go ./
RUN CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o /out/edgecdnx-external-dns-webhook .

FROM alpine:3.22
RUN apk add --no-cache ca-certificates && addgroup -S -g 65532 webhook && adduser -S -D -H -u 65532 -G webhook webhook
COPY --from=build /out/edgecdnx-external-dns-webhook /usr/local/bin/edgecdnx-external-dns-webhook
USER 65532:65532
EXPOSE 8888
ENTRYPOINT ["/usr/local/bin/edgecdnx-external-dns-webhook"]
