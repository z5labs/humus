FROM docker.io/library/golang:1.27.4-trixie@sha256:430985542100b871e108ccc00e8dcc5eef08ca14ee5825e98ebf563d8ffec4ee AS build
WORKDIR /src
COPY go.mod go.sum ./
COPY http3/go.mod http3/go.sum http3/
COPY example/http/shoppingcart/go.mod example/http/shoppingcart/go.sum example/http/shoppingcart/
RUN go -C example/http/shoppingcart mod download
COPY . .
RUN CGO_ENABLED=0 go -C example/http/shoppingcart build -trimpath -ldflags=-s -o /out/cart . && \
    CGO_ENABLED=0 go -C example/http/shoppingcart build -trimpath -ldflags=-s -o /out/catalog ./catalog && \
    CGO_ENABLED=0 go -C example/http/shoppingcart build -trimpath -ldflags=-s -o /out/migrate ./cmd/migrate

FROM gcr.io/distroless/static-debian13:nonroot@sha256:8ce6dd87e9823c5b48dcfb2c8604714ccb351ce59607327bcc4957448da279b5
COPY --from=build /out/cart /cart
COPY --from=build /out/catalog /catalog
COPY --from=build /out/migrate /migrate
USER 65532:65532
