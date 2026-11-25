FROM docker.io/library/golang:1.27.4-trixie@sha256:430985542100b871e108ccc00e8dcc5eef08ca14ee5825e98ebf563d8ffec4ee AS build
WORKDIR /src
COPY go.mod go.sum ./
COPY example/http/polls/go.mod example/http/polls/go.sum example/http/polls/
RUN go -C example/http/polls mod download
COPY . .
RUN CGO_ENABLED=0 go -C example/http/polls build -trimpath -ldflags=-s -o /out/polls .

FROM gcr.io/distroless/static-debian13:nonroot@sha256:8ce6dd87e9823c5b48dcfb2c8604714ccb351ce59607327bcc4957448da279b5
COPY --from=build /out/polls /polls
USER 65532:65532
