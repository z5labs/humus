FROM docker.io/library/golang:1.27.4-trixie@sha256:b54b2639de8c1bfe538ec5ffc45f29b1932235ac5f6d63c059853646677112d9 AS build
WORKDIR /src
COPY go.mod go.sum ./
COPY queue/rabbitmq/go.mod queue/rabbitmq/go.sum queue/rabbitmq/
COPY example/rabbitmq/mailer/go.mod example/rabbitmq/mailer/go.sum example/rabbitmq/mailer/
RUN go -C example/rabbitmq/mailer mod download
COPY . .
RUN CGO_ENABLED=0 go -C example/rabbitmq/mailer build -trimpath -ldflags=-s -o /out/mailer . && \
    CGO_ENABLED=0 go -C example/rabbitmq/mailer build -trimpath -ldflags=-s -o /out/mailrequest ./cmd/mailrequest

FROM gcr.io/distroless/static-debian13:nonroot@sha256:25ac8f0c3b2334a9eb05452266bd4697864a8c8ef0ef71e5e6480d7f92868329
COPY --from=build /out/mailer /mailer
COPY --from=build /out/mailrequest /mailrequest
USER 65532:65532
ENTRYPOINT ["/mailer"]
