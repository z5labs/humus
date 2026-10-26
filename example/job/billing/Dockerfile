FROM docker.io/library/golang:1.27.3-trixie@sha256:fbb7ae40b62ff8afe31662df570d39b4f5052ac32b90dae8e60d7657a20a74f4 AS build
WORKDIR /src
COPY go.mod go.sum ./
COPY example/job/billing/go.mod example/job/billing/go.sum example/job/billing/
RUN go -C example/job/billing mod download
COPY . .
RUN CGO_ENABLED=0 go -C example/job/billing build -trimpath -ldflags=-s -o /out/billing .

FROM gcr.io/distroless/static-debian13:nonroot@sha256:18f4cc26c9ffa7f20a359298be7deadac5c607c8e03a02b8119f1a5fc2f82c99
COPY --from=build /out/billing /billing
USER 65532:65532
ENTRYPOINT ["/billing"]
