FROM golang:1.26.4-alpine3.22 AS build
WORKDIR /src
COPY . .
RUN CGO_ENABLED=0 go build -o /out/api ./cmd/api

FROM alpine:3.20.3
COPY --from=build /out/api /app/api
ENTRYPOINT ["/app/api"]
