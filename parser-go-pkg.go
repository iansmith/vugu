package vugu

import (
	"bytes"
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"io/ioutil"
	"log"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"time"

	"github.com/cespare/xxhash"
)

// ParserGoPkg knows how to perform source file generation in relation to a package folder.
// Whereas ParserGo handles converting a single template, ParserGoPkg is a higher level interface
// and provides the functionality of the vugugen command line tool.  It will scan a package
// folder for .vugu files and convert them to .go, with the appropriate defaults and logic.
type ParserGoPkg struct {
	pkgPath string
	opts    ParserGoPkgOpts
}

// ParserGoPkgOpts is the options for ParserGoPkg.
type ParserGoPkgOpts struct {
	SkipRegisterComponentTypes bool   // indicates func init() { vugu.RegisterComponentType(...) } code should not be emitted in each file
	SkipGoMod                  bool   // do not try and create go.mod if it doesn't exist
	SkipMainGo                 bool   // do not try and create main_wasm.go if it doesn't exist in a main package
	RootTypeName               string // in case you don't want to use root everytime
}

// NewParserGoPkg returns a new ParserGoPkg with the specified options or default if nil.  The pkgPath is required and must be an absolute path.
func NewParserGoPkg(pkgPath string, opts *ParserGoPkgOpts) *ParserGoPkg {
	ret := &ParserGoPkg{
		pkgPath: pkgPath,
	}
	if opts != nil {
		ret.opts = *opts
	}
	return ret
}

