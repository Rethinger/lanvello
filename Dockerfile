FROM golang:1.24-bookworm AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
RUN CGO_ENABLED=0 go build -trimpath -o /out/lanvello ./cmd/lanvello

FROM debian:bookworm-slim
RUN apt-get update && apt-get install -y --no-install-recommends ca-certificates tor && rm -rf /var/lib/apt/lists/*
COPY --from=build /out/lanvello /usr/local/bin/lanvello
ENV LANVELLO_DATA_DIR=/state
EXPOSE 11434
ENTRYPOINT ["lanvello", "serve", "--listen", "0.0.0.0:11434"]
