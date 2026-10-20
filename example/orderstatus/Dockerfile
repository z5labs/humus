FROM docker.io/library/golang:1.27.2-trixie@sha256:47667934874913a449b96faa7ef4777fa285d9ec29126ee80ed61c45b7cccf0e AS build
WORKDIR /src
COPY go.mod go.sum ./
COPY queue/kafka/go.mod queue/kafka/go.sum queue/kafka/
COPY example/orderstatus/go.mod example/orderstatus/go.sum example/orderstatus/
RUN go -C example/orderstatus mod download
COPY . .
RUN CGO_ENABLED=0 go -C example/orderstatus build -trimpath -ldflags=-s -o /out/orderstatus . && \
    CGO_ENABLED=0 go -C example/orderstatus build -trimpath -ldflags=-s -o /out/orderevents ./cmd/orderevents

FROM gcr.io/distroless/static-debian13:nonroot@sha256:e2e927ec666bae08560abb3c55d0659eceabb657f56b6782ab500a9fc7f555e3
COPY --from=build /out/orderstatus /orderstatus
COPY --from=build /out/orderevents /orderevents
USER 65532:65532
ENTRYPOINT ["/orderstatus"]