// Run does the work and generates the appropriate .go files from .vugu files.
// It will also create a go.mod file if not present and not SkipGoMod.  Same for main.go and SkipMainGo (will also skip
// if package already has file with package name something other than main).
// Per-file code generation is performed by ParserGo.
func (p *ParserGoPkg) Run() error {

	// vugugen path/to/package

	// comp-name.vugu
	// comp-name.go
	// tag is "comp-name"
	// component type is CompName
	// component data type is CompNameData
	// register unless disabled
	// create CompName if it doesn't exist in the package
	// create CompNameData if it doesn't exist in the package
	// create CompName.NewData with defaults if it doesn't exist in the package

	// how about a default main_wasm.go if one doesn't exist in the package? would be really useful!
	// also go.mod

	// flags:
	// * component registration
	// * skip generating go.mod

	// --

	// record the times of existing files, so we can restore after if the same
	hashTimes, err := fileHashTimes(p.pkgPath)
	if err != nil {
		return err
	}

	pkgF, err := os.Open(p.pkgPath)
	if err != nil {
		return err
	}
	defer pkgF.Close()

	allFileNames, err := pkgF.Readdirnames(-1)
	if err != nil {
		return err
	}

	var vuguFileNames []string
	for _, fn := range allFileNames {
		if filepath.Ext(fn) == ".vugu" {
			vuguFileNames = append(vuguFileNames, fn)
		}
	}

	if len(vuguFileNames) == 0 {
		return fmt.Errorf("no .vugu files found, please create one and try again")
	}

	pkgName := goGuessPkgName(p.pkgPath, p.opts.RootTypeName)

	namesToCheck := []string{"main"}

	// run ParserGo on each file to generate the .go files
	for _, fn := range vuguFileNames {

		baseFileName := strings.TrimSuffix(fn, ".vugu")
		goFileName := baseFileName + ".go"
		compTypeName := fnameToGoTypeName(goFileName)

		pg := &ParserGo{}

		pg.PackageName = pkgName
		pg.ComponentType = compTypeName
		pg.DataType = pg.ComponentType + "Data"
		pg.OutDir = p.pkgPath
		pg.OutFile = goFileName

		// add to our list of names to check after
		namesToCheck = append(namesToCheck, pg.ComponentType)
		namesToCheck = append(namesToCheck, pg.ComponentType+".NewData")
		namesToCheck = append(namesToCheck, pg.DataType)
		namesToCheck = append(namesToCheck, pg.ComponentType+".GetBackgroundUpdater")
		namesToCheck = append(namesToCheck, pg.ComponentType+".GetStarter")
		namesToCheck = append(namesToCheck, pg.ComponentType+".GetEnder")
		namesToCheck = append(namesToCheck, pg.ComponentType+".GetCapabilityChecker")

		// read in source
		b, err := ioutil.ReadFile(filepath.Join(p.pkgPath, fn))
		if err != nil {
			return err
		}

		// parse it
		err = pg.Parse(bytes.NewReader(b), false)
		if err != nil {
			return fmt.Errorf("error parsing %q: %v", fn, err)
		}

	}

	// after the code generation is done, check the package for the various names in question to see
	// what we need to generate
	namesFound, err := goPkgCheckNames(p.pkgPath, namesToCheck)
	if err != nil {
		return err
	}

	// if main package, generate main_wasm.go with default stuff if no main func in the package and no main_wasm.go
	// note that we need to compute the main package in the face of different root types
	if (!p.opts.SkipMainGo) && pkgName == "main" {

		mainGoPath := filepath.Join(p.pkgPath, "main_wasm.go")
		// log.Printf("namesFound: %#v", namesFound)
		// log.Printf("maingo found: %v", fileExists(mainGoPath))
		// if _, ok := namesFound["main"]; (!ok) && !fileExists(mainGoPath) {

		// NOTE: For now we're disabling the "main" symbol name check, because in single-dir cases
		// it's picking up the main_wasm.go in server.go (even though it's excluded via build tag).  This
		// needs some more thought but for now this will work for the common cases.
		if !fileExists(mainGoPath) {

			// log.Printf("WRITING TO main_wasm.go STUFF")

			//get all of main_wasm.go into a string
			mainstr := `// +build wasm
package main

import (
	"log"
	"os"
	"syscall/js"

	"github.com/iansmith/vugu"
)

func main() {

	println("Entering main()")

	instance, err := vugu.New(&$$ROOTNAME$${}, nil) //my main component
	if err != nil {
		log.Fatal(err)
	}

	defer func() {
		ender := instance.Type.GetEnder()
		if ender != nil {
			ender.End()
		}
		println("Exiting main()")
	}()

	//
	// CapabilityCheck
	//
	checker := instance.Type.GetCapabilityChecker()
	if checker != nil {
		g := js.Global()
		//N.B. we definitely need some clean interface in go
		//to using web workers... something goroutines and channels...
		worker := g.Get("vugu_detectWebWorker")
		html5 := g.Get("vugu_detectHtml5")
		webgl := g.Get("vugu_detectWebGL")
		html5Result := html5.Invoke().Bool()
		webglResult := webgl.Invoke().Bool()
		workerResult := worker.Invoke().Bool()
		page:=checker.CapabilityCheck(html5Result, webglResult, workerResult)
		log.Printf("page from cc %s",page)
		if page!="" {
			g.Get("location").Set("href",page)
		}

	}
	//
	// BackgroundUpdater
	//
	backgrounder := instance.Type.GetBackgroundUpdater()
	var readyChannel chan interface{}
	if backgrounder != nil {
		readyChannel = make(chan interface{})
		backgrounder.BackgroundInit(readyChannel)
	}

	env := vugu.NewJSEnv("#root_mount_parent", instance, vugu.RegisteredComponentTypes())
	env.DebugWriter = os.Stdout
	if err:=env.Render(); err!=nil {
		panic(err)
	}

	//
	// Starter
	//
	starter := instance.Type.GetStarter()
	if starter!=nil {
		starter.Start()
	}

	for {
		f, payload:=env.EventWait(readyChannel)
		if f==vugu.Failed {
			log.Printf("received notification from EventWait that we should bail out")
			return
		}
		switch f {
		case vugu.BackgroundClosed:
			continue //this is a noop from a rendering standpoint
		case vugu.DOMFired:
			//nothing to do just drop into render
		case vugu.BackgroundUpdate:
			if backgrounder == nil {
				panic("background event but nobody to send it to")
			}
			backgrounder.Update(payload)
		}
		err = env.Render()
		if err != nil {
			panic(err)
		}
	}
}
`
			rootType := p.opts.RootTypeName
			if p.opts.RootTypeName == "" {
				rootType = "Root" //this is to allow people to call this with a zero-valued p.opts
			}
			mainstr = strings.Replace(mainstr, "$$ROOTNAME$$", rootType, 1)
			err := ioutil.WriteFile(mainGoPath, []byte(mainstr), 0644)
			if err != nil {
				return err
			}

		}

	}

	// write go.mod if it doesn't exist and not disabled - actually this really only makes sense for main,
	// otherwise we really don't know what the right module name is
	goModPath := filepath.Join(p.pkgPath, "go.mod")
	if pkgName == "main" && !p.opts.SkipGoMod && !fileExists(goModPath) {
		err := ioutil.WriteFile(goModPath, []byte(`module `+pkgName+"\n"), 0644)
		if err != nil {
			return err
		}
	}

	for _, fn := range vuguFileNames {
		goFileName := strings.TrimSuffix(fn, ".vugu") + ".go"
		goFilePath := filepath.Join(p.pkgPath, goFileName)

		err := func() error {
			// get ready to append to file
			f, err := os.OpenFile(goFilePath, os.O_WRONLY|os.O_APPEND, 0644)
			if err != nil {
				return err
			}
			defer f.Close()

			compTypeName := fnameToGoTypeName(goFileName)

			// create CompName struct if it doesn't exist in the package
			if _, ok := namesFound[compTypeName]; !ok {
				fmt.Fprintf(f, "\ntype %s struct {}\n", compTypeName)
			}

			// check for fooData type
			if _, ok := namesFound[compTypeName+"Data"]; !ok {
				fmt.Fprintf(f, "\ntype %s struct {}\n", compTypeName+"Data")
			}

			// create CompName.NewData with defaults if it doesn't exist in the package
			if _, ok := namesFound[compTypeName+".NewData"]; !ok {
				fmt.Fprintf(f, "\nfunc (ct *%s) NewData(props vugu.Props) (interface{}, error) { return &%s{}, nil }\n",
					compTypeName, compTypeName+"Data")
			}

			if _, ok := namesFound[compTypeName+".GetEnder"]; !ok {
				fmt.Fprintf(f, "\nfunc (c *%s) GetEnder() vugu.Ender {return nil}\n", compTypeName)
			}
			if _, ok := namesFound[compTypeName+".GetStarter"]; !ok {
				fmt.Fprintf(f, "\nfunc (c *%s) GetStarter() vugu.Starter {return nil}\n", compTypeName)
			}

			if _, ok := namesFound[compTypeName+".GetCapabilityChecker"]; !ok {
				fmt.Fprintf(f, "\nfunc (c *%s) GetCapabilityChecker() vugu.CapabilityChecker {return nil}\n", compTypeName)
			}
			if _, ok := namesFound[compTypeName+".GetBackgroundUpdater"]; !ok {
				fmt.Fprintf(f, "\nfunc (c *%s) GetBackgroundUpdater() vugu.BackgroundUpdater {return nil}\n", compTypeName)
			}

			// register component unless disabled
			if !p.opts.SkipRegisterComponentTypes && !fileHasInitFunc(goFilePath) {
				fmt.Fprintf(f, "\nfunc init() { vugu.RegisterComponentType(%q, &%s{}) }\n", strings.TrimSuffix(goFileName, ".go"), compTypeName)
			}

			return nil
		}()
		if err != nil {
			return err
		}

	}

	err = restoreFileHashTimes(p.pkgPath, hashTimes)
	if err != nil {
		return err
	}

	return nil

}

