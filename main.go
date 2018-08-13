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

type options struct {
	Src            string
	Dst            string
	Package        string
	Namespace      string
	Insecure       bool
	ClientCertFile string
	ClientKeyFile  string
	Version        bool
}

func main() {
	opts := options{}

	flag.StringVar(&opts.Src, "i", opts.Src, "input file, url, or '-' for stdin")
	flag.StringVar(&opts.Dst, "o", opts.Dst, "output file, or '-' for stdout")
	flag.StringVar(&opts.Namespace, "n", opts.Namespace, "override namespace")
	flag.StringVar(&opts.Package, "p", opts.Package, "package name")
	flag.BoolVar(&opts.Insecure, "yolo", opts.Insecure, "accept invalid https certificates")
	flag.StringVar(&opts.ClientCertFile, "cert", opts.ClientCertFile, "use client TLS cert file")
	flag.StringVar(&opts.ClientKeyFile, "key", opts.ClientKeyFile, "use client TLS key file")
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
		f, err := os.OpenFile(opts.Dst, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0644)
		if err != nil {
			log.Fatal(err)
		}
		defer f.Close()
		w = f
	}

	cli := httpClient(opts.Insecure, opts.ClientCertFile, opts.ClientKeyFile)

	err := codegen(w, opts, cli)
	if err != nil {
		log.Fatal(err)
	}
}

func codegen(w io.Writer, opts options, cli *http.Client) error {
	var err error
	var f io.ReadCloser
	if opts.Src == "" || opts.Src == "-" {
		f = os.Stdin
	} else if f, err = open(opts.Src, cli); err != nil {
		return err
	}
	d, err := wsdl.Unmarshal(f)
	if err != nil {
		return err
	}
	f.Close()

	enc := wsdlgo.NewEncoder(w)
	enc.SetClient(cli)
	if opts.Package != "" {
		enc.SetPackageName(wsdlgo.PackageName(opts.Package))
	}
	if opts.Namespace != "" {
		enc.SetLocalNamespace(opts.Namespace)
	}

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
func httpClient(insecure bool, clientCertPath, clientKeyPath string) *http.Client {
	tlsConfig := &tls.Config{InsecureSkipVerify: insecure}

	if clientCertPath != "" && clientKeyPath != "" {
		clientCert, err := tls.LoadX509KeyPair(clientCertPath, clientKeyPath)
		if err != nil {
			log.Fatalln("Failed to load x509 client key pair:", err)
		}
		tlsConfig.Certificates = []tls.Certificate{clientCert}
		tlsConfig.Renegotiation = tls.RenegotiateFreelyAsClient
	} else if clientCertPath == "" && clientKeyPath != "" {
		log.Fatalln("Certificate file is required when using key file")
	} else if clientCertPath != "" && clientKeyPath == "" {
		log.Fatalln("Key file is required when using certificate file")
	}

	defaultTransport := http.DefaultTransport.(*http.Transport)
	transport := &http.Transport{
		Proxy:                 defaultTransport.Proxy,
		DialContext:           defaultTransport.DialContext,
		MaxIdleConns:          defaultTransport.MaxIdleConns,
		IdleConnTimeout:       defaultTransport.IdleConnTimeout,
		ExpectContinueTimeout: defaultTransport.ExpectContinueTimeout,
		TLSHandshakeTimeout:   defaultTransport.TLSHandshakeTimeout,
		TLSClientConfig:       tlsConfig,
	}
	return &http.Client{Transport: transport}
}
