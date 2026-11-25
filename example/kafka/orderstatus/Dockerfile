FROM docker.io/library/golang:1.27.4-trixie@sha256:430985542100b871e108ccc00e8dcc5eef08ca14ee5825e98ebf563d8ffec4ee AS build
WORKDIR /src
COPY go.mod go.sum ./
COPY queue/kafka/go.mod queue/kafka/go.sum queue/kafka/
COPY example/kafka/orderstatus/go.mod example/kafka/orderstatus/go.sum example/kafka/orderstatus/
RUN go -C example/kafka/orderstatus mod download
COPY . .
RUN CGO_ENABLED=0 go -C example/kafka/orderstatus build -trimpath -ldflags=-s -o /out/orderstatus . && \
    CGO_ENABLED=0 go -C example/kafka/orderstatus build -trimpath -ldflags=-s -o /out/orderevents ./cmd/orderevents

FROM gcr.io/distroless/static-debian13:nonroot@sha256:8ce6dd87e9823c5b48dcfb2c8604714ccb351ce59607327bcc4957448da279b5
COPY --from=build /out/orderstatus /orderstatus
COPY --from=build /out/orderevents /orderevents
USER 65532:65532
ENTRYPOINT ["/orderstatus"]
