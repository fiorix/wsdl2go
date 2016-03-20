// Package wsdl provides Web Services Description Language (WSDL) decoder.
//
// http://www.w3schools.com/xml/xml_wsdl.asp
package wsdl

import (
	"encoding/xml"
	"io"
)

// Unmarshal unmarshals WSDL documents starting from the <definitions> tag.
//
// The Definitions object it returns is an unmarshalled version of the
// WSDL XML that can be introspected to generate the Web Services API.
func Unmarshal(r io.Reader) (*Definitions, error) {
	var d Definitions
	err := xml.NewDecoder(r).Decode(&d)
	if err != nil {
		return nil, err
	}
	return &d, nil
}
