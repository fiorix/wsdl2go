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
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"text/template"

	"github.com/grid-x/wsdl2go/wsdl"
)

// An Encoder generates Go code from WSDL definitions.
type Encoder interface {
	// Encode generates Go code from d.
	Encode(d *wsdl.Definitions) error

	// SetPackageName sets some fmt.Stringer that can produce package name
	SetPackageName(packageName fmt.Stringer)

	// SetClient records the given http client that
	// is used when fetching remote parts of WSDL
	// and WSDL schemas.
	SetClient(c *http.Client)

	// SetLocalNamespace allows overriding of the Namespace in XMLName instead
	// of the one specified in wsdl
	SetLocalNamespace(namespace string)
}

type goEncoder struct {
	// where to write Go code
	w io.Writer

	// http client
	http *http.Client

	// some mechanism to name package
	packageName fmt.Stringer

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
	needsTag          map[string]string
	needsStdPkg       map[string]bool
	needsExtPkg       map[string]bool
	importedSchemas   map[string]bool
	usedNamespaces    map[string]string

	// localNamespace allows overriding of namespace in XMLName
	localNamespace string
}

// NewEncoder creates and initializes an Encoder that generates code to w.
func NewEncoder(w io.Writer) Encoder {
	return &goEncoder{
		w:               w,
		http:            http.DefaultClient,
		stypes:          make(map[string]*wsdl.SimpleType),
		ctypes:          make(map[string]*wsdl.ComplexType),
		elements:        make(map[string]*wsdl.Element),
		funcs:           make(map[string]*wsdl.Operation),
		messages:        make(map[string]*wsdl.Message),
		soapOps:         make(map[string]*wsdl.BindingOperation),
		needsTag:        make(map[string]string),
		needsStdPkg:     make(map[string]bool),
		needsExtPkg:     make(map[string]bool),
		importedSchemas: make(map[string]bool),
	}
}

