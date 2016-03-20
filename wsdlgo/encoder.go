// Package wsdlgo provides an encoder from WSDL to Go code.
package wsdlgo

// TODO: make it generate code fully compliant with the spec.
// TODO: support all WSDL types.
// TODO: fully support SOAP bindings, faults, and transports.

import (
	"bytes"
	"encoding/xml"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strconv"
	"strings"

	"github.com/fiorix/wsdl2go/wsdl"
)

// An Encoder generates Go code from WSDL definitions.
type Encoder interface {
	// Encode generates Go code from d.
	Encode(d *wsdl.Definitions) error

	// SetClient records the given http client that
	// is used when fetching remote parts of WSDL
	// and WSDL schemas.
	SetClient(c *http.Client)
}

type goEncoder struct {
	// where to write Go code
	w io.Writer

	// http client
	http *http.Client

	// types cache
	stypes map[string]*wsdl.SimpleType
	ctypes map[string]*wsdl.ComplexType

	// funcs cache
	funcs map[string]*wsdl.Operation

	// messages cache
	messages map[string]*wsdl.Message

	// whether to add supporting types
	needsReflect      bool
	needsDateType     bool
	needsTimeType     bool
	needsDateTimeType bool
	needsDurationType bool
}

// NewEncoder creates and initializes an Encoder that generates code to w.
func NewEncoder(w io.Writer) Encoder {
	return &goEncoder{
		w:        w,
		http:     http.DefaultClient,
		stypes:   make(map[string]*wsdl.SimpleType),
		ctypes:   make(map[string]*wsdl.ComplexType),
		funcs:    make(map[string]*wsdl.Operation),
		messages: make(map[string]*wsdl.Message),
	}
}

func (ge *goEncoder) SetClient(c *http.Client) {
	ge.http = c
}

func (ge *goEncoder) Encode(d *wsdl.Definitions) error {
	if d == nil {
		return nil
	}
	var b bytes.Buffer
	err := ge.encode(&b, d)
	if err != nil {
		return err
	}
	if b.Len() == 0 {
		return nil
	}
	var errb bytes.Buffer
	input := b.String()
	// dat pipe
	cmd := exec.Cmd{
		Path:   filepath.Join(os.Getenv("GOROOT"), "bin", "gofmt"),
		Stdin:  &b,
		Stdout: ge.w,
		Stderr: &errb,
	}
	err = cmd.Run()
	if err != nil {
		var x bytes.Buffer
		fmt.Fprintf(&x, "gofmt: %v\n", err)
		if errb.Len() > 0 {
			fmt.Fprintf(&x, "gofmt stderr:\n%s\n", errb.String())
		}
		fmt.Fprintf(&x, "generated code:\n%s\n", input)
		return fmt.Errorf(x.String())
	}
	return nil
}

func (ge *goEncoder) encode(w io.Writer, d *wsdl.Definitions) error {
	err := ge.importParts(d)
	if err != nil {
		return fmt.Errorf("wsdl import: %v", err)
	}
	ge.cacheTypes(d)
	ge.cacheFuncs(d)
	ge.cacheMessages(d)
	pkg := strings.ToLower(d.Binding.Name)
	if pkg == "" {
		pkg = "internal"
	}
	var b bytes.Buffer
	err = ge.writeGoFuncs(&b, d) // functions first, for clarity
	if err != nil {
		return err
	}
	err = ge.writeGoTypes(&b, d)
	if err != nil {
		return err
	}
	fmt.Fprintf(w, "package %s\n\nimport (\n\"errors\"\n", pkg)
	if ge.needsReflect {
		fmt.Fprintf(w, "\"reflect\"\n")
	}
	fmt.Fprintf(w, "\n\"golang.org/x/net/context\"\n)\n\n")
	_, err = io.Copy(w, &b)
	return err
}

func (ge *goEncoder) importParts(d *wsdl.Definitions) error {
	err := ge.importRoot(d)
	if err != nil {
		return err
	}
	return ge.importSchema(d)
}

func (ge *goEncoder) importRoot(d *wsdl.Definitions) error {
	for _, imp := range d.Imports {
		if imp.Location == "" {
			continue
		}
		err := ge.importRemote(imp.Location, &d)
		if err != nil {
			return err
		}
	}
	return nil
}

func (ge *goEncoder) importSchema(d *wsdl.Definitions) error {
	for _, imp := range d.Schema.Imports {
		if imp.Location == "" {
			continue
		}
		err := ge.importRemote(imp.Location, &d.Schema)
		if err != nil {
			return err
		}
	}
	return nil
}

// download xml from url, decode in v.
func (ge *goEncoder) importRemote(url string, v interface{}) error {
	resp, err := ge.http.Get(url)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	return xml.NewDecoder(resp.Body).Decode(v)
}

