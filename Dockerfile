FROM golang:1.26-alpine AS build
ARG VERSION=dev
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
RUN CGO_ENABLED=0 go build -ldflags "-X main.version=${VERSION}" -o /sre-agent .

FROM alpine:3.22
RUN adduser -D -H agent
USER agent
COPY --from=build /sre-agent /usr/local/bin/sre-agent
EXPOSE 8080
ENTRYPOINT ["sre-agent"]
CMD ["serve"]
