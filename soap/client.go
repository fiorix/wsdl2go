// Package soap provides a SOAP HTTP client.
package soap

import (
	"bytes"
	"encoding/xml"
	"fmt"
	"io"
	"io/ioutil"
	"net/http"
	"reflect"
)

// XSINamespace is a constant describing the standard namespace for XML schema
const XSINamespace = "http://www.w3.org/2001/XMLSchema-instance"

// A RoundTripper executes a request passing the given req as the SOAP
// envelope body. The HTTP response is then de-serialized onto the resp
// object. Returns error in case an error occurs serializing req, making
// the HTTP request, or de-serializing the response.
type RoundTripper interface {
	RoundTrip(req, resp Message) error
	RoundTripSoap12(action string, req, resp Message) error
}

// Message is an opaque type used by the RoundTripper to carry XML
// documents for SOAP.
type Message interface{}

// Header is an opaque type used as the SOAP Header element in requests.
type Header interface{}

// AuthHeader is a Header to be encoded as the SOAP Header element in
// requests, to convey credentials for authentication.
type AuthHeader struct {
	Namespace string `xml:"xmlns:ns,attr"`
	Username  string `xml:"ns:username"`
	Password  string `xml:"ns:password"`
}

// Client is a SOAP client.
type Client struct {
	URL                    string              // URL of the server
	Namespace              string              // SOAP Namespace
	ExcludeActionNamespace bool                // Include Namespace to SOAP Action header
	Envelope               string              // Optional SOAP Envelope
	Header                 Header              // Optional SOAP Header
	ContentType            string              // Optional Content-Type (default text/xml)
	Config                 *http.Client        // Optional HTTP client
	Pre                    func(*http.Request) // Optional hook to modify outbound requests
}

//XMLTyper is an interface that provides SetXMLType function
type XMLTyper interface {
	SetXMLType()
}

func setXMLType(v reflect.Value) {
	var vXMLTyperType = reflect.TypeOf((*XMLTyper)(nil)).Elem()
	if !v.IsValid() {
		return
	}
	switch v.Type().Kind() {
	case reflect.Interface:
		setXMLType(v.Elem())
	case reflect.Ptr:
		if v.IsNil() {
			break
		}
		ok := v.Type().Implements(vXMLTyperType)
		if ok {
			v.MethodByName("SetXMLType").Call(nil)
		}
		setXMLType(v.Elem())
	case reflect.Slice:
		for i := 0; i < v.Len(); i++ {
			setXMLType(v.Index(i))
		}
	case reflect.Struct:
		for i := 0; i < v.NumField(); i++ {
			if v.Field(i).CanAddr() {
				setXMLType(v.Field(i).Addr())
			} else {
				setXMLType(v.Field(i))
			}
		}
	}
}

func doRoundTrip(c *Client, setHeaders func(*http.Request), in, out Message) error {
	setXMLType(reflect.ValueOf(in))
	req := &Envelope{
		EnvelopeAttr: c.Envelope,
		NSAttr:       c.Namespace,
		XSIAttr:      XSINamespace,
		Header:       c.Header,
		Body:         Body{Message: in},
	}

	if req.EnvelopeAttr == "" {
		req.EnvelopeAttr = "http://schemas.xmlsoap.org/soap/envelope/"
	}
	if req.NSAttr == "" {
		req.NSAttr = c.URL
	}
	var b bytes.Buffer
	err := xml.NewEncoder(&b).Encode(req)
	if err != nil {
		return err
	}
	cli := c.Config
	if cli == nil {
		cli = http.DefaultClient
	}
	r, err := http.NewRequest("POST", c.URL, &b)
	if err != nil {
		return err
	}
	setHeaders(r)
	if c.Pre != nil {
		c.Pre(r)
	}
	resp, err := cli.Do(r)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		// read only the first Mb of the body in error case
		limReader := io.LimitReader(resp.Body, 1024*1024)
		body, _ := ioutil.ReadAll(limReader)
		return fmt.Errorf("%q: %q", resp.Status, body)
	}
	return xml.NewDecoder(resp.Body).Decode(out)
}

// RoundTrip implements the RoundTripper interface.
func (c *Client) RoundTrip(in, out Message) error {
	headerFunc := func(r *http.Request) {
		var actionName, soapAction string
		if in != nil {
			soapAction = reflect.TypeOf(in).Elem().Name()
		}
		ct := c.ContentType
		if ct == "" {
			ct = "text/xml"
		}
		r.Header.Set("Content-Type", ct)
		if in != nil {
			if c.ExcludeActionNamespace {
				actionName = soapAction
			} else {
				actionName = fmt.Sprintf("%s/%s", c.Namespace, soapAction)
			}
			r.Header.Add("SOAPAction", actionName)
		}
	}
	return doRoundTrip(c, headerFunc, in, out)
}

// RoundTripWithAction implements the RoundTripper interface.
func (c *Client) RoundTripWithAction(soapAction string, in, out Message) error {
	headerFunc := func(r *http.Request) {
		var actionName string
		ct := c.ContentType
		if ct == "" {
			ct = "text/xml"
		}
		r.Header.Set("Content-Type", ct)
		if in != nil {
			if c.ExcludeActionNamespace {
				actionName = soapAction
			} else {
				actionName = fmt.Sprintf("%s/%s", c.Namespace, soapAction)
			}
			r.Header.Add("SOAPAction", actionName)
		}
	}
	return doRoundTrip(c, headerFunc, in, out)
}

// RoundTripSoap12 provides round trip for SOAP 1.2 and implements
// the RoundTripper interface
func (c *Client) RoundTripSoap12(action string, in, out Message) error {
	headerFunc := func(r *http.Request) {
		r.Header.Add("Content-Type", fmt.Sprintf("application/soap+xml; charset=utf-8; action=\"%s\"", action))
	}
	return doRoundTrip(c, headerFunc, in, out)
}

// Envelope is a SOAP envelope.
type Envelope struct {
	XMLName      xml.Name `xml:"SOAP-ENV:Envelope"`
	EnvelopeAttr string   `xml:"xmlns:SOAP-ENV,attr"`
	NSAttr       string   `xml:"xmlns:ns,attr"`
	XSIAttr      string   `xml:"xmlns:xsi,attr,omitempty"`
	Header       Message  `xml:"SOAP-ENV:Header"`
	Body         Body
}

// Body is the body of a SOAP envelope.
type Body struct {
	XMLName xml.Name `xml:"SOAP-ENV:Body"`
	Message Message
}
