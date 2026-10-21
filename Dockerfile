FROM golang:1.24-alpine AS build
WORKDIR /src
COPY go.mod ./
COPY . .
ARG VERSION=dev
RUN CGO_ENABLED=0 go build -trimpath -ldflags "-s -w -X main.version=${VERSION}" -o /tailcove-agent ./cmd/tailcove-agent

FROM gcr.io/distroless/static-debian12:nonroot
COPY --from=build /tailcove-agent /tailcove-agent
ENTRYPOINT ["/tailcove-agent"]
CMD ["run"]
