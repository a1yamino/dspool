FROM golang:1.27-alpine AS build

WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
RUN CGO_ENABLED=0 go build -trimpath -ldflags "-s -w" -o /out/dspool ./cmd/dspool

FROM gcr.io/distroless/static-debian12:nonroot

COPY --from=build /out/dspool /usr/local/bin/dspool
WORKDIR /data
EXPOSE 8787

# Mount a config file at /config/config.yaml and a keys file next to it.
# In containers remember to set listen: 0.0.0.0:8787, client_keys and admin_token.
ENTRYPOINT ["/usr/local/bin/dspool", "-config", "/config/config.yaml"]
