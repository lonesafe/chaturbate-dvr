FROM node:20-alpine AS css-builder
WORKDIR /workspace
COPY package.json package-lock.json ./
RUN npm ci
COPY tailwind.config.js ./
COPY router/view/templates/ router/view/templates/
RUN npm run build:css

FROM golang:1.23-alpine AS builder
WORKDIR /workspace
COPY ./ ./
COPY --from=css-builder /workspace/router/view/templates/styles/app.css router/view/templates/styles/app.css
RUN go build -o chaturbate-dvr .

FROM alpine:3.20 AS runnable
RUN apk add --no-cache ca-certificates ffmpeg tzdata
WORKDIR /usr/src/app
COPY --from=builder /workspace/chaturbate-dvr /chaturbate-dvr
ENTRYPOINT ["/chaturbate-dvr"]