func (ge *goEncoder) cacheTypes(d *wsdl.Definitions) {
	// operation types are declared as go struct types
	for _, v := range d.Schema.Elements {
		if v.Type == "" && v.ComplexType != nil {
			ct := *v.ComplexType
			ct.Name = v.Name
			ge.ctypes[v.Name] = &ct
		}
	}
	// simple types map 1:1 to go basic types
	for _, v := range d.Schema.SimpleTypes {
		ge.stypes[v.Name] = v
	}
	// complex types are declared as go struct types
	for _, v := range d.Schema.ComplexTypes {
		ge.ctypes[v.Name] = v
	}
}

func (ge *goEncoder) cacheFuncs(d *wsdl.Definitions) {
	// operations are declared as boilerplate go functions
	for _, v := range d.PortType.Operations {
		ge.funcs[v.Name] = v
	}
}

func (ge *goEncoder) cacheMessages(d *wsdl.Definitions) {
	for _, v := range d.Messages {
		ge.messages[v.Name] = v
	}
}

// writeGoFuncs writes Go function definitions from WSDL types to w.
// Functions are written in the same order of the WSDL document.
func (ge *goEncoder) writeGoFuncs(w io.Writer, d *wsdl.Definitions) error {
	if d.Binding.Type != "" {
		a, b := ge.trimns(d.Binding.Type), ge.trimns(d.PortType.Name)
		if a != b {
			return fmt.Errorf(
				"binding %q requires port type %q but it's not defined",
				d.Binding.Name, d.Binding.Type)
		}
	}
	if d.PortType.Operations == nil {
		return nil
	}
	for _, op := range d.PortType.Operations {
		// TODO: really rename input to have Request suffix?
		ge.writeComments(w, op.Name, op.Doc)
		in, err := ge.inputParams(op)
		if err != nil {
			return err
		}
		out, err := ge.outputParams(op)
		if err != nil {
			return err
		}
		ret := make([]string, len(out))
		for i, p := range out {
			parts := strings.SplitN(p, " ", 2)
			if len(parts) == 2 {
				ret[i] = ge.wsdl2goDefault(parts[1])
			}
		}
		ge.fixParamConflicts(in, out)
		fmt.Fprintf(w, "func %s(%s) (%s) {\nreturn %s\n}\n\n",
			strings.Title(op.Name),
			strings.Join(in, ","),
			strings.Join(out, ","),
			strings.Join(ret, ","),
		)
	}
	return nil
}

func (ge *goEncoder) inputParams(op *wsdl.Operation) ([]string, error) {
	in := []string{"ctx context.Context"}
	if op.Input == nil {
		return in, nil
	}
	im := ge.trimns(op.Input.Message)
	req, ok := ge.messages[im]
	if !ok {
		return nil, fmt.Errorf("operation %q wants input message %q but it's not defined", op.Name, im)
	}
	return append(in, ge.genParams(req, "Request")...), nil
}

func (ge *goEncoder) outputParams(op *wsdl.Operation) ([]string, error) {
	out := []string{"err error"}
	if op.Output == nil {
		return out, nil
	}
	om := ge.trimns(op.Output.Message)
	resp, ok := ge.messages[om]
	if !ok {
		return nil, fmt.Errorf("operation %q wants output message %q but it's not defined", op.Name, om)
	}
	return append(ge.genParams(resp, "Response"), out[0]), nil
}

func (ge *goEncoder) genParams(m *wsdl.Message, suffix string) []string {
	params := make([]string, len(m.Parts))
	for i, param := range m.Parts {
		var t string
		switch {
		case param.Type != "":
			t = ge.wsdl2goType(param.Type, suffix)
		case param.Element != "":
			t = ge.wsdl2goType(param.Element, suffix)
		}
		params[i] = param.Name + " " + t
	}
	return params
}

// Fixes request and response parameters with the same name, in place.
// Each string in the slice consists of Go's "name Type", we only
// compare names. In case of a conflict, we set the response one
// in the form of respName.
func (ge *goEncoder) fixParamConflicts(req, resp []string) {
	for _, a := range req {
		for j, b := range resp {
			x := strings.SplitN(a, " ", 2)[0]
			y := strings.SplitN(b, " ", 2)
			if len(y) > 1 {
				if x == y[0] {
					n := strings.Title(y[0])
					resp[j] = "resp" + n + " " + y[1]
				}
			}
		}
	}
}

