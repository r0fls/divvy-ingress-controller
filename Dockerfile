FROM golang:stretch

RUN mkdir -p /go/src/github.com/r0fls/divvy-ingress-controller
WORKDIR /go/src/github.com/r0fls/divvy-ingress-controller
COPY . .
RUN go build
CMD ["./divvy-ingress-controller"]
