FROM docker.io/library/golang:1.27.2-trixie@sha256:47667934874913a449b96faa7ef4777fa285d9ec29126ee80ed61c45b7cccf0e AS build
WORKDIR /src
COPY go.mod go.sum ./
COPY example/shoppingcart/go.mod example/shoppingcart/go.sum example/shoppingcart/
RUN go -C example/shoppingcart mod download
COPY . .
RUN CGO_ENABLED=0 go -C example/shoppingcart build -trimpath -ldflags=-s -o /out/cart . && \
    CGO_ENABLED=0 go -C example/shoppingcart build -trimpath -ldflags=-s -o /out/catalog ./catalog && \
    CGO_ENABLED=0 go -C example/shoppingcart build -trimpath -ldflags=-s -o /out/migrate ./cmd/migrate

FROM gcr.io/distroless/static-debian13:nonroot@sha256:e2e927ec666bae08560abb3c55d0659eceabb657f56b6782ab500a9fc7f555e3
COPY --from=build /out/cart /cart
COPY --from=build /out/catalog /catalog
COPY --from=build /out/migrate /migrate
USER 65532:65532