func (ge *goEncoder) SetPackageName(name fmt.Stringer) {
	ge.packageName = name
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

var numberSequence = regexp.MustCompile(`([a-zA-Z])(\d+)([a-zA-Z]?)`)
var numberReplacement = []byte(`$1 $2 $3`)

func addWordBoundariesToNumbers(s string) string {
	b := []byte(s)
	b = numberSequence.ReplaceAll(b, numberReplacement)
	return string(b)
}

func (ge *goEncoder) Encode(d *wsdl.Definitions) error {
	if d == nil {
		return nil
	}

	// default mechanism to set package name
	if ge.packageName == nil {
		ge.packageName = BindingPackageName(d.Binding)
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
	ge.unionSchemasData(d, &d.Schema)
	err := ge.importParts(d)
	ge.usedNamespaces = d.Namespaces
	if err != nil {
		return fmt.Errorf("wsdl import: %v", err)
	}
	ge.cacheTypes(d)
	ge.cacheFuncs(d)
	ge.cacheMessages(d)
	ge.cacheSOAPOperations(d)

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
		// TODO: probably faulty wsdl?
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

	fmt.Fprintf(w, "package %s\n\nimport (\n", ge.packageName)
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
		schema := &wsdl.Schema{}
		err := ge.importRemote(imp.Location, schema)
		if err != nil {
			return err
		}
		ge.unionSchemasData(d, schema)
		for _, item := range schema.Imports {
			schema = &wsdl.Schema{}
			err := ge.importRemote(item.Location, schema)
			if err != nil {
				return err
			}
			ge.unionSchemasData(d, schema)
		}
		for _, item := range schema.Includes {
			schema = &wsdl.Schema{}
			err := ge.importRemote(item.Location, schema)
			if err != nil {
				return err
			}
			ge.unionSchemasData(d, schema)
		}
	}
	return nil
}

func (ge *goEncoder) unionSchemasData(d *wsdl.Definitions, s *wsdl.Schema) {
	for ns := range s.Namespaces {
		d.Namespaces[ns] = s.Namespaces[ns]
	}
	for _, ct := range s.ComplexTypes {
		ct.TargetNamespace = s.TargetNamespace
	}
	for _, st := range s.SimpleTypes {
		st.TargetNamespace = s.TargetNamespace
	}
	d.Schema.ComplexTypes = append(d.Schema.ComplexTypes, s.ComplexTypes...)
	d.Schema.SimpleTypes = append(d.Schema.SimpleTypes, s.SimpleTypes...)
	d.Schema.Elements = append(d.Schema.Elements, s.Elements...)
}

// download xml from url, decode in v.
func (ge *goEncoder) importRemote(loc string, v interface{}) error {
	_, alreadyImported := ge.importedSchemas[loc]
	if alreadyImported {
		return nil
	}

	u, err := url.Parse(loc)
	if err != nil {
		return err
	}

	var r io.Reader
	switch u.Scheme {
	case "http", "https":
		resp, err := ge.http.Get(loc)
		if err != nil {
			return err
		}
		ge.importedSchemas[loc] = true
		defer resp.Body.Close()
		r = resp.Body
	default:
		file, err := os.Open(u.Path)
		if err != nil {
			return fmt.Errorf("could not open file raw: %s path: %s escaped: %s : %v", u.RawPath, u.Path, u.EscapedPath(), err)
		}

		r = bufio.NewReader(file)
	}
	return xml.NewDecoder(r).Decode(v)

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

func (ge *goEncoder) cacheChoiceTypeElements(choice *wsdl.Choice) {
	if choice != nil {
		for _, cct := range choice.ComplexTypes {
			ge.cacheComplexTypeElements(cct)
		}
		ge.cacheElements(choice.Elements)
	}
}

func (ge *goEncoder) cacheComplexTypeElements(ct *wsdl.ComplexType) {
	if ct.AllElements != nil {
		ge.cacheElements(ct.AllElements)
	}
	if ct.Sequence != nil {
		ge.cacheElements(ct.Sequence.Elements)
	}
	if ct.Choice != nil {
		ge.cacheElements(ct.Choice.Elements)
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

			//Add in Choice elements
			for _, choice := range seq.Choices {
				ge.cacheChoiceTypeElements(choice)
			}
		}
		if cce != nil && cce.Choice != nil {
			ge.cacheChoiceTypeElements(cce.Choice)
		}
	}
}

func (ge *goEncoder) cacheElements(ct []*wsdl.Element) {
	for _, el := range ct {
		if el.Name == "" || el.Type == "" {
			if el.Ref == "" {
				continue
			}
			el.Name = trimns(el.Ref)
			el.Type = el.Name
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
			if ct.Choice != nil {
				ge.cacheElements(ct.Choice.Elements)
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
			// TODO: probably faulty wsdl?
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
		in, out := code(inParams), codeParams(outParams)
		name := goSymbol(op.Name)
		var doc bytes.Buffer
		ge.writeComments(&doc, name, op.Doc)
		funcs[i] = &interfaceTypeFunc{
			Doc:    doc.String(),
			Name:   name,
			Input:  strings.Join(in, ","),
			Output: strings.Join(out, ","),
		}
		i++
	}
	n := d.PortType.Name
	return interfaceTypeT.Execute(w, &struct {
		Name  string
		Impl  string // private type that implements the interface
		Funcs []*interfaceTypeFunc
	}{
		goSymbol(n),
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
		goSymbol(n),
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
		ge.writeComments(w, op.Name, op.Doc)
		inParams, err := ge.inputParams(op)
		if err != nil {
			return err
		}
		outParams, err := ge.outputParams(op)
		if err != nil {
			return err
		}

		ok := ge.writeSOAPFunc(w, d, op, inParams, outParams)
		if !ok {
			in, out := code(inParams), codeParams(outParams)
			ret := make([]string, len(out))
			for i, p := range outParams {
				ret[i] = ge.wsdl2goDefault(p.dataType)
			}

			ge.needsStdPkg["errors"] = true
			ge.needsStdPkg["context"] = true
			in = append([]string{"ctx context.Context"}, in...)

			fn := ge.fixFuncNameConflicts(goSymbol(op.Name))
			fmt.Fprintf(w, "func %s(%s) (%s) {\nreturn %s\n}\n\n",
				fn,
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
	α := struct {
		{{if .OpInputDataType}}
			{{if .RPCStyle}}M{{end}} {{.OpInputDataType}} ` + "`xml:\"{{.OpName}}\"`" + `
		{{end}}
	}{
		{{if .OpInputDataType}}{{.OpInputDataType}} {
			{{range $index, $element := .InputNames}}{{$element}},
			{{end}}
		},{{end}}
	}

	γ := struct {
		{{if .OpResponseDataType}}
			{{if .RPCStyle}}M {{end}}{{.OpResponseDataType}} ` + "`xml:\"{{.OpResponseName}}\"`" + `
		{{end}}
	}{}
	if err := p.cli.RoundTripWithAction("{{.Name}}", α, &γ); err != nil {
		return {{.RetDef}}
	}
	return {{range $index, $element := .OpOutputNames}}{{index $.OpOutputPrefixes $index}}γ.{{if $.RPCStyle}}M.{{end}}{{$element}}, {{end}}nil
}
`))

var soapActionFuncT = template.Must(template.New("soapActionFunc").Parse(
	`func (p *{{.PortType}}) {{.Name}}({{.Input}}) ({{.Output}}) {
	α := struct {
		{{if .OpInputDataType}}
			{{if .RPCStyle}}M{{end}} {{.OpInputDataType}} ` + "`xml:\"{{.OpName}}\"`" + `
		{{end}}
	}{
		{{if .OpInputDataType}}{{.OpInputDataType}} {
			{{range $index, $element := .InputNames}}{{$element}},
			{{end}}
		},{{end}}
	}

	γ := struct {
		{{if .OpResponseDataType}}
			{{if .RPCStyle}}M {{end}}{{.OpResponseDataType}} ` + "`xml:\"{{.OpResponseName}}\"`" + `
		{{end}}
	}{}
	if err := p.cli.{{.RoundTripType}}("{{.Action}}", α, &γ); err != nil {
		return {{.RetDef}}
	}
	return {{range $index, $element := .OpOutputNames}}{{index $.OpOutputPrefixes $index}}γ.{{if $.RPCStyle}}M.{{end}}{{$element}}, {{end}}nil
}
`))

func (ge *goEncoder) writeSOAPFunc(w io.Writer, d *wsdl.Definitions, op *wsdl.Operation, in, out []*parameter) bool {
	if _, exists := ge.soapOps[op.Name]; !exists {
		// TODO: probably faulty wsdl?
		return false
	}

	// Do we need to wrap into a operation element?
	rpcStyle := false

	if d.Binding.BindingType != nil {
		rpcStyle = d.Binding.BindingType.Style == "rpc"
	}

	ge.needsExtPkg["github.com/grid-x/wsdl2go/soap"] = true

	// inputNames describe the accessors to the input parameter names
	inputNames := make([]string, len(in))
	for index, name := range in {
		returnVal := maskKeywordUsage(name.code)

		if !strings.HasPrefix(name.dataType, "*") {
			returnVal = "&" + returnVal
		}

		inputNames[index] = returnVal
	}

	// outputDataTypes describe the data types which are returned by the func
	outputDataTypes := make([]string, len(out))

	// retDefaults describes the default return values in case of an error
	retDefaults := make([]string, len(out))

	// operationOutputNames describes the fields which are part of the response we unmarshal
	// len-1, because the last parameter is error, which is not part of the xml response we unmarshal
	operationOutputNames := make([]string, len(out)-1)
	operationOutputPrefixes := make([]string, len(out)-1)

	for index, name := range out {
		outputDataTypes[index] = name.dataType

		// operationOutputNames names will only be computed till len-1
		if index == len(out)-1 {
			continue
		}

		operationOutputNames[index] = strings.ToUpper(name.code[:1]) + name.code[1:]
		operationOutputPrefixes[index] = ""
		retDefaults[index] = "nil"

		// If the output is >not< a pointer, we need to return the value of the response
		if !strings.HasPrefix(name.dataType, "*") {
			operationOutputPrefixes[index] = "*"

			// Also - only resolve the default for non-pointer returns (otherwise nil suffices)
			retDefaults[index] = ge.wsdl2goDefault(name.dataType)
		}
	}
	retDefaults[len(retDefaults)-1] = "err"

	// Check if we need to prefix the op with a namespace
	namespacedOpName := op.Name
	nsSplit := strings.Split(ge.funcs[op.Name].Input.Message, ":")
	if len(nsSplit) > 1 {
		namespacedOpName = nsSplit[0] + ":" + namespacedOpName
	}

	// The response name is always the operation name + "Response" according to specification.
	// Note, we also omit the namespace, since this does currently not work reliable with golang
	// (See: https://github.com/golang/go/issues/14407)
	opResponseName := op.Name + "Response"

	// No-input operations can be inlined into an anonymous struct on rpc, and omitted otherwise
	operationInputDataType := ""

	if len(in) > 0 && op.Input != nil {
		operationInputDataType = ge.sanitizedOperationsType(ge.messages[trimns(op.Input.Message)].Name)
	} else if rpcStyle {
		operationInputDataType = "struct{}"
	}

	// No-output operations can be inlined into an anonymous struct on rpc, and omitted otherwise
	operationOutputDataType := ""

	if len(out) > 0 && op.Output != nil {
		operationOutputDataType = ge.sanitizedOperationsType(ge.messages[trimns(op.Output.Message)].Name)
	} else if rpcStyle {
		operationInputDataType = "struct{}"
	}

	soapFunctionName := "RoundTripSoap12"
	soapAction := ""
	if bindingOp, exists := ge.soapOps[op.Name]; exists {
		soapAction = bindingOp.Operation.Action
		if soapAction == "" {
			soapFunctionName = "RoundTripWithAction"
			soapAction = bindingOp.Operation11.Action
		}
	}
	if soapAction != "" {
		soapActionFuncT.Execute(w, &struct {
			RoundTripType      string
			Action             string
			PortType           string
			Name               string
			OpName             string
			OpInputDataType    string
			InputNames         []string
			OpResponseName     string
			OpResponseDataType string
			OpOutputNames      []string
			OpOutputPrefixes   []string
			Input              string
			Output             string
			RetDef             string
			RPCStyle           bool
		}{
			soapFunctionName,
			soapAction,
			strings.ToLower(d.PortType.Name[:1]) + d.PortType.Name[1:],
			goSymbol(op.Name),
			namespacedOpName,
			operationInputDataType,
			inputNames,
			opResponseName,
			operationOutputDataType,
			operationOutputNames,
			operationOutputPrefixes,
			strings.Join(code(in), ","),
			strings.Join(outputDataTypes, ","),
			strings.Join(retDefaults, ","),
			rpcStyle,
		})
		return true
	}
	soapFuncT.Execute(w, &struct {
		PortType           string
		Name               string
		OpName             string
		OpInputDataType    string
		InputNames         []string
		OpResponseName     string
		OpResponseDataType string
		OpOutputNames      []string
		OpOutputPrefixes   []string
		Input              string
		Output             string
		RetDef             string
		RPCStyle           bool
	}{
		strings.ToLower(d.PortType.Name[:1]) + d.PortType.Name[1:],
		goSymbol(op.Name),
		namespacedOpName,
		operationInputDataType,
		inputNames,
		opResponseName,
		operationOutputDataType,
		operationOutputNames,
		operationOutputPrefixes,
		strings.Join(code(in), ","),
		strings.Join(outputDataTypes, ","),
		strings.Join(retDefaults, ","),
		rpcStyle,
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
func (ge *goEncoder) inputParams(op *wsdl.Operation) ([]*parameter, error) {
	if op.Input == nil {
		return []*parameter{}, nil
	}
	im := trimns(op.Input.Message)
	req, ok := ge.messages[im]
	if !ok {
		return nil, fmt.Errorf("operation %q wants input message %q but it's not defined", op.Name, im)
	}

	// TODO: I had to disable this for my use case - do other use cases still work with false? -> nope changed it back to true
	return ge.genParams(req, true), nil
}

// returns list of function output parameters plus error.
func (ge *goEncoder) outputParams(op *wsdl.Operation) ([]*parameter, error) {
	out := []*parameter{{code: "err", dataType: "error"}}

	if op.Output == nil {
		return out, nil
	}
	om := trimns(op.Output.Message)
	resp, ok := ge.messages[om]
	if !ok {
		return nil, fmt.Errorf("operation %q wants output message %q but it's not defined", op.Name, om)
	}
	return append(ge.genParams(resp, false), out[0]), nil
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
	code     string
	dataType string
	xmlToken string
}

func code(list []*parameter) []string {
	code := make([]string, len(list))
	for i, p := range list {
		code[i] = maskKeywordUsage(p.code) + " " + p.dataType
	}
	return code
}

func codeParams(list []*parameter) []string {
	code := make([]string, len(list))
	for i, p := range list {
		code[i] = p.dataType
	}
	return code
}

func maskKeywordUsage(code string) string {
	returnVal := code

	if isGoKeyword[code] {
		returnVal = "_" + code
	}

	return returnVal
}

func (ge *goEncoder) genParams(m *wsdl.Message, needsTag bool) []*parameter {
	params := make([]*parameter, len(m.Parts))
	for i, param := range m.Parts {
		var t, token, elName string
		switch {
		case param.Type != "":
			t = ge.wsdl2goType(param.Type)
			elName = trimns(param.Type)
			token = t
		case param.Element != "":
			elName = trimns(param.Element)
			if el, ok := ge.elements[elName]; ok {
				t = ge.wsdl2goType(trimns(el.Type))
			} else {
				t = ge.wsdl2goType(param.Element)
			}
			token = trimns(param.Element)
		}
		params[i] = &parameter{code: param.Name, dataType: t, xmlToken: token}
		if needsTag {
			ge.needsStdPkg["encoding/xml"] = true
			ge.needsTag[strings.TrimPrefix(t, "*")] = elName
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
func (ge *goEncoder) fixParamConflicts(req, resp []string) {
	for _, a := range req {
		for j, b := range resp {
			x := strings.SplitN(a, " ", 2)[0]
			y := strings.SplitN(b, " ", 2)
			if len(y) > 1 {
				if x == y[0] {
					n := goSymbol(y[0])
					resp[j] = "resp" + n + " " + y[1]
				}
			}
		}
	}
}

// Helps to clean up operation names, so we can generate
// nice datatype names which make golang happy.
// E.g. - a soap operation gkstServer_getVersion is sanitized
// to gkstServerGetVersion (remove snake case)
func (ge *goEncoder) sanitizedOperationsType(opName string) string {
	return "Operation" + goSymbol(opName)
}

// Converts types from wsdl type to Go type.
func (ge *goEncoder) wsdl2goType(t string) string {
	// TODO: support other types.
	v := trimns(t)
	if _, exists := ge.stypes[v]; exists {
		return goSymbol(v)
	}
	switch strings.ToLower(v) {
	case "int":
		return "int"
	case "integer":
		return "int64" // todo: replace this with math/big since integer is infinite set
	case "long":
		return "int64"
	case "float", "double", "decimal":
		return "float64"
	case "boolean":
		return "bool"
	case "hexbinary", "base64binary":
		return "[]byte"
	case "string", "anyuri", "token", "nmtoken", "qname", "language", "id":
		return "string"
	case "date":
		ge.needsDateType = true
		return "Date"
	case "time":
		ge.needsTimeType = true
		return "Time"
	case "nonnegativeinteger":
		return "uint"
	case "positiveinteger":
		return "uint64"
	case "normalizedstring":
		return "string"
	case "unsignedint":
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
		return "*" + goSymbol(v)
	}
}

// Returns the default Go type for the given wsdl type.
func (ge *goEncoder) wsdl2goDefault(t string) string {
	v := trimns(t)
	if v != "" && v[0] == '*' {
		v = v[1:]
	}
	switch v {
	case "error":
		return `errors.New("not implemented")`
	case "bool":
		return "false"
	case "uint", "int", "int64", "float64":
		return "0"
	case "string":
		return `""`
	case "[]byte", "interface{}":
		return "nil"
	default:
		return "&" + v + "{}"
	}
}

func (ge *goEncoder) renameType(old, name string) {
	// TODO: rename Elements that point to this type also?
	ct, exists := ge.ctypes[old]
	if !exists {
		old = trimns(old)
		ct, exists = ge.ctypes[old]
		if !exists {
			return
		}
		name = trimns(name)
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
		stname := goSymbol(st.Name)
		if st.Restriction != nil {
			ge.writeComments(&b, stname, "")
			fmt.Fprintf(&b, "type %s %s\n\n", stname, ge.wsdl2goType(st.Restriction.Base))
			ge.genValidator(&b, stname, st.Restriction)
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
			doc := stname + " is a union of: " + strings.Join(ntypes, ", ")
			ge.writeComments(&b, stname, doc)
			fmt.Fprintf(&b, "type %s interface{}\n\n", stname)
		}
	}
	var err error
	for _, name := range ge.sortedComplexTypes() {
		ct := ge.ctypes[name]
		err = ge.genGoStruct(&b, d, ct)
		if err != nil {
			return err
		}
		ge.genGoXMLTypeFunction(&b, ct)
	}

	// Operation wrappers - mainly used for rpc, not exclusively
	for _, name := range ge.sortedOperations() {
		ct := ge.soapOps[name]

		err = ge.genGoOpStruct(&b, d, ct)
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

func (ge *goEncoder) sortedOperations() []string {
	keys := make([]string, len(ge.soapOps))
	i := 0
	for k := range ge.soapOps {
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

func (ge *goEncoder) genGoXMLTypeFunction(w io.Writer, ct *wsdl.ComplexType) {
	if ct.ComplexContent == nil || ct.ComplexContent.Extension == nil || ct.TargetNamespace == "" {
		return
	}

	ext := ct.ComplexContent.Extension
	if ext.Base != "" && !ct.Abstract {
		ge.writeComments(w, "SetXMLType", "")
		fmt.Fprintf(w, "func (t *%s) SetXMLType() {\n", goSymbol(ct.Name))
		fmt.Fprintf(w, "if t.OverrideTypeAttrXSI != nil {\n")
		fmt.Fprintf(w, "    t.TypeAttrXSI = *t.OverrideTypeAttrXSI\n")
		fmt.Fprintf(w, "} else {\n")
		fmt.Fprintf(w, "    t.TypeAttrXSI = \"objtype:%s\"\n", ct.Name)
		fmt.Fprintf(w, "}\n")
		fmt.Fprintf(w, "if t.OverrideTypeNamespace != nil {\n")
		fmt.Fprintf(w, "    t.TypeNamespace = *t.OverrideTypeNamespace\n")
		fmt.Fprintf(w, "} else {\n")
		fmt.Fprintf(w, "    t.TypeNamespace = \"%s\"\n", ct.TargetNamespace)
		fmt.Fprintf(w, "}\n}\n\n")
	}
}

// helper function to print out the XMLName
func (ge *goEncoder) genXMLName(w io.Writer, targetNamespace string, name string) {
	if elName, ok := ge.needsTag[name]; ok {
		if ge.localNamespace == "" {
			fmt.Fprintf(w, "XMLName xml.Name `xml:\"%s %s\" json:\"-\" yaml:\"-\"`\n",
				targetNamespace, elName)
		} else {
			fmt.Fprintf(w, "XMLName xml.Name `xml:\"%s:%s\" json:\"-\" yaml:\"-\"`\n",
				ge.localNamespace, elName)
		}
	}
}

var invalidGoSymbol = regexp.MustCompile(`[0-9_]*[^0-9a-zA-Z_]+`)

func goSymbol(s string) string {
	v := invalidGoSymbol.ReplaceAllString(trimns(s), " ")
	var name string
	for _, part := range strings.Split(v, " ") {
		name += strings.Title(part)
	}
	return name
}

func trimns(s string) string {
	n := strings.SplitN(s, ":", 2)
	if len(n) == 2 {
		return n[1]
	}
	return s
}

func (ge *goEncoder) genGoStruct(w io.Writer, d *wsdl.Definitions, ct *wsdl.ComplexType) error {
	c := 0
	if len(ct.AllElements) == 0 {
		c++
	}
	if ct.ComplexContent == nil || ct.ComplexContent.Extension == nil {
		c++
	}
	if ct.Sequence == nil && ct.Choice == nil {
		c++
	} else if ct.Sequence != nil &&
		(len(ct.Sequence.ComplexTypes) == 0 && len(ct.Sequence.Elements) == 0 && len(ct.Sequence.Choices) == 0) {
		c++
	} else if ct.Choice != nil && (len(ct.Choice.ComplexTypes) == 0 && len(ct.Choice.Elements) == 0) {
		c++
	}

	name := goSymbol(ct.Name)
	ge.writeComments(w, name, ct.Doc)
	if ct.Abstract {
		fmt.Fprintf(w, "type %s interface{}\n\n", name)
		return nil
	}
	if ct.Sequence != nil && ct.Sequence.Any != nil {
		if len(ct.Sequence.Elements) == 0 {
			fmt.Fprintf(w, "type %s []interface{}\n\n", name)
			return nil
		}
	}
	if ct.Choice != nil && ct.Choice.Any != nil {
		if len(ct.Choice.Elements) == 0 {
			fmt.Fprintf(w, "type %s []interface{}\n\n", name)
			return nil
		}
	}
	if ct.ComplexContent != nil {
		restr := ct.ComplexContent.Restriction
		if restr != nil && len(restr.Attributes) == 1 && restr.Attributes[0].ArrayType != "" {
			fmt.Fprintf(w, "type %s struct {\n", name)
			typ := strings.SplitN(trimns(restr.Attributes[0].ArrayType), "[", 2)[0]
			fmt.Fprintf(w, "Items []*%s `xml:\"item,omitempty\" json:\"item,omitempty\" yaml:\"item,omitempty\"`\n", typ)
			fmt.Fprintf(w, "}\n\n")
			return nil
		}
	}
	if c > 2 && len(ct.Attributes) == 0 {
		fmt.Fprintf(w, "type %s struct {\n", name)
		ge.genXMLName(w, d.TargetNamespace, name)
		fmt.Fprintf(w, "}\n\n")
		return nil
	}
	fmt.Fprintf(w, "type %s struct {\n", name)
	ge.genXMLName(w, d.TargetNamespace, name)
	err := ge.genStructFields(w, d, ct)

	if ct.ComplexContent != nil && ct.ComplexContent.Extension != nil {
		fmt.Fprint(w, "TypeAttrXSI   string `xml:\"xsi:type,attr,omitempty\"`\n")
		fmt.Fprint(w, "TypeNamespace string `xml:\"xmlns:objtype,attr,omitempty\"`\n")
		fmt.Fprint(w, "\n")
		fmt.Fprint(w, "OverrideTypeAttrXSI   *string `xml:\"-\"`\n")
		fmt.Fprint(w, "OverrideTypeNamespace *string `xml:\"-\"`\n")
	}
	if err != nil {
		return err
	}
	fmt.Fprintf(w, "}\n\n")
	return nil
}

func (ge *goEncoder) genGoOpStruct(w io.Writer, d *wsdl.Definitions, bo *wsdl.BindingOperation) error {
	name := goSymbol(bo.Name)

	inputMessage := ge.messages[trimns(ge.funcs[bo.Name].Input.Message)]

	// No-Op on operations which don't take arguments
	// (These can be inlined, and don't need to pollute the file)
	if len(inputMessage.Parts) > 0 {
		ge.genOpStructMessage(w, d, name, inputMessage)
	}

	// Output messages are always required
	ge.genOpStructMessage(w, d, name, ge.messages[trimns(ge.funcs[bo.Name].Output.Message)])

	return nil
}

func (ge *goEncoder) genStructFields(w io.Writer, d *wsdl.Definitions, ct *wsdl.ComplexType) error {
	err := ge.genComplexContent(w, d, ct)
	if err != nil {
		return err
	}
	return ge.genElements(w, ct)
}

func (ge *goEncoder) genOpStructMessage(w io.Writer, d *wsdl.Definitions, name string, message *wsdl.Message) {
	sanitizedMessageName := ge.sanitizedOperationsType(message.Name)

	ge.writeComments(w, sanitizedMessageName, "Operation wrapper for "+name+".")
	ge.writeComments(w, sanitizedMessageName, "")
	fmt.Fprintf(w, "type %s struct {\n", sanitizedMessageName)
	if elName, ok := ge.needsTag[sanitizedMessageName]; ok {
		fmt.Fprintf(w, "XMLName xml.Name `xml:\"%s %s\" json:\"-\" yaml:\"-\"`\n",
			d.TargetNamespace, elName)
	}

	for _, part := range message.Parts {
		wsdlType := part.Type

		// Probably soap12
		if wsdlType == "" {
			wsdlType = part.Element
		}

		ge.genElementField(w, &wsdl.Element{
			XMLName: part.XMLName,
			Name:    part.Name,
			Type:    wsdlType,
			// TODO: Maybe one could make guesses about nillable?
		})
	}

	fmt.Fprintf(w, "}\n\n")
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

	for _, attr := range ext.Attributes {
		ge.genAttributeField(w, attr)
	}

	sequences := make([]*wsdl.Sequence, 0)
	if ext.Sequence != nil {
		sequences = append(sequences, ext.Sequence)
		for _, choice := range ext.Sequence.Choices {
			tmpSeq := &wsdl.Sequence{
				ComplexTypes: choice.ComplexTypes,
				Elements:     choice.Elements,
				Any:          choice.Any}
			sequences = append(sequences, tmpSeq)
		}
	}
	if ext.Choice != nil {
		tmpSeq := &wsdl.Sequence{
			ComplexTypes: ext.Choice.ComplexTypes,
			Elements:     ext.Choice.Elements,
			Any:          ext.Choice.Any}
		sequences = append(sequences, tmpSeq)
	}
	for _, seq := range sequences {
		for _, v := range seq.ComplexTypes {
			err := ge.genElements(w, v)
			if err != nil {
				return err
			}
		}
		for _, v := range seq.Elements {
			ge.genElementField(w, v)
		}

	}
	return nil
}

func (ge *goEncoder) genElements(w io.Writer, ct *wsdl.ComplexType) error {
	for _, el := range ct.AllElements {
		ge.genElementField(w, el)
	}
	if ct.Sequence != nil {
		for _, el := range ct.Sequence.Elements {
			ge.genElementField(w, el)
		}
		for _, choice := range ct.Sequence.Choices {
			for _, el := range choice.Elements {
				ge.genElementField(w, el)
			}
		}
	}
	if ct.Choice != nil {
		for _, el := range ct.Choice.Elements {
			ge.genElementField(w, el)
		}
	}
	for _, attr := range ct.Attributes {
		ge.genAttributeField(w, attr)
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
	var slicetype string
	if el.Type == "" && el.ComplexType != nil {
		seq := el.ComplexType.Sequence
		if seq == nil && el.ComplexType.Choice != nil {
			seq = &wsdl.Sequence{
				ComplexTypes: el.ComplexType.Choice.ComplexTypes,
				Elements:     el.ComplexType.Choice.Elements,
				Any:          el.ComplexType.Choice.Any}
		}
		if seq != nil {
			if len(seq.Elements) == 1 {
				n := el.Name
				seqel := seq.Elements[0]
				el = new(wsdl.Element)
				*el = *seqel
				slicetype = seqel.Name
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
	et := el.Type
	if et == "" {
		et = "string"
	}
	tag := el.Name
	fmt.Fprintf(w, "%s ", goSymbol(el.Name))
	if el.Max != "" && el.Max != "1" {
		fmt.Fprintf(w, "[]")
		if slicetype != "" {
			tag = el.Name + ">" + slicetype
		}
	}
	typ := ge.wsdl2goType(et)
	if el.Nillable || el.Min == 0 {
		tag += ",omitempty"
		//since we add omitempty tag, we should add pointer to type.
		//thus xmlencoder can differ not-initialized fields from zero-initialized values
		if !strings.HasPrefix(typ, "*") {
			typ = "*" + typ
		}
	}
	fmt.Fprintf(w, "%s `xml:\"%s\" json:\"%s\" yaml:\"%s\"`\n",
		typ, tag, tag, tag)
}

func (ge *goEncoder) genAttributeField(w io.Writer, attr *wsdl.Attribute) {
	if attr.Name == "" && attr.Ref != "" {
		attr.Name = trimns(attr.Ref)
	}
	if attr.Type == "" {
		attr.Type = "string"
	}

	tag := fmt.Sprintf("%s,attr", attr.Name)
	fmt.Fprintf(w, "%s ", goSymbol(attr.Name))
	typ := ge.wsdl2goType(attr.Type)
	if attr.Nillable || attr.Min == 0 {
		tag += ",omitempty"
	}
	fmt.Fprintf(w, "%s `xml:\"%s\" json:\"%s\" yaml:\"%s\"`\n",
		typ, tag, tag, tag)
}

// writeComments writes comments to w, capped at ~80 columns.
func (ge *goEncoder) writeComments(w io.Writer, typeName, comment string) {
	comment = strings.Trim(strings.Replace(comment, "\n", " ", -1), " ")
	if comment == "" {
		comment = goSymbol(typeName) + " was auto-generated from WSDL."
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

// SetLocalNamespace allows overridding of namespace in XMLName
func (ge *goEncoder) SetLocalNamespace(s string) {
	ge.localNamespace = s
}
