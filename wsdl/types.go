package wsdl

// TODO: Add all types from the spec.

import "encoding/xml"

// Definitions is the root element of a WSDL document.
type Definitions struct {
	XMLName         xml.Name   `xml:"definitions"`
	Name            string     `xml:"name,attr"`
	TargetNamespace string     `xml:"targetNamespace,attr"`
	SOAPEnv         string     `xml:"SOAP-ENV,attr"`
	SOAPEnc         string     `xml:"SOAP-ENC,attr"`
	Service         Service    `xml:"service"`
	Imports         []*Import  `xml:"import"`
	Schema          Schema     `xml:"types>schema"`
	Messages        []*Message `xml:"message"`
	PortType        PortType   `xml:"portType"` // TODO: PortType slice?
	Binding         Binding    `xml:"binding"`
}

// Service defines a WSDL service and with a location, like an HTTP server.
type Service struct {
	Doc   string  `xml:"documentation"`
	Ports []*Port `xml:"port"`
}

// Port for WSDL service.
type Port struct {
	XMLName xml.Name `xml:"port"`
	Name    string   `xml:"name,attr"`
	Binding string   `xml:"binding,attr"`
	Address Address  `xml:"address"`
}

// Address of WSDL service.
type Address struct {
	XMLName  xml.Name `xml:"address"`
	Location string   `xml:"location,attr"`
}

// Schema of WSDL document.
type Schema struct {
	XMLName      xml.Name        `xml:"schema"`
	Imports      []*ImportSchema `xml:"import"`
	SimpleTypes  []*SimpleType   `xml:"simpleType"`
	ComplexTypes []*ComplexType  `xml:"complexType"`
	Elements     []*Element      `xml:"element"`
}

// SimpleType describes a simple type, such as string.
type SimpleType struct {
	XMLName     xml.Name     `xml:"simpleType"`
	Name        string       `xml:"name,attr"`
	Union       *Union       `xml:"union"`
	Restriction *Restriction `xml:"restriction"`
}

// Union is a mix of multiple types in a union.
type Union struct {
	XMLName     xml.Name `xml:"union"`
	MemberTypes string   `xml:"memberTypes,attr"`
}

// Restriction describes the WSDL type of the simple type and
// optionally its allowed values.
type Restriction struct {
	XMLName xml.Name `xml:"restriction"`
	Base    string   `xml:"base,attr"`
	Enum    []*Enum  `xml:"enumeration"`
}

// Enum describes one possible value for a Restriction.
type Enum struct {
	XMLName xml.Name `xml:"enumeration"`
	Value   string   `xml:"value,attr"`
}

// ComplexType describes a complex type, such as a struct.
type ComplexType struct {
	XMLName        xml.Name        `xml:"complexType"`
	Name           string          `xml:"name,attr"`
	Abstract       bool            `xml:"abstract,attr"`
	Doc            string          `xml:"annotation>documentation"`
	AllElements    []*Element      `xml:"all>element"`
	ComplexContent *ComplexContent `xml:"complexContent"`
	SimpleContent  *SimpleContent  `xml:"simpleContent"`
	Sequence       *Sequence       `xml:"sequence"`
	Attributes     []*Attribute    `xml:"attribute"`
}

// SimpleContent describes simple content within a complex type.
type SimpleContent struct {
	XMLName   xml.Name   `xml:"simpleContent"`
	Extension *Extension `xml:"extension"`
}

// ComplexContent describes complex content within a complex type. Usually
// for extending the complex type with fields from the complex content.
type ComplexContent struct {
	XMLName   xml.Name   `xml:"complexContent"`
	Extension *Extension `xml:"extension"`
}

// Extension describes a complex content extension.
type Extension struct {
	XMLName    xml.Name     `xml:"extension"`
	Base       string       `xml:"base,attr"`
	Sequence   *Sequence    `xml:"sequence"`
	Attributes []*Attribute `xml:"attribute"`
}

// Sequence describes a list of elements (parameters) of a type.
type Sequence struct {
	XMLName      xml.Name       `xml:"sequence"`
	ComplexTypes []*ComplexType `xml:"complexType"`
	Elements     []*Element     `xml:"element"`
	Any          []*AnyElement  `xml:"any"`
}

// Attribute describes an attribute of a complext type.
type Attribute struct {
	XMLName xml.Name `xml:"attribute"`
	Name    string   `xml:"name,attr"`
	Type    string   `xml:"type,attr"`
}

// Element describes an element of a given type.
type Element struct {
	XMLName     xml.Name     `xml:"element"`
	Name        string       `xml:"name,attr"`
	Ref         string       `xml:"ref,attr"`
	Type        string       `xml:"type,attr"`
	Min         int          `xml:"minOccurs,attr"`
	Max         string       `xml:"maxOccurs,attr"` // can be # or unbounded
	Nillable    bool         `xml:"nillable,attr"`
	ComplexType *ComplexType `xml:"complexType"`
}

// AnyElement describes an element of an undefined type.
type AnyElement struct {
	XMLName xml.Name `xml:"any"`
	Min     int      `xml:"minOccurs,attr"`
	Max     string   `xml:"maxOccurs,attr"` // can be # or unbounded
}

// Import points to another WSDL to be imported at root level.
type Import struct {
	XMLName   xml.Name `xml:"import"`
	Namespace string   `xml:"namespace,attr"`
	Location  string   `xml:"location,attr"`
}

// ImportSchema points to another WSDL to be imported at schema level.
type ImportSchema struct {
	XMLName   xml.Name `xml:"import"`
	Namespace string   `xml:"namespace,attr"`
	Location  string   `xml:"schemaLocation,attr"`
}

// Message describes the data being communicated, such as functions
// and their parameters.
type Message struct {
	XMLName xml.Name `xml:"message"`
	Name    string   `xml:"name,attr"`
	Parts   []*Part  `xml:"part"`
}

// Part describes what Type or Element to use from the PortType.
type Part struct {
	XMLName xml.Name `xml:"part"`
	Name    string   `xml:"name,attr"`
	Type    string   `xml:"type,attr,omitempty"`
	Element string   `xml:"element,attr,omitempty"` // TODO: not sure omitempty
}

// PortType describes a set of operations.
type PortType struct {
	XMLName    xml.Name     `xml:"portType"`
	Name       string       `xml:"name,attr"`
	Operations []*Operation `xml:"operation"`
}

// Operation describes an operation.
type Operation struct {
	XMLName xml.Name `xml:"operation"`
	Name    string   `xml:"name,attr"`
	Doc     string   `xml:"documentation"`
	Input   *IO      `xml:"input"`
	Output  *IO      `xml:"output"`
}

// IO describes which message is linked to an operation, for input
// or output parameters.
type IO struct {
	XMLName xml.Name
	Message string `xml:"message,attr"`
}

// Binding describes SOAP to WSDL binding.
type Binding struct {
	XMLName    xml.Name            `xml:"binding"`
	Name       string              `xml:"name,attr"`
	Type       string              `xml:"type,attr"`
	Operations []*BindingOperation `xml:"operation"`
}

// BindingOperation describes the requirement for binding SOAP to WSDL
// operations.
type BindingOperation struct {
	XMLName xml.Name   `xml:"operation"`
	Name    string     `xml:"name,attr"`
	Input   *BindingIO `xml:"input>body"`
	Output  *BindingIO `xml:"output>body"`
}

// BindingIO describes the IO binding of SOAP operations. See IO for details.
type BindingIO struct {
	Parts string `xml:"parts,attr"`
	Use   string `xml:"use,attr"`
}
