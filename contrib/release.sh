#!/bin/bash

VERSION=${VERSION:-`git describe --tags`}

for OS in linux freebsd windows darwin
do
	GOOS=$OS GOARCH=amd64 go build -ldflags "-w -X main.version=${VERSION}"
	TARBALL=wsdl2go-$VERSION-$OS-amd64.tar.gz
	if [ "$OS" = "windows" ]; then
		tar czf $TARBALL wsdl2go.exe
	else
		tar czf $TARBALL wsdl2go
	fi
	rm -f wsdl2go wsdl2go.exe
done
