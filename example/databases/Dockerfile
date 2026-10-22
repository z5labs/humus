FROM docker.io/library/golang:1.27.3-trixie@sha256:fbb7ae40b62ff8afe31662df570d39b4f5052ac32b90dae8e60d7657a20a74f4 AS build
WORKDIR /src
COPY go.mod go.sum ./
COPY kube/go.mod kube/go.sum kube/
COPY example/databases/go.mod example/databases/go.sum example/databases/
RUN go -C example/databases mod download
COPY . .
RUN CGO_ENABLED=0 go -C example/databases build -trimpath -ldflags=-s -o /out/databases .

FROM gcr.io/distroless/static-debian13:nonroot@sha256:d62067879de890a1def9da0e7766b7d20810a5e5555cef8506b752437d7a3e88
COPY --from=build /out/databases /databases
USER 65532:65532
ENTRYPOINT ["/databases"]
