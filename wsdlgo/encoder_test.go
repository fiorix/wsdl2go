package wsdlgo

import (
	"bytes"
	"fmt"
	"io"
	"io/ioutil"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/fiorix/wsdl2go/wsdl"
	"github.com/stretchr/testify/assert"
)

func LoadDefinition(t *testing.T, filename string, want error) *wsdl.Definitions {
	f, err := os.Open(filepath.Join("testdata", filename))
	if err != nil {
		t.Errorf("missing wsdl file %q: %v", filename, err)
	}
	defer f.Close()

	// replace CURRENT_DIR in wsdl with the current working directory
	// - for testing file: schema
	ba, _ := ioutil.ReadAll(f)
	pwd, _ := os.Getwd()
	s := strings.Replace(string(ba), "CURRENT_DIR", pwd, 1)

	d, err := wsdl.Unmarshal(strings.NewReader(s))
	if err != want {
		t.Errorf("%q failed: want %v, have %v", filename, want, err)
	}
	return d
}

var EncoderCases = []struct {
	F string
	G string
	E error
}{
	{F: "broken.wsdl", E: io.EOF},
	{F: "w3cexample1.wsdl", G: "w3cexample1.golden", E: nil},
	{F: "w3cexample2.wsdl", G: "w3cexample2.golden", E: nil},
	{F: "w3example1.wsdl", G: "w3example1.golden", E: nil},
	{F: "w3example2.wsdl", G: "w3example2.golden", E: nil},
	{F: "soap12wcf.wsdl", G: "soap12wcf.golden", E: nil},
	{F: "memcache.wsdl", G: "memcache.golden", E: nil},
	{F: "importer.wsdl", G: "memcache.golden", E: nil},
	{F: "data.wsdl", G: "data.golden", E: nil},
	{F: "data_withkeyword.wsdl", G: "data_withkeyword.golden", E: nil},
	{F: "localimport.wsdl", G: "localimport.golden", E: nil},
	{F: "localimport-url.wsdl", G: "localimport.golden", E: nil},
	{F: "localimport_choice.wsdl", G: "localimport_choice.golden", E: nil},
	{F: "arrayexample.wsdl", G: "arrayexample.golden", E: nil},
	{F: "radioreference.wsdl", G: "radioreference.golden", E: nil},
	{F: "scannerservice.wsdl", G: "scannerservice.golden", E: nil},
}

func NewTestServer(t *testing.T) *httptest.Server {
	mux := http.NewServeMux()
	fs := http.FileServer(http.Dir("testdata"))
	mux.Handle("/", fs)
	s := httptest.NewUnstartedServer(mux)
	l, err := net.Listen("tcp4", ":9999")
	if err != nil {
		t.Fatalf("cannot listen on 9999: %v", err)
	}
	s.Listener = l
	s.Start()
	return s
}

func TestEncoder(t *testing.T) {
	s := NewTestServer(t)
	defer s.Close()
	for i, tc := range EncoderCases {
		d := LoadDefinition(t, tc.F, tc.E)
		var err error
		var want []byte
		var have bytes.Buffer
		err = NewEncoder(&have).Encode(d)
		if err != nil {
			t.Errorf("test %d, encoding %q: %v", i, tc.F, err)
		}
		if tc.G == "" {
			continue
		}
		want, err = ioutil.ReadFile(filepath.Join("testdata", tc.G))
		if err != nil {
			t.Errorf("test %d: missing golden file %q: %v", i, tc.G, err)
		}
		if !bytes.Equal(have.Bytes(), want) {
			assert.Equal(t, string(want), string(have.Bytes()))
		}
	}
}

func Diff(prefix, ext string, a, b []byte) error {
	diff, err := exec.LookPath("diff")
	if err != nil {
		return fmt.Errorf("diff: %v", err)
	}
	cases := []struct {
		File string
		Data []byte
	}{
		{File: prefix + "-a." + ext, Data: a},
		{File: prefix + "-b." + ext, Data: b},
	}
	for _, c := range cases {
		defer os.Remove(c.File)
		if err = ioutil.WriteFile(c.File, c.Data, 0600); err != nil {
			return err
		}
	}
	var stdout, stderr bytes.Buffer
	cmd := exec.Cmd{
		Path:   diff,
		Args:   []string{"-u", cases[0].File, cases[1].File},
		Stdout: &stdout,
		Stderr: &stderr,
	}
	err = cmd.Run()
	if err != nil {
		return fmt.Errorf("%v: %s", err, stdout.String())
	}
	return nil
}
