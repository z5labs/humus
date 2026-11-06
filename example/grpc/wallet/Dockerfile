FROM docker.io/library/golang:1.27.3-trixie@sha256:af1bfc2614ebb1dbd31af514b78740fd678850e76498762b425ecee4c5c6ea80 AS build
WORKDIR /src
COPY go.mod go.sum ./
COPY grpc/go.mod grpc/go.sum grpc/
COPY example/grpc/wallet/go.mod example/grpc/wallet/go.sum example/grpc/wallet/
RUN go -C example/grpc/wallet mod download
COPY . .
RUN CGO_ENABLED=0 go -C example/grpc/wallet build -trimpath -ldflags=-s -o /out/wallet . && \
    CGO_ENABLED=0 go -C example/grpc/wallet build -trimpath -ldflags=-s -o /out/ledger ./ledger

FROM gcr.io/distroless/static-debian13:nonroot@sha256:25ac8f0c3b2334a9eb05452266bd4697864a8c8ef0ef71e5e6480d7f92868329
COPY --from=build /out/wallet /wallet
COPY --from=build /out/ledger /ledger
USER 65532:65532
