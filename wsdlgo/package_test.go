package wsdlgo

import (
	"testing"
	"github.com/fiorix/wsdl2go/wsdl"
)

func TestBindingPackageName_String(t *testing.T) {
	tests := []struct {
		expected string
		binding  wsdl.Binding
	}{
		{"foo", wsdl.Binding{Name: "foo"}},
		{"dataendpointsoap11binding", wsdl.Binding{Name: "DataEndpointSoap11Binding"}},
		{"somedottedbindingname", wsdl.Binding{Name: "Some.Dotted.Binding.Name"}},
	}

	for _, test := range tests {
		namer := BindingPackageName(test.binding)
		name := namer.String()
		if test.expected != name {
			t.Errorf("expected `%s`, actual `%s`", test.expected, name)
		}
	}
}
