FROM docker.io/library/golang:1.27.3-trixie@sha256:d9fa6e7ba43678ee1e5d1617007db9a134612032a74e2d86b44a9f582f1564cc AS build
WORKDIR /src
COPY go.mod go.sum ./
COPY kube/go.mod kube/go.sum kube/
COPY example/kube/databases/go.mod example/kube/databases/go.sum example/kube/databases/
RUN go -C example/kube/databases mod download
COPY . .
RUN CGO_ENABLED=0 go -C example/kube/databases build -trimpath -ldflags=-s -o /out/databases .

FROM gcr.io/distroless/static-debian13:nonroot@sha256:25ac8f0c3b2334a9eb05452266bd4697864a8c8ef0ef71e5e6480d7f92868329
COPY --from=build /out/databases /databases
USER 65532:65532
ENTRYPOINT ["/databases"]
