FROM docker.io/library/golang:1.27.4-trixie@sha256:b54b2639de8c1bfe538ec5ffc45f29b1932235ac5f6d63c059853646677112d9 AS build
WORKDIR /src
COPY go.mod go.sum ./
COPY queue/kafka/go.mod queue/kafka/go.sum queue/kafka/
COPY example/kafka/orderstatus/go.mod example/kafka/orderstatus/go.sum example/kafka/orderstatus/
RUN go -C example/kafka/orderstatus mod download
COPY . .
RUN CGO_ENABLED=0 go -C example/kafka/orderstatus build -trimpath -ldflags=-s -o /out/orderstatus . && \
    CGO_ENABLED=0 go -C example/kafka/orderstatus build -trimpath -ldflags=-s -o /out/orderevents ./cmd/orderevents

FROM gcr.io/distroless/static-debian13:nonroot@sha256:aafe993665bf6200db38a02c11a9351e782a84b6a45e7f33b6695638c88e3931
COPY --from=build /out/orderstatus /orderstatus
COPY --from=build /out/orderevents /orderevents
USER 65532:65532
ENTRYPOINT ["/orderstatus"]
