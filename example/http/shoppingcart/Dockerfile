FROM docker.io/library/golang:1.27.3-trixie@sha256:af1bfc2614ebb1dbd31af514b78740fd678850e76498762b425ecee4c5c6ea80 AS build
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
