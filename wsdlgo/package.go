package wsdlgo

import (
	"strings"

	"github.com/grid-x/wsdl2go/wsdl"
)

const (
	fallbackPackageName = "internal"
)

// BindingPackageName formats package name from wsdl binding
type BindingPackageName wsdl.Binding

func (p BindingPackageName) String() string {
	packageName := strings.Replace(strings.ToLower(p.Name), ".", "", -1)
	if packageName == "" {
		packageName = fallbackPackageName
	}
	return packageName
}

// PackageName is just a string with interface
type PackageName string

func (p PackageName) String() string {
	return string(p)
}
