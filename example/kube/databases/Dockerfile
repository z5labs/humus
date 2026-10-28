FROM docker.io/library/golang:1.27.3-trixie@sha256:c124b9ad3452f46061b5054607d26ea51730dae5372c5e469920063ba2bc6dcc AS build
WORKDIR /src
COPY go.mod go.sum ./
COPY kube/go.mod kube/go.sum kube/
COPY example/kube/databases/go.mod example/kube/databases/go.sum example/kube/databases/
RUN go -C example/kube/databases mod download
COPY . .
RUN CGO_ENABLED=0 go -C example/kube/databases build -trimpath -ldflags=-s -o /out/databases .

FROM gcr.io/distroless/static-debian13:nonroot@sha256:18f4cc26c9ffa7f20a359298be7deadac5c607c8e03a02b8119f1a5fc2f82c99
COPY --from=build /out/databases /databases
USER 65532:65532
ENTRYPOINT ["/databases"]
