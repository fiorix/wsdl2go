// Package wsdlgo provides an encoder from WSDL to Go code.
package wsdlgo

// TODO: make it generate code fully compliant with the spec.
// TODO: support all WSDL types.
// TODO: fully support SOAP bindings, faults, and transports.

import (
	"bufio"
	"bytes"
	"encoding/xml"
	"errors"
	"fmt"
	"go/parser"
	"go/token"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
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
	SetCustomInclude(ci string)
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
	customInclude     []string
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

func (ge *goEncoder) SetCustomInclude(customIncludeString string) {
	ge.customInclude = strings.Split(customIncludeString, ",")
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
	pkg := ge.formatPackageName("providers")
	//pkg := ge.formatPackageName(d.Binding.Name)
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

func (ge *goEncoder) getChildrenElements(d *wsdl.Definitions, parent string, al *[]string) {
	for _, v := range d.Schema.Elements {
		if parent == v.Name {
			if v.Type == "" && v.ComplexType != nil {
				ct := *v.ComplexType

				if len(ct.Sequence.Elements) > 0 {
					for _, el := range ct.Sequence.Elements {
						if strings.HasPrefix(el.Type, "tns") {
							newParent := strings.Split(el.Type, ":")[1]
							ge.getComplexChildren(
								d,
								newParent,
								al,
							)
						}

					}
				}
			}
		}
	}
}

// getStaticResponseMap is a fix for an everything response body from juniper.
// Juniper gives a generic static response for many of it's requests
// we only want to map those responses that actually relate to our custom list
// of functions we want to include... unfortunately because of this I think
// we are going to be losing some of our dynamism - hence the following code
func getStaticResponseMap() (staticResponseMap map[string]bool) {
	staticResponseMap = make(map[string]bool)
	staticResponseMap["Errors"] = true
	staticResponseMap["Warnings"] = true
	staticResponseMap["HotelStaticData"] = true
	staticResponseMap["ZoneList"] = true
	staticResponseMap["HotelList"] = true
	// and so begins the slow decent into code funk
	staticResponseMap["HotelContent"] = true
	staticResponseMap["PackageContent"] = true

	return staticResponseMap

}

func removeUnwantedSequences(s *wsdl.Sequence) {
	staticResponseMap := getStaticResponseMap()
	var keys []int

	for k, el := range s.Elements {
		if !staticResponseMap[el.Name] {
			// remove
			keys = append(keys, k)
		}
	}

	sort.Sort(sort.Reverse(sort.IntSlice(keys)))

	if len(keys) > 0 {
		for _, v := range keys {
			s.Elements[v] = s.Elements[len(s.Elements)-1]
			s.Elements[len(s.Elements)-1] = nil
			s.Elements = s.Elements[:len(s.Elements)-1]
		}
	}
}

func (ge *goEncoder) getComplexChildren(d *wsdl.Definitions, parent string, al *[]string) {
	for _, v := range d.Schema.ComplexTypes {
		if parent == v.Name {
			if parent == "JP_StaticDataRS" || parent == "JP_Contents" {
				// remove sequenced elements that don't apply
				removeUnwantedSequences(v.Sequence)
			}

			if v.Sequence != nil && len(v.Sequence.Elements) > 0 {
				for _, el := range v.Sequence.Elements {
					if strings.HasPrefix(el.Type, "tns") {
						newParent := strings.Split(el.Type, ":")[1]

						ge.getComplexChildren(d, newParent, al)
					}
				}
			}

			if v.Attributes != nil && len(v.Attributes) > 0 {
				for _, a := range v.Attributes {
					if aType := strings.Split(a.Type, ":"); aType[0] == "tns" {
						*al = append(*al, aType[1])
					}
				}
			}

			if v.SimpleContent != nil && v.SimpleContent.Extension != nil && len(v.SimpleContent.Extension.Attributes) > 0 {
				for _, a := range v.SimpleContent.Extension.Attributes {
					if types := strings.Split(a.Type, ":"); types[0] == "tns" {
						ge.getComplexChildren(d, types[1], al)
						// this is to handle a case where SimpleContent is the type
						// and will never be added due to only searching
						// ComplexType with this method... it needs done better
						*al = append(*al, types[1])
					}
				}
			}

			*al = append(*al, v.Name)
		}
	}
}

func (ge *goEncoder) buildAllowable(d *wsdl.Definitions, al *[]string) {
	for _, include := range ge.customInclude {
		// need to include name+"Response"
		*al = append(*al, include+"Response")

		ge.getChildrenElements(d, include, al)
		ge.getComplexChildren(d, include, al)

		// also need to find allowables for include+"Response"
		ge.getChildrenElements(d, include+"Response", al)
		ge.getComplexChildren(d, include+"Response", al)
	}
}

func (ge *goEncoder) removeElementNonAllowables(d *wsdl.Definitions, al []string) {
	delete := make(map[int]struct{})
	var keys []int
	for key, v := range d.Schema.Elements {
		keep := false
		for _, name := range al {
			if name == v.Name {
				keep = true
			}
		}

		if _, ok := delete[key]; !keep && !ok {
			delete[key] = struct{}{}
			keys = append(keys, key)
		}
	}

	sort.Sort(sort.Reverse(sort.IntSlice(keys)))

	if len(delete) > 0 {
		for _, value := range keys {
			d.Schema.Elements[value] = d.Schema.Elements[len(d.Schema.Elements)-1]
			d.Schema.Elements[len(d.Schema.Elements)-1] = nil
			d.Schema.Elements = d.Schema.Elements[:len(d.Schema.Elements)-1]
		}
	}
}

func (ge *goEncoder) removeComplexNonAllowables(d *wsdl.Definitions, al []string) {
	delete := make(map[int]struct{})
	var keys []int
	for key, v := range d.Schema.ComplexTypes {
		keep := false
		for _, name := range al {
			if name == v.Name {
				keep = true
			}
		}

		if _, ok := delete[key]; !keep && !ok {
			delete[key] = struct{}{}
			keys = append(keys, key)
		}
	}

	sort.Sort(sort.Reverse(sort.IntSlice(keys)))

	if len(delete) > 0 {
		for _, value := range keys {
			d.Schema.ComplexTypes[value] = d.Schema.ComplexTypes[len(d.Schema.ComplexTypes)-1]
			d.Schema.ComplexTypes[len(d.Schema.ComplexTypes)-1] = nil
			d.Schema.ComplexTypes = d.Schema.ComplexTypes[:len(d.Schema.ComplexTypes)-1]
		}
	}
}

func (ge *goEncoder) removeSimpleNonAllowables(d *wsdl.Definitions, al []string) {
	delete := make(map[int]struct{})
	var keys []int
	for key, v := range d.Schema.SimpleTypes {
		keep := false
		for _, name := range al {
			if name == v.Name {
				keep = true
			}
		}

		if _, ok := delete[key]; !keep && !ok {
			delete[key] = struct{}{}
			keys = append(keys, key)
		}
	}

	sort.Sort(sort.Reverse(sort.IntSlice(keys)))

	if len(delete) > 0 {
		for _, value := range keys {
			d.Schema.SimpleTypes[value] = d.Schema.SimpleTypes[len(d.Schema.SimpleTypes)-1]
			d.Schema.SimpleTypes[len(d.Schema.SimpleTypes)-1] = nil
			d.Schema.SimpleTypes = d.Schema.SimpleTypes[:len(d.Schema.SimpleTypes)-1]
		}
	}
}

func (ge *goEncoder) removeNonAllowables(d *wsdl.Definitions, al []string) {
	ge.removeElementNonAllowables(d, al)
	ge.removeComplexNonAllowables(d, al)
	ge.removeSimpleNonAllowables(d, al)
}

func (ge *goEncoder) cacheTypes(d *wsdl.Definitions) {
	// build allowable list of types if customInclude is given
	var allowableList []string
	if len(ge.customInclude) > 0 {
		allowableList = ge.customInclude
		ge.buildAllowable(d, &allowableList)
		// remove things not in list
		ge.removeNonAllowables(d, allowableList)
	}

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
		name := ge.trimns(el.Name)
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
		if len(ge.customInclude) > 0 {
			keep := false
			for _, include := range ge.customInclude {
				if include == v.Name {
					keep = true
				}
			}

			if !keep {
				continue
			}
		}
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
		if len(ge.customInclude) > 0 {
			keep := false
			for _, include := range ge.customInclude {
				if include == fn {
					keep = true
				}
			}

			if !keep {
				continue
			}
		}
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
		if len(inParams) != 1 || len(outParams) != 2 {
			continue
		}
		in, out := code(inParams), code(outParams)
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
		a, b := ge.trimns(d.Binding.Type), ge.trimns(d.PortType.Name)
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
		in, out := code(inParams), code(outParams)
		ret := make([]string, len(out))
		for i, p := range out {
			parts := strings.SplitN(p, " ", 2)
			if len(parts) == 2 {
				ret[i] = ge.wsdl2goDefault(parts[1])
			}
		}
		ok := ge.writeSOAPFunc(w, d, op, in, out, ret, outParams[0].xmlToken)
		if !ok {
			ge.needsStdPkg["errors"] = true
			ge.needsStdPkg["context"] = true
			in = append([]string{"ctx context.Context"}, in...)
			ge.fixParamConflicts(in, out)
			fn := ge.fixFuncNameConflicts(strings.Title(op.Name))
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
	γ := struct {
		XMLName xml.Name ` + "`xml:\"Envelope\"`" + `
		Body    struct {
			M {{.OutputType}} ` + "`xml:\"{{.XMLOutputType}}\"`" + `
		}
	}{}
	if err = p.cli.RoundTrip(α, &γ); err != nil {
		return {{.RetDef}}, err
	}
	return {{if .RetPtr}}&{{end}}γ.Body.M, nil
}
`))

func (ge *goEncoder) writeSOAPFunc(w io.Writer, d *wsdl.Definitions, op *wsdl.Operation, in, out, ret []string, xmlToken string) bool {
	if _, exists := ge.soapOps[op.Name]; !exists {
		return false
	}
	// TODO: handle other input params: e.g. SOAP headers.
	if len(in) < 1 || len(out) != 2 {
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
		PortType      string
		Name          string
		Input         string
		Output        string
		OutputType    string
		XMLOutputType string
		RetPtr        bool
		RetDef        string
	}{
		strings.ToLower(d.PortType.Name[:1]) + d.PortType.Name[1:],
		strings.Title(op.Name),
		strings.Join(in, ","),
		strings.Join(out, ","),
		strings.TrimPrefix(typ[1], "*"),
		strings.TrimPrefix(xmlToken, "*"),
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
func (ge *goEncoder) inputParams(op *wsdl.Operation) ([]*parameter, error) {
	if op.Input == nil {
		return []*parameter{}, nil
	}
	im := ge.trimns(op.Input.Message)
	req, ok := ge.messages[im]
	if !ok {
		return nil, fmt.Errorf("operation %q wants input message %q but it's not defined", op.Name, im)
	}
	return ge.genParams(req, true), nil
}

// returns list of function output parameters plus error.
func (ge *goEncoder) outputParams(op *wsdl.Operation) ([]*parameter, error) {
	out := []*parameter{{code: "err error"}}
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
	xmlToken string
}

func code(list []*parameter) []string {
	code := make([]string, len(list))
	for i, p := range list {
		code[i] = p.code
	}
	return code
}

func (ge *goEncoder) genParams(m *wsdl.Message, needsTag bool) []*parameter {
	params := make([]*parameter, len(m.Parts))
	for i, param := range m.Parts {
		var t, token string
		switch {
		case param.Type != "":
			t = ge.wsdl2goType(param.Type)
			token = t
		case param.Element != "":
			t = ge.wsdl2goType(param.Element)
			token = ge.trimns(param.Element)
		}
		name := param.Name
		if isGoKeyword[name] {
			name = "_" + name
		}
		params[i] = &parameter{code: name + " " + t, xmlToken: token}
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
	case "float", "double", "decimal":
		return "float64"
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
	case "anysequence":
		return "interface{}"
	default:
		return "*" + strings.Title(strings.Replace(v, ".", "", -1))
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
		if st.Restriction != nil {
			ge.writeComments(&b, st.Name, "")
			fmt.Fprintf(&b, "type %s %s\n\n", st.Name, ge.wsdl2goType(st.Restriction.Base))
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
			ge.writeComments(&b, st.Name, doc)
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
	name := strings.Title(ct.Name)
	ge.writeComments(w, name, ct.Doc)
	if ct.Sequence != nil && ct.Sequence.Any != nil {
		fmt.Fprintf(w, "type %s []interface{}\n\n", name)
		return nil
	}
	if c > 2 {
		if len(ct.Attributes) > 0 || ct.SimpleContent != nil {
			fmt.Fprintf(w, "type %s struct {\n", name)
			ge.genAttributes(w, ct)
			//todo: check new lines
			fmt.Fprintln(w, "}\n\n")
		} else {
			fmt.Fprintf(w, "type %s struct {}\n\n", name)
		}
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

	err = ge.genAttributes(w, ct)
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

func (ge *goEncoder) genAttributes(w io.Writer, ct *wsdl.ComplexType) error {
	var attributes []*wsdl.Attribute

	if ct.SimpleContent != nil && ct.SimpleContent.Extension != nil {
		if len(ct.SimpleContent.Extension.Attributes) > 0 {
			attributes = ct.SimpleContent.Extension.Attributes
		} else {
			return ge.genExtensionField(w, ct)
		}
	} else if ct.Attributes != nil {
		attributes = ct.Attributes
	} else {
		return errors.New("attributes expected but not present")
	}

	for _, a := range attributes {
		ge.genAttributeField(w, a)
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

func (ge *goEncoder) genExtensionField(w io.Writer, c *wsdl.ComplexType) error {
	e := c.SimpleContent.Extension

	eBase := strings.Split(e.Base, ":")
	var goEType string

	if eBase[0] == "s" {
		goEType = ge.wsdl2goType(eBase[1])
	} else {
		return errors.New(fmt.Sprintf("unhandled base: ", eBase[0]))
	}

	name := strings.Split(c.Name, "_")[1]

	_, err := fmt.Fprintf(
		w,
		"%s %s `xml:\"%s,omitempty,attr\" json:\"%s,omitempty\"`\n",
		name,
		goEType,
		name,
		name,
	)

	if err != nil {
		return err
	}

	return nil

}

func (ge *goEncoder) genAttributeField(w io.Writer, a *wsdl.Attribute) error {
	aType := strings.Split(a.Type, ":")
	var goAType string

	if aType[0] == "s" {
		goAType = ge.wsdl2goType(aType[1])
	} else if aType[0] == "tns" {
		goAType = aType[1]
	}

	_, err := fmt.Fprintf(
		w,
		"%s %s `xml:\"%s,omitempty,attr\" json:\"%s,omitempty\"`\n",
		a.Name,
		goAType,
		a.Name,
		a.Name,
	)

	if err != nil {
		return err
	}

	return nil
}

func (ge *goEncoder) genElementField(w io.Writer, el *wsdl.Element) {
	if el.Ref != "" {
		ref := ge.trimns(el.Ref)
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
