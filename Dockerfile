FROM golang:1.15 as builder

WORKDIR /app

COPY . .

RUN go build -o txwatch . 

FROM golang:1.15 as app

WORKDIR /app

COPY --from=builder /app/txwatch .

ENTRYPOINT [ "/app/txwatch" ]
