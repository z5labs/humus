FROM docker.io/library/golang:1.27.4-trixie@sha256:f18c3d73a184fb0582828510722e723b63a93f7188d11dd223d9be6f6880e65d AS build
WORKDIR /src
COPY go.mod go.sum ./
COPY queue/rabbitmq/go.mod queue/rabbitmq/go.sum queue/rabbitmq/
COPY example/rabbitmq/mailer/go.mod example/rabbitmq/mailer/go.sum example/rabbitmq/mailer/
RUN go -C example/rabbitmq/mailer mod download
COPY . .
RUN CGO_ENABLED=0 go -C example/rabbitmq/mailer build -trimpath -ldflags=-s -o /out/mailer . && \
    CGO_ENABLED=0 go -C example/rabbitmq/mailer build -trimpath -ldflags=-s -o /out/mailrequest ./cmd/mailrequest

FROM gcr.io/distroless/static-debian13:nonroot@sha256:aafe993665bf6200db38a02c11a9351e782a84b6a45e7f33b6695638c88e3931
COPY --from=build /out/mailer /mailer
COPY --from=build /out/mailrequest /mailrequest
USER 65532:65532
ENTRYPOINT ["/mailer"]
