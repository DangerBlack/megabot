FROM golang:1.27-alpine AS builder

RUN apk add --no-cache gcc musl-dev

WORKDIR /app
COPY go.mod go.sum ./
RUN go mod download
COPY . .
RUN CGO_ENABLED=1 GOOS=linux go build -o megabot ./src/main.go

FROM alpine:3.24
# go-sqlite3 bundles SQLite statically: no sqlite-libs needed at runtime.
# apk upgrade pulls security fixes released after the base image was built.
RUN apk --no-cache upgrade && apk --no-cache add ca-certificates tzdata ffmpeg
WORKDIR /app
COPY --from=builder /app/megabot .

RUN mkdir -p /app/data

CMD ["./megabot"]
