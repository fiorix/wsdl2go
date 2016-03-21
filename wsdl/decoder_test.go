package wsdl

import (
	"encoding/xml"
	"os"
	"path/filepath"
	"reflect"
	"testing"
)

func TestUnmarshal(t *testing.T) {
	cases := []struct {
		F string
		E error
	}{
		{
			F: "golden1.wsdl",
			E: nil,
		}, {
			F: "golden2.wsdl",
			E: xml.UnmarshalError("..."),
		},
	}
	for i, tc := range cases {
		f, err := os.Open(filepath.Join("testdata", tc.F))
		if err != nil {
			t.Errorf("test %d (%q) failed: %v", i, tc.F, err)
		}
		defer f.Close()
		_, err = Unmarshal(f)
		if tc.E == nil {
			if err != nil {
				t.Errorf("test %d (%q) failed: want %v, have %v", i, tc.F, tc.E, err)
			}
			continue
		}
		want := reflect.ValueOf(tc.E).Type().Name()
		have := reflect.ValueOf(err).Type().Name()
		if want != have {
			t.Errorf("test %d (%q) failed: want %q, have %q", i, tc.F, want, have)
		}
	}
}