func fileHasInitFunc(p string) bool {
	b, err := ioutil.ReadFile(p)
	if err != nil {
		return false
	}
	// hacky but workable for now
	return regexp.MustCompile(`^func init\(`).Match(b)
}

func fileExists(p string) bool {
	_, err := os.Stat(p)
	return !os.IsNotExist(err)
}

func fnameToGoTypeName(s string) string {
	s = strings.Split(s, ".")[0] // remove file extension if present
	parts := strings.Split(s, "-")
	for i := range parts {
		p := parts[i]
		if len(p) > 0 {
			p = strings.ToUpper(p[:1]) + p[1:]
		}
		parts[i] = p
	}
	return strings.Join(parts, "")
}

func goGuessPkgName(pkgPath string, rootName string) (ret string) {

	// defer func() { log.Printf("goGuessPkgName returning %q", ret) }()

	// see if the package already has a name and use it if so
	fset := token.NewFileSet()
	pkgs, err := parser.ParseDir(fset, pkgPath, nil, parser.PackageClauseOnly) // just get the package name
	if err != nil {
		goto checkMore
	}
	if len(pkgs) != 1 {
		goto checkMore
	}
	{
		var pkg *ast.Package
		for _, pkg1 := range pkgs {
			pkg = pkg1
		}
		return pkg.Name
	}

checkMore:

	// check for a root.vugu file, in which case we assume "main"
	_, err = os.Stat(filepath.Join(pkgPath, "root.vugu"))
	if err == nil {
		return "main"
	}

	//if they supplied a root type, we look for a package of that name
	if rootName != "" && rootName != "Root" {
		lowerName := strings.ToLower(rootName)
		_, err = os.Stat(filepath.Join(pkgPath, lowerName+".vugu"))
		if err == nil {
			return "main"
		}
	}

	// otherwise we use the name of the folder...
	dirBase := filepath.Base(pkgPath)
	if regexp.MustCompile(`^[a-z0-9]+$`).MatchString(dirBase) {
		return dirBase
	}

	// ...unless it makes no sense in which case we use "main"

	return "main"

}

