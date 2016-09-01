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
	"runtime"
	"sort"
	"strconv"
	"strings"
	"text/template"

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

	// soap operations cache
	soapOps map[string]*wsdl.BindingOperation

	// whether to add supporting types
	needsDateType     bool
	needsTimeType     bool
	needsDateTimeType bool
	needsDurationType bool
	needsTag          map[string]bool
	needsStdPkg       map[string]bool
	needsExtPkg       map[string]bool
}

// NewEncoder creates and initializes an Encoder that generates code to w.
func NewEncoder(w io.Writer) Encoder {
	return &goEncoder{
		w:           w,
		http:        http.DefaultClient,
		stypes:      make(map[string]*wsdl.SimpleType),
		ctypes:      make(map[string]*wsdl.ComplexType),
		funcs:       make(map[string]*wsdl.Operation),
		messages:    make(map[string]*wsdl.Message),
		soapOps:     make(map[string]*wsdl.BindingOperation),
		needsTag:    make(map[string]bool),
		needsStdPkg: make(map[string]bool),
		needsExtPkg: make(map[string]bool),
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

	goroot := os.Getenv("GOROOT")
	if goroot == "" {
		// no goroot is set, check to see whether it is windows or not
		if runtime.GOOS == "windows" {
			goroot = "C:\\go"
		} else {
			goroot = "/usr/local/go"
		}

	}

	cmd := exec.Cmd{
		Path:   filepath.Join(goroot, "bin", "gofmt"),
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
	ge.cacheSOAPOperations(d)
	pkg := strings.ToLower(d.Binding.Name)
	if pkg == "" {
		pkg = "internal"
	}
	var b bytes.Buffer
	var ff []func(io.Writer, *wsdl.Definitions) error
	if len(ge.soapOps) > 0 {
		ff = append(ff,
			ge.writeInterfaceFuncs,
			ge.writeGoTypes,
			ge.writePortType,
			ge.writeGoFuncs,
		)
	} else {
		// this is rpc; meh
		ff = append(ff,
			ge.writeGoFuncs,
			ge.writeGoTypes,
		)
	}
	for _, f := range ff {
		err := f(&b, d)
		if err != nil {
			return err
		}
	}
	fmt.Fprintf(w, "package %s\n\nimport (\n", pkg)
	for pkg := range ge.needsStdPkg {
		fmt.Fprintf(w, "%q\n", pkg)
	}
	if len(ge.needsStdPkg) > 0 {
		fmt.Fprintf(w, "\n")
	}
	for pkg := range ge.needsExtPkg {
		fmt.Fprintf(w, "%q\n", pkg)
	}
	fmt.Fprintf(w, ")\n\n")
	if d.TargetNamespace != "" {
		ge.writeComments(w, "Namespace", "")
		fmt.Fprintf(w, "var Namespace = %q\n\n", d.TargetNamespace)
	}
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

func (ge *goEncoder) cacheSOAPOperations(d *wsdl.Definitions) {
	for _, v := range d.Binding.Operations {
		ge.soapOps[v.Name] = v
	}
}

var interfaceTypeT = template.Must(template.New("interfaceType").Parse(`
// New{{.Name}} creates an initializes a {{.Name}}.
func New{{.Name}}(cli *soap.Client) {{.Name}} {
	return &{{.Impl}}{cli}
}

// {{.Name}} was auto-generated from WSDL
// and defines interface for the remote service. Useful for testing.
type {{.Name}} interface {
{{- range .Funcs }}
{{.Doc}}{{.Name}}({{.Input}}) ({{.Output}})
{{ end }}
}
`))

type interfaceTypeFunc struct{ Doc, Name, Input, Output string }

// writeInterfaceFuncs writes Go interface definitions from WSDL types to w.
// Functions are written in the same order of the WSDL document.
func (ge *goEncoder) writeInterfaceFuncs(w io.Writer, d *wsdl.Definitions) error {
	funcs := make([]*interfaceTypeFunc, len(d.PortType.Operations))
	// Looping over the operations to determine what are the interface
	// functions.
	for i, op := range d.PortType.Operations {
		if _, exists := ge.soapOps[op.Name]; !exists {
			// TODO: rpc?
			continue
		}
		in, err := ge.inputParams(op)
		if err != nil {
			return err
		}
		out, err := ge.outputParams(op)
		if err != nil {
			return err
		}
		if len(in) != 1 && len(out) != 2 {
			continue
		}
		name := strings.Title(op.Name)
		in[0] = renameParam(in[0], "α")
		out[0] = renameParam(out[0], "β")
		var doc bytes.Buffer
		ge.writeComments(&doc, name, op.Doc)
		funcs[i] = &interfaceTypeFunc{
			Doc:    doc.String(),
			Name:   name,
			Input:  strings.Join(in, ","),
			Output: strings.Join(out, ","),
		}
	}
	n := d.PortType.Name
	return interfaceTypeT.Execute(w, &struct {
		Name  string
		Impl  string // private type that implements the interface
		Funcs []*interfaceTypeFunc
	}{
		strings.Title(n),
		strings.ToLower(n)[:1] + n[1:],
		funcs,
	})
}

var portTypeT = template.Must(template.New("portType").Parse(`
// {{.Name}} implements the {{.Interface}} interface.
type {{.Name}} struct {
	cli *soap.Client
}

`))

func (ge *goEncoder) writePortType(w io.Writer, d *wsdl.Definitions) error {
	if d.PortType.Operations == nil || len(d.PortType.Operations) == 0 {
		return nil
	}
	n := d.PortType.Name
	return portTypeT.Execute(w, &struct {
		Name      string
		Interface string
	}{
		strings.ToLower(n)[:1] + n[1:],
		strings.Title(n),
	})
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
		ok := ge.writeSOAPFunc(w, d, op, in, out, ret)
		if !ok {
			ge.needsStdPkg["errors"] = true
			ge.needsExtPkg["golang.org/x/net/context"] = true
			in = append([]string{"ctx context.Context"}, in...)
			ge.fixParamConflicts(in, out)
			fmt.Fprintf(w, "func %s(%s) (%s) {\nreturn %s\n}\n\n",
				strings.Title(op.Name),
				strings.Join(in, ","),
				strings.Join(out, ","),
				strings.Join(ret, ","),
			)
		}
	}
	return nil
}

var soapFuncT = template.Must(template.New("soapFunc").Parse(
	`func (p *{{.PortType}}) {{.Name}}({{.Input}}) ({{.Output}}) {
	γ := struct {
		XMLName xml.Name ` + "`xml:\"Envelope\"`" + `
		Body    struct {
			M {{.OutputType}} ` + "`xml:\"{{.OutputType}}\"`" + `
		}
	}{}
	if err = p.cli.RoundTrip(α, &γ); err != nil {
		return {{.RetDef}}, err
	}
	return {{if .RetPtr}}&{{end}}γ.Body.M, nil
}
`))

func (ge *goEncoder) writeSOAPFunc(w io.Writer, d *wsdl.Definitions, op *wsdl.Operation, in, out, ret []string) bool {
	if _, exists := ge.soapOps[op.Name]; !exists {
		return false
	}
	if len(in) != 1 && len(out) != 2 {
		return false
	}
	ge.needsStdPkg["encoding/xml"] = true
	ge.needsExtPkg["github.com/fiorix/wsdl2go/soap"] = true
	in[0] = renameParam(in[0], "α")
	out[0] = renameParam(out[0], "β")
	typ := strings.SplitN(out[0], " ", 2)
	if strings.HasPrefix(ret[0], "&") {
		ret[0] = "nil"
	}
	soapFuncT.Execute(w, &struct {
		PortType   string
		Name       string
		Input      string
		Output     string
		OutputType string
		RetPtr     bool
		RetDef     string
	}{
		strings.ToLower(d.PortType.Name[:1]) + d.PortType.Name[1:],
		strings.Title(op.Name),
		strings.Join(in, ","),
		strings.Join(out, ","),
		strings.TrimPrefix(typ[1], "*"),
		typ[1][0] == '*',
		ret[0],
	})
	return true
}

func renameParam(p, name string) string {
	v := strings.SplitN(p, " ", 2)
	if len(v) != 2 {
		return p
	}
	return name + " " + v[1]
}

// returns list of function input parameters.
func (ge *goEncoder) inputParams(op *wsdl.Operation) ([]string, error) {
	if op.Input == nil {
		return []string{}, nil
	}
	im := ge.trimns(op.Input.Message)
	req, ok := ge.messages[im]
	if !ok {
		return nil, fmt.Errorf("operation %q wants input message %q but it's not defined", op.Name, im)
	}
	return ge.genParams(req, true), nil
}

// returns list of function output parameters plus error.
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
	return append(ge.genParams(resp, false), out[0]), nil
}

func (ge *goEncoder) genParams(m *wsdl.Message, needsTag bool) []string {
	params := make([]string, len(m.Parts))
	for i, param := range m.Parts {
		var t string
		switch {
		case param.Type != "":
			t = ge.wsdl2goType(param.Type)
		case param.Element != "":
			t = ge.wsdl2goType(param.Element)
		}
		params[i] = param.Name + " " + t
		if needsTag {
			ge.needsTag[strings.TrimPrefix(t, "*")] = true
		}
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

// Converts types from wsdl type to Go type.
func (ge *goEncoder) wsdl2goType(t string) string {
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
	case "float", "double":
		return "float64"
	case "boolean":
		return "bool"
	case "hexbinary", "base64binary":
		return "[]byte"
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
		return "*" + strings.Title(v)
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
	case "[]byte":
		return "nil"
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
		fmt.Fprintf(&b, "type %s %s\n\n", st.Name, ge.wsdl2goType(st.Restriction.Base))
		ge.genValidator(&b, st.Name, st.Restriction)
	}
	var err error
	for _, name := range ge.sortedComplexTypes() {
		ct := ge.ctypes[name]
		err = ge.genGoStruct(&b, d, ct)
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

var validatorT = template.Must(template.New("validator").Parse(`
// Validate validates {{.TypeName}}.
func (v {{.TypeName}}) Validate() bool {
	for _, vv := range []{{.Type}} {
		{{range .Args}}{{.}},{{"\n"}}{{end}}
	}{
		if reflect.DeepEqual(v, vv) {
			return true
		}
	}
	return false
}
`))

func (ge *goEncoder) genValidator(w io.Writer, typeName string, r *wsdl.Restriction) {
	if len(r.Enum) == 0 {
		return
	}
	args := make([]string, len(r.Enum))
	t := ge.wsdl2goType(r.Base)
	for i, v := range r.Enum {
		if t == "string" {
			args[i] = strconv.Quote(v.Value)
		} else {
			args[i] = v.Value
		}
	}
	ge.needsStdPkg["reflect"] = true
	validatorT.Execute(w, &struct {
		TypeName string
		Type     string
		Args     []string
	}{
		typeName,
		t,
		args,
	})
}

func (ge *goEncoder) genGoStruct(w io.Writer, d *wsdl.Definitions, ct *wsdl.ComplexType) error {
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
	name := strings.Title(ct.Name)
	ge.writeComments(w, name, ct.Doc)
	fmt.Fprintf(w, "type %s struct {\n", name)
	if ge.needsTag[name] {
		fmt.Fprintf(w, "XMLName xml.Name `xml:\"%s %s\" json:\"-\" yaml:\"-\"`\n",
			d.TargetNamespace, ct.Name)
	}
	err := ge.genStructFields(w, d, ct)
	if err != nil {
		return err
	}
	fmt.Fprintf(w, "}\n\n")
	return nil
}

func (ge *goEncoder) genStructFields(w io.Writer, d *wsdl.Definitions, ct *wsdl.ComplexType) error {
	err := ge.genComplexContent(w, d, ct)
	if err != nil {
		return err
	}
	return ge.genElements(w, ct)
}

func (ge *goEncoder) genComplexContent(w io.Writer, d *wsdl.Definitions, ct *wsdl.ComplexType) error {
	if ct.ComplexContent == nil || ct.ComplexContent.Extension == nil {
		return nil
	}
	ext := ct.ComplexContent.Extension
	if ext.Base != "" {
		base, exists := ge.ctypes[ge.trimns(ext.Base)]
		if exists {
			err := ge.genStructFields(w, d, base)
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
	var slicetype string
	if el.Type == "" && el.ComplexType != nil {
		seq := el.ComplexType.Sequence
		if seq != nil && len(seq.Elements) == 1 {
			n := el.Name
			el = el.ComplexType.Sequence.Elements[0]
			slicetype = el.Name
			el.Name = n
		}
	}
	tag := el.Name
	fmt.Fprintf(w, "%s ", strings.Title(el.Name))
	if el.Max != "" && el.Max != "1" {
		fmt.Fprintf(w, "[]")
		if slicetype != "" {
			tag = el.Name + ">" + slicetype
		}
	}
	typ := ge.wsdl2goType(el.Type)
	if el.Nillable || el.Min == 0 {
		tag += ",omitempty"
	}
	fmt.Fprintf(w, "%s `xml:\"%s\" json:\"%s\" yaml:\"%s\"`\n",
		typ, tag, tag, tag)
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
