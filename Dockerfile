FROM golang:1.23-alpine AS build

WORKDIR /app
COPY go.mod go.sum ./
RUN GOPROXY=https://goproxy.cn,direct go mod download
COPY main.go .
RUN CGO_ENABLED=0 go build -o typonote-server .

FROM alpine:3.21
WORKDIR /app
COPY --from=build /app/typonote-server .
VOLUME /app/uploads
EXPOSE 8080
CMD ["./typonote-server"]