// goPkgCheckNames parses a package dir and looks for names, returning a map of what was
// found.  Names like "A.B" mean a method of name "B" with receiver of type "*A"
// (so we can check for existence of a "NewData" method and whatever else)
func goPkgCheckNames(pkgPath string, names []string) (map[string]interface{}, error) {

	ret := make(map[string]interface{})

	fset := token.NewFileSet()
	pkgs, err := parser.ParseDir(fset, pkgPath, nil, 0)
	if err != nil {
		return ret, err
	}

	if len(pkgs) != 1 {
		return ret, fmt.Errorf("unexpected package count after parsing, expected 1 and got this: %#v", pkgs)
	}

	var pkg *ast.Package
	for _, pkg1 := range pkgs {
		pkg = pkg1
	}

	for _, file := range pkg.Files {

		if file.Scope != nil {
			for _, n := range names {
				if v, ok := file.Scope.Objects[n]; ok {
					ret[n] = v
				}
			}
		}

		// log.Printf("file: %#v", file)
		// log.Printf("file.Scope.Objects: %#v", file.Scope.Objects)
		// log.Printf("next: %#v", file.Scope.Objects["Example1"])
		// e1 := file.Scope.Objects["Example1"]
		// if e1.Kind == ast.Typ {
		// e1.Decl
		// }
		for _, d := range file.Decls {
			if fd, ok := d.(*ast.FuncDecl); ok {

				var drecv, dmethod string
				if fd.Recv != nil {
					for _, f := range fd.Recv.List {
						// log.Printf("f.Type: %#v", f.Type)
						if tstar, ok := f.Type.(*ast.StarExpr); ok {
							// log.Printf("tstar.X: %#v", tstar.X)
							if tstarXi, ok := tstar.X.(*ast.Ident); ok && tstarXi != nil {
								// log.Printf("namenamenamename: %#v", tstarXi.Name)
								drecv = tstarXi.Name
							}
						}
						// log.Printf("f.Names: %#v", f.Names)
						// for _, fn := range f.Names {
						// 	if fn != nil {
						// 		log.Printf("NAMENAME: %#v", fn.Name)
						// 		if fni, ok := fn.Name.(*ast.Ident); ok && fni != nil {
						// 		}
						// 	}
						// }

					}
				} else {
					continue // don't care methods with no receiver - found them already above as single (no period) names
				}

				//log.Printf("fd.Name: %#v", fd.Name)
				if fd.Name != nil {
					dmethod = fd.Name.Name
				}

				for _, n := range names {
					recv, method := nameParts(n)
					if drecv == recv && dmethod == method {
						ret[n] = d
					}
				}
			}
		}
	}
	// log.Printf("Objects: %#v", pkg.Scope.Objects)

	return ret, nil
}

func nameParts(n string) (recv, method string) {

	ret := strings.SplitN(n, ".", 2)
	if len(ret) < 2 {
		method = n
		return
	}
	recv = ret[0]
	method = ret[1]
	return
}

// fileHashTimes will scan a directory and return a map of hashes and corresponding mod times
func fileHashTimes(dir string) (map[uint64]time.Time, error) {

	ret := make(map[uint64]time.Time)

	f, err := os.Open(dir)
	if err != nil {
		return nil, err
	}
	defer f.Close()

	fis, err := f.Readdir(-1)
	if err != nil {
		return nil, err
	}
	for _, fi := range fis {
		if fi.IsDir() {
			continue
		}
		h := xxhash.New()
		fmt.Fprint(h, fi.Name()) // hash the name too so we don't confuse different files with the same contents
		b, err := ioutil.ReadFile(filepath.Join(dir, fi.Name()))
		if err != nil {
			return nil, err
		}
		h.Write(b)
		ret[h.Sum64()] = fi.ModTime()
	}

	return ret, nil
}

// restoreFileHashTimes takes the map returned by fileHashTimes and for any files where the hash
// matches we restore the mod time - this way we can clobber files during code generation but
// then if the resulting output is byte for byte the same we can just change the mod time back and
// things that look at timestamps will see the file as unchanged; somewhat hacky, but simple and
// workable for now - it's important for the developer experince we don't do unnecessary builds
// in cases where things don't change
func restoreFileHashTimes(dir string, hashTimes map[uint64]time.Time) error {

	f, err := os.Open(dir)
	if err != nil {
		return err
	}
	defer f.Close()

	fis, err := f.Readdir(-1)
	if err != nil {
		return err
	}
	for _, fi := range fis {
		if fi.IsDir() {
			continue
		}
		fiPath := filepath.Join(dir, fi.Name())
		h := xxhash.New()
		fmt.Fprint(h, fi.Name()) // hash the name too so we don't confuse different files with the same contents
		b, err := ioutil.ReadFile(fiPath)
		if err != nil {
			return err
		}
		h.Write(b)
		if t, ok := hashTimes[h.Sum64()]; ok {
			err := os.Chtimes(fiPath, time.Now(), t)
			if err != nil {
				log.Printf("Error in os.Chtimes(%q, now, %q): %v", fiPath, t, err)
			}
		}
	}

	return nil
}
