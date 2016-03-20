#!/bin/bash

VERSION=${VERSION:-`git describe --tags`}

for OS in linux freebsd windows darwin
do
	GOOS=$OS GOARCH=amd64 go build -ldflags "-w -X main.version=${VERSION}"
	tar czf wsdl2go-$VERSION-$OS-amd64.tar.gz wsdl2go*
done
