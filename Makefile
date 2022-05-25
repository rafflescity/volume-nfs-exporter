build:
	env GOOS=linux GOARCH=amd64 CGO_ENABLED=0 \
		go build -v -ldflags="-extldflags=-static -s -w" \
		-o volume-nfs-provisioner
	chmod -v +x volume-nfs-provisioner
	chmod -vR +x cmd

provisioner:
	docker build . -f Dockerfile.provisioner -t daocloud.io/piraeus/volume-nfs-provisioner

exporter:
	docker build . -f Dockerfile.exporter.alpine -t daocloud.io/piraeus/volume-nfs-exporter:alpine
	docker build . -f Dockerfile.exporter.ganesha -t daocloud.io/piraeus/volume-nfs-exporter:ganesha
	docker build . -f Dockerfile.exporter.busybox -t daocloud.io/piraeus/volume-nfs-exporter:busybox

upload:
	docker push daocloud.io/piraeus/volume-nfs-provisioner
	docker push daocloud.io/piraeus/volume-nfs-exporter:busybox
	docker push daocloud.io/piraeus/volume-nfs-exporter:ganesha
	docker push daocloud.io/piraeus/volume-nfs-exporter:alpine

all: clean build provisioner exporter upload

clean:
	rm -vf volume-nfs-provisioner