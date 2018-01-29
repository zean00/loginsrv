FROM fabiorphp/golang-glide:latest as builder

RUN mkdir -p /go/src/github.com/tarent && \
	cd /go/src/github.com/tarent && \
	git clone https://github.com/zean00/loginsrv.git && \
	cd loginsrv && \
	glide --debug install
RUN	cd /go/src/github.com/tarent/loginsrv && \
	env CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build -a -installsuffix cgo -ldflags '-w'

FROM alpine
RUN apk --update --no-cache add ca-certificates \
    && addgroup -S loginsrv && adduser -S -g loginsrv loginsrv
USER loginsrv
ENV LOGINSRV_HOST=0.0.0.0 LOGINSRV_PORT=8080
COPY --from=builder /go/src/github.com/tarent/loginsrv/loginsrv / 
ENTRYPOINT ["/loginsrv"]
EXPOSE 8080