// Converts types from wsdl type to Go type. If t is a complex type
// (a struct, as opposed to int or string) and a suffix is provided,
// we look for the suffix in its name and add if needed. When we do
// that, we also update the list of cached ctypes to match this new
// type name, with the suffix (e.g. ping -> pingRequest). This is
// to avoid ambiguous parameter and function names.
func (ge *goEncoder) wsdl2goType(t, suffix string) string {
	// TODO: support other types.
	v := ge.trimns(t)
	if _, exists := ge.stypes[v]; exists {
		return v
	}
	switch strings.ToLower(v) {
	case "int":
		return "int"
	case "long":
		return "int64"
	case "float":
		return "float64"
	case "boolean":
		return "bool"
	case "string":
		return "string"
	case "date":
		ge.needsDateType = true
		return "Date"
	case "time":
		ge.needsTimeType = true
		return "Time"
	case "datetime":
		ge.needsDateTimeType = true
		return "DateTime"
	case "duration":
		ge.needsDurationType = true
		return "Duration"
	default:
		if suffix != "" && !strings.HasSuffix(t, suffix) {
			ge.renameType(t, t+suffix)
			t = v + suffix
		} else {
			t = v
		}
		if len(t) == 0 {
			return "FIXME"
		}
		return "*" + strings.Title(t)
	}
}

// Returns the default Go type for the given wsdl type.
func (ge *goEncoder) wsdl2goDefault(t string) string {
	v := ge.trimns(t)
	if v != "" && v[0] == '*' {
		v = v[1:]
	}
	switch v {
	case "error":
		return `errors.New("not implemented")`
	case "bool":
		return "false"
	case "int", "int64", "float64":
		return "0"
	case "string":
		return `""`
	default:
		return "&" + v + "{}"
	}
}

func (ge *goEncoder) trimns(s string) string {
	n := strings.SplitN(s, ":", 2)
	if len(n) == 2 {
		return n[1]
	}
	return s
}

func (ge *goEncoder) renameType(old, name string) {
	// TODO: rename Elements that point to this type also?
	ct, exists := ge.ctypes[old]
	if !exists {
		old = ge.trimns(old)
		ct, exists = ge.ctypes[old]
		if !exists {
			return
		}
		name = ge.trimns(name)
	}
	ct.Name = name
	delete(ge.ctypes, old)
	ge.ctypes[name] = ct
}

// writeGoTypes writes Go types from WSDL types to w.
//
// Types are written in this order, alphabetically: date types that we
// generate, simple types, then complex types.
func (ge *goEncoder) writeGoTypes(w io.Writer, d *wsdl.Definitions) error {
	var b bytes.Buffer
	for _, name := range ge.sortedSimpleTypes() {
		st := ge.stypes[name]
		if st.Restriction == nil {
			continue
		}
		ge.writeComments(&b, st.Name, "")
		fmt.Fprintf(w, "type %s %s\n\n", st.Name, ge.wsdl2goType(st.Restriction.Base, ""))
		ge.genValidator(&b, st.Name, st.Restriction)
	}
	var err error
	for _, name := range ge.sortedComplexTypes() {
		ct := ge.ctypes[name]
		err = ge.genGoStruct(&b, ct)
		if err != nil {
			return err
		}
	}
	ge.genDateTypes(w) // must be called last
	_, err = io.Copy(w, &b)
	return err
}

func (ge *goEncoder) sortedSimpleTypes() []string {
	keys := make([]string, len(ge.stypes))
	i := 0
	for k := range ge.stypes {
		keys[i] = k
		i++
	}
	sort.Strings(keys)
	return keys
}

func (ge *goEncoder) sortedComplexTypes() []string {
	keys := make([]string, len(ge.ctypes))
	i := 0
	for k := range ge.ctypes {
		keys[i] = k
		i++
	}
	sort.Strings(keys)
	return keys
}

func (ge *goEncoder) genDateTypes(w io.Writer) {
	cases := []struct {
		needs bool
		name  string
		code  string
	}{
		{
			needs: ge.needsDateType,
			name:  "Date",
			code:  "type Date string\n\n",
		},
		{
			needs: ge.needsTimeType,
			name:  "Time",
			code:  "type Time string\n\n",
		},
		{
			needs: ge.needsDateTimeType,
			name:  "DateTime",
			code:  "type DateTime string\n\n",
		},
		{
			needs: ge.needsDurationType,
			name:  "Duration",
			code:  "type Duration string\n\n",
		},
	}
	for _, c := range cases {
		if !c.needs {
			continue
		}
		ge.writeComments(w, c.name, c.name+" in WSDL format.")
		io.WriteString(w, c.code)
	}
}

