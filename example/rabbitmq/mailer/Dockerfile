FROM docker.io/library/golang:1.27.3-trixie@sha256:c124b9ad3452f46061b5054607d26ea51730dae5372c5e469920063ba2bc6dcc AS build
WORKDIR /src
COPY go.mod go.sum ./
COPY queue/rabbitmq/go.mod queue/rabbitmq/go.sum queue/rabbitmq/
COPY example/rabbitmq/mailer/go.mod example/rabbitmq/mailer/go.sum example/rabbitmq/mailer/
RUN go -C example/rabbitmq/mailer mod download
COPY . .
RUN CGO_ENABLED=0 go -C example/rabbitmq/mailer build -trimpath -ldflags=-s -o /out/mailer . && \
    CGO_ENABLED=0 go -C example/rabbitmq/mailer build -trimpath -ldflags=-s -o /out/mailrequest ./cmd/mailrequest

FROM gcr.io/distroless/static-debian13:nonroot@sha256:18f4cc26c9ffa7f20a359298be7deadac5c607c8e03a02b8119f1a5fc2f82c99
COPY --from=build /out/mailer /mailer
COPY --from=build /out/mailrequest /mailrequest
USER 65532:65532
ENTRYPOINT ["/mailer"]
