FROM golang:1.13.1 as builder
COPY . /go/src/github.com/tsuru/custom-cloudstack-ccm/
WORKDIR /go/src/github.com/tsuru/custom-cloudstack-ccm/
RUN CGO_ENABLED=0 GOOS=linux make build

FROM alpine:latest
RUN apk --no-cache add ca-certificates
WORKDIR /root/
COPY --from=builder /go/src/github.com/tsuru/custom-cloudstack-ccm/csccm .
CMD ["./csccm"]
