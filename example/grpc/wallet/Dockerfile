FROM docker.io/library/golang:1.27.3-trixie@sha256:fbb7ae40b62ff8afe31662df570d39b4f5052ac32b90dae8e60d7657a20a74f4 AS build
WORKDIR /src
COPY go.mod go.sum ./
COPY grpc/go.mod grpc/go.sum grpc/
COPY example/grpc/wallet/go.mod example/grpc/wallet/go.sum example/grpc/wallet/
RUN go -C example/grpc/wallet mod download
COPY . .
RUN CGO_ENABLED=0 go -C example/grpc/wallet build -trimpath -ldflags=-s -o /out/wallet . && \
    CGO_ENABLED=0 go -C example/grpc/wallet build -trimpath -ldflags=-s -o /out/ledger ./ledger

FROM gcr.io/distroless/static-debian13:nonroot@sha256:d62067879de890a1def9da0e7766b7d20810a5e5555cef8506b752437d7a3e88
COPY --from=build /out/wallet /wallet
COPY --from=build /out/ledger /ledger
USER 65532:65532
