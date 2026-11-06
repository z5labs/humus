FROM docker.io/library/golang:1.27.3-trixie@sha256:af1bfc2614ebb1dbd31af514b78740fd678850e76498762b425ecee4c5c6ea80 AS build
WORKDIR /src
COPY go.mod go.sum ./
COPY example/job/billing/go.mod example/job/billing/go.sum example/job/billing/
RUN go -C example/job/billing mod download
COPY . .
RUN CGO_ENABLED=0 go -C example/job/billing build -trimpath -ldflags=-s -o /out/billing .

FROM gcr.io/distroless/static-debian13:nonroot@sha256:25ac8f0c3b2334a9eb05452266bd4697864a8c8ef0ef71e5e6480d7f92868329
COPY --from=build /out/billing /billing
USER 65532:65532
ENTRYPOINT ["/billing"]
