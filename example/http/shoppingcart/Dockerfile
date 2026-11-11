FROM docker.io/library/golang:1.27.4-trixie@sha256:b54b2639de8c1bfe538ec5ffc45f29b1932235ac5f6d63c059853646677112d9 AS build
WORKDIR /src
COPY go.mod go.sum ./
COPY example/http/shoppingcart/go.mod example/http/shoppingcart/go.sum example/http/shoppingcart/
RUN go -C example/http/shoppingcart mod download
COPY . .
RUN CGO_ENABLED=0 go -C example/http/shoppingcart build -trimpath -ldflags=-s -o /out/cart . && \
    CGO_ENABLED=0 go -C example/http/shoppingcart build -trimpath -ldflags=-s -o /out/catalog ./catalog && \
    CGO_ENABLED=0 go -C example/http/shoppingcart build -trimpath -ldflags=-s -o /out/migrate ./cmd/migrate

FROM gcr.io/distroless/static-debian13:nonroot@sha256:25ac8f0c3b2334a9eb05452266bd4697864a8c8ef0ef71e5e6480d7f92868329
COPY --from=build /out/cart /cart
COPY --from=build /out/catalog /catalog
COPY --from=build /out/migrate /migrate
USER 65532:65532