func (ge *goEncoder) genValidator(w io.Writer, typeName string, r *wsdl.Restriction) {
	if len(r.Enum) == 0 {
		return
	}
	args := make([]string, len(r.Enum))
	t := ge.wsdl2goType(r.Base, "")
	for i, v := range r.Enum {
		if t == "string" {
			args[i] = strconv.Quote(v.Value)
		} else {
			args[i] = v.Value
		}
	}
	fmt.Fprintf(w, "// Validate validates the %s.", typeName)
	fmt.Fprintf(w, "\nfunc (v %s) Validate() bool {\n", typeName)
	fmt.Fprintf(w, "for _, vv := range []%s{\n", t)
	fmt.Fprintf(w, "%s,\n", strings.Join(args, ",\n"))
	fmt.Fprintf(w, "}{\nif reflect.DeepEqual(v, vv) { return true }\n}\nreturn false\n}\n\n")
	if !ge.needsReflect {
		ge.needsReflect = true
	}
}

func (ge *goEncoder) genGoStruct(w io.Writer, ct *wsdl.ComplexType) error {
	if ct.Abstract {
		return nil
	}
	c := 0
	if len(ct.AllElements) == 0 {
		c++
	}
	if ct.ComplexContent == nil || ct.ComplexContent.Extension == nil {
		c++
	}
	if ct.Sequence == nil {
		c++
	} else if len(ct.Sequence.ComplexTypes) == 0 && len(ct.Sequence.Elements) == 0 {
		c++
	}
	if c > 2 {
		// dont generate empty structs
		return nil
	}
	ge.writeComments(w, ct.Name, ct.Doc)
	fmt.Fprintf(w, "type %s struct {\n", ct.Name)
	err := ge.genStructFields(w, ct)
	if err != nil {
		return err
	}
	fmt.Fprintf(w, "}\n\n")
	return nil
}

func (ge *goEncoder) genStructFields(w io.Writer, ct *wsdl.ComplexType) error {
	err := ge.genComplexContent(w, ct)
	if err != nil {
		return err
	}
	return ge.genElements(w, ct)
}

func (ge *goEncoder) genComplexContent(w io.Writer, ct *wsdl.ComplexType) error {
	if ct.ComplexContent == nil || ct.ComplexContent.Extension == nil {
		return nil
	}
	ext := ct.ComplexContent.Extension
	if ext.Base != "" {
		base, exists := ge.ctypes[ge.trimns(ext.Base)]
		if exists {
			err := ge.genStructFields(w, base)
			if err != nil {
				return err
			}
		}
	}
	if ext.Sequence == nil {
		return nil
	}
	seq := ext.Sequence
	for _, v := range seq.ComplexTypes {
		err := ge.genElements(w, v)
		if err != nil {
			return err
		}
	}
	for _, v := range ext.Sequence.Elements {
		ge.genElementField(w, v)
	}
	return nil
}

func (ge *goEncoder) genElements(w io.Writer, ct *wsdl.ComplexType) error {
	for _, el := range ct.AllElements {
		ge.genElementField(w, el)
	}
	if ct.Sequence == nil {
		return nil
	}
	for _, el := range ct.Sequence.Elements {
		ge.genElementField(w, el)
	}
	return nil
}

func (ge *goEncoder) genElementField(w io.Writer, el *wsdl.Element) {
	if el.Type == "" && el.ComplexType != nil {
		seq := el.ComplexType.Sequence
		if seq != nil && len(seq.Elements) == 1 {
			n := el.Name
			el = el.ComplexType.Sequence.Elements[0]
			el.Name = n
		}
	}
	fmt.Fprintf(w, "%s ", strings.Title(el.Name))
	if el.Max != "" && el.Max != "1" {
		fmt.Fprintf(w, "[]")
	}
	fmt.Fprint(w, ge.wsdl2goType(el.Type, ""))
	fmt.Fprintf(w, " `xml:\"%s", el.Name)
	if el.Nillable || el.Min == 0 {
		fmt.Fprintf(w, ",omitempty")
	}
	fmt.Fprintf(w, "\"`\n")
}

// writeComments writes comments to w, capped at ~80 columns.
func (ge *goEncoder) writeComments(w io.Writer, typeName, comment string) {
	comment = strings.Trim(strings.Replace(comment, "\n", " ", -1), " ")
	if comment == "" {
		comment = strings.Title(typeName) + " was auto-generated from WSDL."
	}
	count, line := 0, ""
	words := strings.Split(comment, " ")
	for _, word := range words {
		if line == "" {
			count, line = 2, "//"
		}
		count += len(word)
		if count > 60 {
			fmt.Fprintf(w, "%s %s\n", line, word)
			count, line = 0, ""
			continue
		}
		line = line + " " + word
		count++
	}
	if line != "" {
		fmt.Fprintf(w, "%s\n", line)
	}
	return
}
