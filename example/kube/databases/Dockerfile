FROM docker.io/library/golang:1.27.3-trixie@sha256:af1bfc2614ebb1dbd31af514b78740fd678850e76498762b425ecee4c5c6ea80 AS build
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
