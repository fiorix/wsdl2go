// Package wsdlgo provides an encoder from WSDL to Go code.
package wsdlgo

// TODO: make it generate code fully compliant with the spec.
// TODO: support all WSDL types.
// TODO: fully support SOAP bindings, faults, and transports.

import (
	"bufio"
	"bytes"
	"encoding/xml"
	"fmt"
	"go/parser"
	"go/token"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
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

	// elements cache
	elements map[string]*wsdl.Element

	// funcs cache
	funcs     map[string]*wsdl.Operation
	funcnames []string

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
		elements:    make(map[string]*wsdl.Element),
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

func gofmtPath() (string, error) {
	goroot := os.Getenv("GOROOT")
	if goroot != "" {
		return filepath.Join(goroot, "bin", "gofmt"), nil
	}
	return exec.LookPath("gofmt")

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

	// try to parse the generated code
	fset := token.NewFileSet()
	_, err = parser.ParseFile(fset, "", &b, parser.ParseComments)
	if err != nil {
		var src bytes.Buffer
		s := bufio.NewScanner(strings.NewReader(input))
		for line := 1; s.Scan(); line++ {
			fmt.Fprintf(&src, "%5d\t%s\n", line, s.Bytes())
		}
		return fmt.Errorf("generated bad code: %v\n%s", err, src.String())
	}

	// dat pipe to gofmt
	path, err := gofmtPath()
	if err != nil {
		return fmt.Errorf("cannot find gofmt with err: %v", err)
	}
	cmd := exec.Cmd{
		Path:   path,
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
	pkg := ge.formatPackageName(d.Binding.Name)
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
		writeComments(w, "Namespace", "")
		fmt.Fprintf(w, "var Namespace = %q\n\n", d.TargetNamespace)
	}
	_, err = io.Copy(w, &b)
	return err
}

func (ge *goEncoder) formatPackageName(pkg string) string {
	return strings.Replace(strings.ToLower(pkg), ".", "", -1)
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
	// cache elements from schema
	ge.cacheElements(d.Schema.Elements)
	// cache elements from complex types
	for _, ct := range ge.ctypes {
		ge.cacheComplexTypeElements(ct)
	}
}

func (ge *goEncoder) cacheComplexTypeElements(ct *wsdl.ComplexType) {
	if ct.AllElements != nil {
		ge.cacheElements(ct.AllElements)
	}
	if ct.Sequence != nil {
		ge.cacheElements(ct.Sequence.Elements)
	}
	cc := ct.ComplexContent
	if cc != nil {
		cce := cc.Extension
		if cce != nil && cce.Sequence != nil {
			seq := cce.Sequence
			for _, cct := range seq.ComplexTypes {
				ge.cacheComplexTypeElements(cct)
			}
			ge.cacheElements(seq.Elements)
		}
	}
}

func (ge *goEncoder) cacheElements(ct []*wsdl.Element) {
	for _, el := range ct {
		if el.Name == "" || el.Type == "" {
			continue
		}
		name := trimns(el.Name)
		if _, exists := ge.elements[name]; exists {
			continue
		}
		ge.elements[name] = el
		ct := el.ComplexType
		if ct != nil {
			ge.cacheElements(ct.AllElements)
			if ct.Sequence != nil {
				ge.cacheElements(ct.Sequence.Elements)
			}
		}
	}
}

func (ge *goEncoder) cacheFuncs(d *wsdl.Definitions) {
	// operations are declared as boilerplate go functions
	for _, v := range d.PortType.Operations {
		ge.funcs[v.Name] = v
	}
	ge.funcnames = make([]string, len(ge.funcs))
	i := 0
	for k := range ge.funcs {
		ge.funcnames[i] = k
		i++
	}
	sort.Strings(ge.funcnames)
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
	funcs := make([]*interfaceTypeFunc, len(ge.funcs))
	// Looping over the operations to determine what are the interface
	// functions.
	i := 0
	for _, fn := range ge.funcnames {
		op := ge.funcs[fn]
		if _, exists := ge.soapOps[op.Name]; !exists {
			// TODO: rpc?
			continue
		}
		inParams, err := ge.inputParams(op)
		if err != nil {
			return err
		}
		outParams, err := ge.outputParams(op)
		if err != nil {
			return err
		}
		fixParamConflicts(inParams, outParams)

		name := strings.Title(op.Name)
		var doc bytes.Buffer
		writeComments(&doc, name, op.Doc)
		funcs[i] = &interfaceTypeFunc{
			Doc:    doc.String(),
			Name:   name,
			Input:  asGoParamsString(inParams),
			Output: asGoParamsString(outParams),
		}
		i++
	}
	n := d.PortType.Name
	return interfaceTypeT.Execute(w, &struct {
		Name  string
		Impl  string // private type that implements the interface
		Funcs []*interfaceTypeFunc
	}{
		strings.Title(n),
		strings.ToLower(n)[:1] + n[1:],
		funcs[:i],
	})
}

var portTypeT = template.Must(template.New("portType").Parse(`
// {{.Name}} implements the {{.Interface}} interface.
type {{.Name}} struct {
	cli *soap.Client
}

`))

func (ge *goEncoder) writePortType(w io.Writer, d *wsdl.Definitions) error {
	if len(ge.funcs) == 0 {
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
		a, b := trimns(d.Binding.Type), trimns(d.PortType.Name)
		if a != b {
			return fmt.Errorf(
				"binding %q requires port type %q but it's not defined",
				d.Binding.Name, d.Binding.Type)
		}
	}
	if len(ge.funcs) == 0 {
		return nil
	}
	for _, fn := range ge.funcnames {
		op := ge.funcs[fn]

		inParams, err := ge.inputParams(op)
		if err != nil {
			return err
		}
		outParams, err := ge.outputParams(op)
		if err != nil {
			return err
		}
		fixParamConflicts(inParams, outParams)
		ok := ge.writeSOAPFunc(w, d, op, inParams, outParams, op.Name)
		if !ok {
			writeComments(w, op.Name, op.Doc)
			ge.needsStdPkg["errors"] = true
			ge.needsStdPkg["context"] = true
			inParams = append([]*parameter{&parameter{Name: "ctx", Type: "context.Context"}}, inParams...)
			fn := ge.fixFuncNameConflicts(strings.Title(op.Name))
			fmt.Fprintf(w, "func %s(%s) (%s) {\nreturn\n}\n\n",
				fn,
				asGoParamsString(inParams),
				asGoParamsString(outParams),
			)
		}
	}
	return nil
}

var soapFuncT = template.Must(template.New("soapFunc").Funcs(template.FuncMap{
	"functionParamString": func(params []*parameter) string {
		return asGoParamsString(params)
	},
	"fieldNameString": func(s string) string {
		t := strings.Title(s)
		return t
	},
}).Parse(`
// {{.Name}} was was auto-generated from WSDL
func (p *{{.PortType}}) {{.Name}}( {{functionParamString .InParams}}) ({{functionParamString .OutParams}}) {
	// request message
	message := struct {
		XMLName xml.Name ` + "`" + `xml:"{{.MessageName}}"` + "`" + `
{{- range .InParams }}
		{{fieldNameString .Name}} {{.Type}} ` + "`" + `xml:"{{.XMLName}}"` + "`" + `
{{- end }}
	}{
{{- range .InParams}}
		{{fieldNameString .Name}}: {{.Name}},
{{- end }}
	}

	// response message
	out := struct {
		XMLName xml.Name ` + "`xml:\"Envelope\"`" + `
		Body struct {
{{- range .OutParams }}
{{- 	if ne .Name "err" }}
			{{fieldNameString .Name}} {{.Type}} ` + "`" + `name:"{{.XMLName}},omitempty"` + "`" + `
{{- 	end }}
{{- end }}
		}
	}{}
	
	// simple context to pass SOAPAction--later versions to offer better call control
	ctx := context.WithValue( context.Background(), "SOAPAction", "{{.SoapAction}}" )
	if err = p.cli.RoundTrip(ctx, message, &out); err != nil {
		return
	}

{{- range .OutParams }}
{{- 	if ne .Name "err" }}
	{{.Name}} = out.Body.{{fieldNameString .Name}}
{{- 	end }}
{{- end }}

	return
}
`))

func (ge *goEncoder) writeSOAPFunc(w io.Writer, d *wsdl.Definitions, op *wsdl.Operation, inParams, outParams []*parameter, messageName string) bool {
	if _, exists := ge.soapOps[op.Name]; !exists {
		return false
	}
	ge.needsStdPkg["context"] = true
	ge.needsStdPkg["encoding/xml"] = true
	ge.needsExtPkg["github.com/fiorix/wsdl2go/soap"] = true

	var soapAction string
	soapOp := ge.soapOps[op.Name]
	if soapOp.Operation != nil {
		soapAction = soapOp.Operation.SoapAction
	}
	soapFuncT.Execute(w, &struct {
		PortType    string
		Name        string
		InParams    []*parameter
		SoapAction  string
		OutParams   []*parameter
		MessageName string
	}{
		strings.ToLower(d.PortType.Name[:1]) + d.PortType.Name[1:],
		strings.Title(op.Name),
		inParams,
		soapAction,
		outParams,
		messageName,
	})
	return true
}

// returns list of function input parameters.
func (ge *goEncoder) inputParams(op *wsdl.Operation) ([]*parameter, error) {
	if op.Input == nil {
		return []*parameter{}, nil
	}
	im := trimns(op.Input.Message)
	req, ok := ge.messages[im]
	if !ok {
		return nil, fmt.Errorf("operation %q wants input message %q but it's not defined", op.Name, im)
	}

	var parts []*wsdl.Part
	if len(op.ParameterOrder) != 0 {
		order := strings.Split(op.ParameterOrder, " ")
		parts = make([]*wsdl.Part, len(order))

		// Use a map to run O( len(parts)+len(order) )
		partLookup := map[string]*wsdl.Part{}
		for _, part := range req.Parts {
			partLookup[part.Name] = part
		}
		for i, o := range order {
			parts[i] = partLookup[o]
		}

	} else {
		parts = req.Parts
	}

	return ge.genParams(parts, true), nil
}

// returns list of function output parameters plus error.
func (ge *goEncoder) outputParams(op *wsdl.Operation) ([]*parameter, error) {
	errP := &parameter{Name: "err", Type: "error", XMLName: "Err"}
	if op.Output == nil {
		return []*parameter{errP}, nil
	}
	om := trimns(op.Output.Message)
	resp, ok := ge.messages[om]
	if !ok {
		return nil, fmt.Errorf("operation %q wants output message %q but it's not defined", op.Name, om)
	}

	return append(ge.genParams(resp.Parts, false), errP), nil
}

var isGoKeyword = map[string]bool{
	"break":       true,
	"case":        true,
	"chan":        true,
	"const":       true,
	"continue":    true,
	"default":     true,
	"else":        true,
	"defer":       true,
	"fallthrough": true,
	"for":         true,
	"func":        true,
	"go":          true,
	"goto":        true,
	"if":          true,
	"import":      true,
	"interface":   true,
	"map":         true,
	"package":     true,
	"range":       true,
	"return":      true,
	"select":      true,
	"struct":      true,
	"switch":      true,
	"type":        true,
	"var":         true,
}

type parameter struct {
	Name    string
	Type    string
	XMLName string
}

func (p parameter) asGo() string {
	c := p.Name + " " + p.Type
	return c
}

func asGoParamsString(params []*parameter) string {
	goP := make([]string, len(params))
	for i, p := range params {
		goP[i] = p.asGo()
	}
	return strings.Join(goP, ", ")
}

func scrubName(unscrubbed string) (name string) {

	name = unscrubbed

	// Golint wants fields and variable names with "ID" instead of "Id"
	idFinder := regexp.MustCompile("(.*)Id$")
	name = idFinder.ReplaceAllString(name, "${1}ID")

	// Golint doesn't want fields and variable names with "_" anywhere--remove mid-word matches,
	underscoreFinder := regexp.MustCompile("(.+)_(.+)")
	name = underscoreFinder.ReplaceAllString(name, "${1}${2}")
	// replace edge underscores with "Var"-- set n as 2 because there are maximum 2 edge cases
	name = strings.Replace(name, "_", "Var", 2)

	// Because other languages just don't care
	if isGoKeyword[name] {
		name = unscrubbed + unscrubbed
	}

	return name
}

func (ge *goEncoder) genParams(parts []*wsdl.Part, needsTag bool) []*parameter {
	params := make([]*parameter, len(parts))
	for i, part := range parts {
		var t string
		switch {
		case part.Type != "":
			t = ge.wsdl2goType(part.Type)
		case part.Element != "":
			t = ge.wsdl2goType(part.Element)
		}

		name := scrubName(part.Name)

		params[i] = &parameter{
			Name:    name,
			Type:    t,
			XMLName: part.Name,
		}
		if needsTag {
			ge.needsTag[strings.TrimPrefix(t, "*")] = true
		}
	}
	return params
}

// Fixes conflicts between function and type names.
func (ge *goEncoder) fixFuncNameConflicts(name string) string {
	if _, exists := ge.stypes[name]; exists {
		name += "Func"
		return ge.fixFuncNameConflicts(name)
	}
	if _, exists := ge.ctypes[name]; exists {
		name += "Func"
		return ge.fixFuncNameConflicts(name)
	}
	return name
}

// Fixes request and response parameters with the same name, in place.
// Each string in the slice consists of Go's "name Type", we only
// compare names. In case of a conflict, we set the response one
// in the form of respName.
func fixParamConflicts(in, out []*parameter) {
	retest := false
	for _, req := range in {
		for i, resp := range out {
			if req.Name == resp.Name {
				resp.Name = "resp" + strings.Title(resp.Name) + string(i)
				retest = true
			}
		}
	}
	if retest {
		fixParamConflicts(in, out)
	}
}

// Converts types from wsdl type to Go type.
func (ge *goEncoder) wsdl2goType(t string) string {
	// TODO: support other types.
	v := trimns(t)
	if _, exists := ge.stypes[v]; exists {
		return v
	}
	switch strings.ToLower(v) {
	case "int", "integer":
		return "int"
	case "long":
		return "int64"
	case "float", "double", "decimal":
		return "float64"
	case "decimal":
		// big.Float works well enough with XML serialization,
		// but falls short for JSON and YAML--may need a custom type to wrap this
		ge.needsStdPkg["math/big"] = true
		return "big.Float"
	case "boolean":
		return "bool"
	case "hexbinary", "base64binary":
		return "[]byte"
	case "string", "anyuri", "token", "qname":
		return "string"
	case "date":
		ge.needsDateType = true
		return "Date"
	case "time":
		ge.needsTimeType = true
		return "Time"
	case "nonnegativeinteger":
		return "uint"
	case "datetime":
		ge.needsDateTimeType = true
		return "DateTime"
	case "duration":
		ge.needsDurationType = true
		return "Duration"
	case "anysequence", "anytype", "anysimpletype":
		return "interface{}"
	default:
		return "*" + strings.Title(strings.Replace(v, ".", "", -1))
	}
}

func trimns(s string) string {
	n := strings.SplitN(s, ":", 2)
	if len(n) == 2 {
		return n[1]
	}
	return s
}

// writeGoTypes writes Go types from WSDL types to w.
//
// Types are written in this order, alphabetically: date types that we
// generate, simple types, then complex types.
func (ge *goEncoder) writeGoTypes(w io.Writer, d *wsdl.Definitions) error {
	var b bytes.Buffer
	for _, name := range ge.sortedSimpleTypes() {
		st := ge.stypes[name]
		if st.Restriction != nil {
			writeComments(&b, st.Name, "")
			fmt.Fprintf(&b, "type %s %s\n\n", scrubName(st.Name), ge.wsdl2goType(st.Restriction.Base))
			ge.genValidator(&b, st.Name, st.Restriction)
		} else if st.Union != nil {
			types := strings.Split(st.Union.MemberTypes, " ")
			ntypes := make([]string, len(types))
			for i, t := range types {
				t = strings.TrimSpace(t)
				if t == "" {
					continue
				}
				ntypes[i] = ge.wsdl2goType(t)
			}
			doc := st.Name + " is a union of: " + strings.Join(ntypes, ", ")
			writeComments(&b, st.Name, doc)
			fmt.Fprintf(&b, "type %s interface{}\n\n", st.Name)
		}
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
		writeComments(w, c.name, c.name+" in WSDL format.")
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
	name := strings.Title(ct.Name)
	writeComments(w, name, ct.Doc)

	// Do array search
	if ct.ComplexContent != nil && ct.ComplexContent.Restriction != nil && ct.ComplexContent.Restriction.Attribute != nil {
		// Check that restriction base is "soapenc:Array"
		if "Array" == trimns(ct.ComplexContent.Restriction.Base) && ct.ComplexContent.Restriction.Attribute.Key == "arrayType" {
			wsdlType := ct.ComplexContent.Restriction.Attribute.Value
			wsdlArrayOf := trimns(wsdlType)
			matches := regexp.MustCompile("([^\\[\\]]+)(.+)").FindStringSubmatch(wsdlArrayOf)
			goArrayOf := matches[1]
			dimensions := matches[2]
			fmt.Fprintf(w, "type %s %s%s\n\n", name, dimensions, goArrayOf)
			return nil
		}
	}

	if ct.Sequence != nil && ct.Sequence.Any != nil {
		fmt.Fprintf(w, "type %s []interface{}\n\n", name)
		return nil
	}
	if c > 2 {
		fmt.Fprintf(w, "type %s struct {}\n\n", name)
		return nil
	}
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
		base, exists := ge.ctypes[trimns(ext.Base)]
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
	if el.Ref != "" {
		ref := trimns(el.Ref)
		nel, ok := ge.elements[ref]
		if !ok {
			return
		}
		el = nel
	}
	if el.Type == "" {
		el.Type = "string"
	}
	var slicetype string
	if el.Type == "" && el.ComplexType != nil {
		seq := el.ComplexType.Sequence
		if seq != nil {
			if len(seq.Elements) == 1 {
				n := el.Name
				el = el.ComplexType.Sequence.Elements[0]
				slicetype = el.Name
				el.Name = n
			} else if len(seq.Any) == 1 {
				el = &wsdl.Element{
					Name: el.Name,
					Type: "anysequence",
					Min:  seq.Any[0].Min,
					Max:  seq.Any[0].Max,
				}
				slicetype = el.Name
			}
		}
	}
	tag := el.Name
	name := scrubName(strings.Title(el.Name))
	fmt.Fprintf(w, "%s ", name)
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
func writeComments(w io.Writer, typeName, comment string) {
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
