FROM golang:1.27-alpine AS builder

RUN apk add --no-cache gcc musl-dev

WORKDIR /app
COPY go.mod go.sum ./
RUN go mod download
COPY . .
RUN CGO_ENABLED=1 GOOS=linux go build -o megabot ./src/main.go

FROM alpine:3.24
RUN apk --no-cache add ca-certificates sqlite-libs tzdata
WORKDIR /app
COPY --from=builder /app/megabot .

RUN mkdir -p /app/data

CMD ["./megabot"]
