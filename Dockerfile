FROM golang:latest
RUN curl https://raw.githubusercontent.com/golang/dep/master/install.sh | sh
WORKDIR /go/src/github.com/elafarge/k8s-dockerhub-sync
COPY . .
RUN dep ensure
RUN CGO_ENABLED=0 GOOS=linux go build -a -installsuffix cgo -o hook .

FROM scratch
COPY --from=0 /go/src/github.com/elafarge/k8s-dockerhub-sync/hook /hook
ENTRYPOINT ["/hook"]
