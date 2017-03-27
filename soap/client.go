// Package soap provides a SOAP HTTP client.
package soap

import (
	"bytes"
	"encoding/xml"
	"fmt"
	"io"
	"io/ioutil"
	"net/http"
	"net/http/httputil"
	"regexp"
	"strings"
)

// A RoundTripper executes a request passing the given req as the SOAP
// envelope body. The HTTP response is then de-serialized onto the resp
// object. Returns error in case an error occurs serializing req, making
// the HTTP request, or de-serializing the response.
type RoundTripper interface {
	RoundTrip(req, resp Message) error
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
	URL         string              // URL of the server
	Namespace   string              // SOAP Namespace
	Envelope    string              // Optional SOAP Envelope
	Header      Header              // Optional SOAP Header
	SOAPAction  string              // Optional SOAP Action name
	ContentType string              // Optional Content-Type (default text/xml)
	Config      *http.Client        // Optional HTTP client
	Pre         func(*http.Request) // Optional hook to modify outbound requests
	Debug       bool                // Optional Print the request and response messages
}

// RoundTrip implements the RoundTripper interface.
func (c *Client) RoundTrip(in, out Message) error {
	req := &Envelope{
		EnvelopeAttr: c.Envelope,
		NSAttr:       c.Namespace,
		Header:       EnvelopeHeader{SOAPAction: c.SOAPAction},
		Body:         Body{Message: in},
	}

	if req.EnvelopeAttr == "" {
		req.EnvelopeAttr = "http://schemas.xmlsoap.org/soap/envelope/"
	}

	var b bytes.Buffer
	err := xml.NewEncoder(&b).Encode(req)
	if err != nil {
		return err
	}

	ct := c.ContentType
	if ct == "" {
		ct = "text/xml"
	}

	cli := c.Config
	if cli == nil {
		cli = http.DefaultClient
	}

	r, err := http.NewRequest("POST", c.URL, &b)
	if err != nil {
		return err
	}

	r.Header.Set("Content-Type", ct)

	if c.Pre != nil {
		c.Pre(r)
	}

	if c.Debug {
		requestDump, err := httputil.DumpRequest(r, true)
		if err != nil {
			fmt.Println(err)
		}
		fmt.Println("Request start ----")
		fmt.Println(string(requestDump))
		fmt.Println("Request end ------")
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

	if c.Debug {
		responseDump, err := httputil.DumpResponse(resp, true)
		if err != nil {
			fmt.Println(err)
		}
		fmt.Println("Response start ----")
		fmt.Println(string(responseDump))
		fmt.Println("Response end ------")
	}

	resBodyBuf := new(bytes.Buffer)
	resBodyBuf.ReadFrom(resp.Body)
	respBodyString := resBodyBuf.String()

	// extract SOAP envelope from HTTP response body
	soapEnvelopeRegEx := regexp.MustCompile("<s:Envelope.*s:Envelope>")
	soapEnvelopeMatches := soapEnvelopeRegEx.FindAllStringSubmatch(respBodyString, -1)
	// there should be only one soap envelop in the HTTP response body
	extractedSOAPEnvelope := soapEnvelopeMatches[0][0]
	if c.Debug {
		fmt.Println("Extracted SOAP envelope = ", extractedSOAPEnvelope)
	}

	// remove self closing tags
	selfClosingTagRegExp := regexp.MustCompile("<\\w:\\w+ i:nil=\"true.*?/>")
	clearedSOAPEnvelope := selfClosingTagRegExp.ReplaceAllString(extractedSOAPEnvelope, "")
	if c.Debug {
		fmt.Println("Cleared SOAP Envelope = ", clearedSOAPEnvelope)
	}

	decoder := xml.NewDecoder(strings.NewReader(clearedSOAPEnvelope))
	return decoder.Decode(out)
}

// Envelope is a SOAP envelope.
type Envelope struct {
	XMLName      xml.Name `xml:"SOAP-ENV:Envelope"`
	EnvelopeAttr string   `xml:"xmlns:SOAP-ENV,attr"`
	NSAttr       string   `xml:"xmlns:ns,attr,omitempty"`
	Header       EnvelopeHeader
	Body         Body
}

// Body is the body of a SOAP envelope.
type Body struct {
	XMLName xml.Name `xml:"SOAP-ENV:Body"`
	Message Message
}

// EnvelopeHeader is the header of a SOAP envelope.
type EnvelopeHeader struct {
	XMLName    xml.Name `xml:"SOAP-ENV:Header"`
	SOAPAction string `xml:"http://www.w3.org/2005/08/addressing Action"`
}
