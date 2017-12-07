package main

import (
	"crypto/tls"
	"flag"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/url"
	"os"

	"github.com/fiorix/wsdl2go/wsdl"
	"github.com/fiorix/wsdl2go/wsdlgo"
)

var version = "tip"

func main() {
	opts := struct {
		Src      string
		Dst      string
		Insecure bool
		Version  bool
	}{}
	flag.StringVar(&opts.Src, "i", opts.Src, "input file, url, or '-' for stdin")
	flag.StringVar(&opts.Dst, "o", opts.Dst, "output file, or '-' for stdout")
	flag.BoolVar(&opts.Insecure, "yolo", opts.Insecure, "accept invalid https certificates")
	flag.BoolVar(&opts.Version, "version", opts.Version, "show version and exit")
	flag.Parse()
	if opts.Version {
		fmt.Printf("wsdl2go %s\n", version)
		return
	}
	var w io.Writer
	switch opts.Dst {
	case "", "-":
		w = os.Stdout
	default:
		f, err := os.OpenFile(opts.Dst, os.O_CREATE|os.O_WRONLY, 0644)
		if err != nil {
			log.Fatal(err)
		}
		defer f.Close()
		w = f
	}

	cli := httpClient(opts.Insecure)

	err := decode(w, opts.Src, cli)
	if err != nil {
		log.Fatal(err)
	}
}

func decode(w io.Writer, src string, cli *http.Client) error {
	var err error
	var f io.ReadCloser
	if src == "" || src == "-" {
		f = os.Stdin
	} else if f, err = open(src, cli); err != nil {
		return err
	}
	d, err := wsdl.Unmarshal(f)
	if err != nil {
		return err
	}
	f.Close()
	enc := wsdlgo.NewEncoder(w)
	enc.SetClient(cli)
	return enc.Encode(d)
}

func open(name string, cli *http.Client) (io.ReadCloser, error) {
	u, err := url.Parse(name)
	if err != nil || u.Scheme == "" {
		return os.Open(name)
	}
	resp, err := cli.Get(name)
	if err != nil {
		return nil, err
	}
	return resp.Body, err
}

// httpClient returns http client with default options
func httpClient(insecure bool) *http.Client {
	transport := *(http.DefaultTransport.(*http.Transport))
	transport.TLSClientConfig = &tls.Config{InsecureSkipVerify: insecure}

	return &http.Client{Transport: &transport}
}
