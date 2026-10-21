FROM docker.io/library/golang:1.27.2-trixie@sha256:47667934874913a449b96faa7ef4777fa285d9ec29126ee80ed61c45b7cccf0e AS build
WORKDIR /src
COPY go.mod go.sum ./
COPY grpc/go.mod grpc/go.sum grpc/
COPY example/wallet/go.mod example/wallet/go.sum example/wallet/
RUN go -C example/wallet mod download
COPY . .
RUN CGO_ENABLED=0 go -C example/wallet build -trimpath -ldflags=-s -o /out/wallet . && \
    CGO_ENABLED=0 go -C example/wallet build -trimpath -ldflags=-s -o /out/ledger ./ledger

FROM gcr.io/distroless/static-debian13:nonroot@sha256:e2e927ec666bae08560abb3c55d0659eceabb657f56b6782ab500a9fc7f555e3
COPY --from=build /out/wallet /wallet
COPY --from=build /out/ledger /ledger
USER 65532:65532
