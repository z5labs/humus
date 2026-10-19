FROM docker.io/library/golang:1.27.2-trixie@sha256:47667934874913a449b96faa7ef4777fa285d9ec29126ee80ed61c45b7cccf0e AS build
WORKDIR /src
COPY go.mod go.sum ./
COPY example/billing/go.mod example/billing/go.sum example/billing/
RUN go -C example/billing mod download
COPY . .
RUN CGO_ENABLED=0 go -C example/billing build -trimpath -ldflags=-s -o /out/billing .

FROM gcr.io/distroless/static-debian13:nonroot@sha256:e2e927ec666bae08560abb3c55d0659eceabb657f56b6782ab500a9fc7f555e3
COPY --from=build /out/billing /billing
USER 65532:65532
ENTRYPOINT ["/billing"]
