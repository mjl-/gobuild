// Gobuild serves reproducibly built binaries from sources via go module proxy.
//
// Serves URLs like:
//
// 	http://localhost:8000/github.com/mjl-/sherpa@v0.6.0/cmd/sherpaclient/linux-amd64-go1.13
// 	http://localhost:8000/github.com/mjl-/sherpa@v0.6.0/cmd/sherpaclient/linux-amd64-go1.13.{log,sha256}
package main

import (
	"crypto/sha256"
	"flag"
	"fmt"
	"io"
	"io/ioutil"
	"log"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
)

var (
	address = flag.String("address", "localhost:8000", "address to serve on")
	workdir string
)

func main() {
	log.SetFlags(0)
	flag.Usage = func() {
		log.Println("usage: gobuild [flags]")
		flag.PrintDefaults()
	}
	flag.Parse()
	args := flag.Args()
	if len(args) != 0 {
		flag.Usage()
		os.Exit(2)
	}

	var err error
	workdir, err = os.Getwd()
	if err != nil {
		log.Fatalln("getwd:", err)
	}

	http.HandleFunc("/", serve)
	log.Println("listening on", *address)
	log.Fatalln(http.ListenAndServe(*address, nil))
}

type request struct {
	Mod       string
	Version   string
	Dir       string
	Goos      string
	Goarch    string
	Goversion string
	Log       bool
	Sum       bool
}

func (r request) basepath() string {
	return fmt.Sprintf("%s@%s/%s/%s-%s-%s", r.Mod, r.Version, r.Dir, r.Goos, r.Goarch, r.Goversion)
}

func (r request) path() string {
	p := r.basepath()
	if r.Log {
		p += ".log"
	} else if r.Sum {
		p += ".sha256"
	}
	return p
}

func parsePath(s string) (r request, ok bool) {
	t := strings.Split(s, "/")
	if len(t) < 2 {
		return
	}
	last := t[len(t)-1]
	t = t[:len(t)-1]

	if strings.HasSuffix(last, ".log") {
		r.Log = true
		last = strings.TrimSuffix(last, ".log")
	} else if strings.HasSuffix(last, ".sha256") {
		r.Sum = true
		last = strings.TrimSuffix(last, ".sha256")
	}
	lt := strings.Split(last, "-")
	if len(lt) != 3 {
		return
	}
	r.Goos = lt[0]
	r.Goarch = lt[1]
	r.Goversion = lt[2]
	if strings.Contains(r.Goversion, "/") {
		return
	}

	for i, v := range t {
		xt := strings.SplitN(v, "@", 2)
		if len(xt) == 2 {
			t[i] = xt[0]
			r.Mod = strings.Join(t[:i+1], "/")
			r.Version = xt[1]
			r.Dir = strings.Join(t[i+1:], "/")
			return r, true
		}
	}
	return
}

func serve(w http.ResponseWriter, r *http.Request) {
	failf := func(format string, args ...interface{}) {
		msg := fmt.Sprintf(format, args...)
		log.Println(msg)
		http.Error(w, "500 - "+msg, http.StatusInternalServerError)
	}

	req, ok := parsePath(r.URL.Path[1:])
	if !ok {
		http.NotFound(w, r)
		return
	}

	lpath := "data/" + req.path()
	f, err := os.Open(lpath)
	if err == nil {
		defer f.Close()
		if req.Log || req.Sum {
			w.Header().Set("Content-Type", "text/plain; charset=utf-8")
		}
		io.Copy(w, f)
		return
	}
	if !os.IsNotExist(err) {
		failf("stat path: %v", err)
		return
	}

	log.Printf("building %#v", req)

	gobin := fmt.Sprintf("%s/sdk/%s/bin/go", workdir, req.Goversion)
	_, err = os.Stat(gobin)
	if err != nil {
		failf("unknown toolchain %q: %v", req.Goversion, err)
		return
	}

	dir, err := ioutil.TempDir("", "gobuild")
	if err != nil {
		failf("tempdir for build: %v", err)
		return
	}
	defer os.RemoveAll(dir)

	homedir := workdir + "/home"
	os.Mkdir(homedir, 0777)

	cmd := exec.CommandContext(r.Context(), gobin, "get", req.Mod+"@"+req.Version)
	cmd.Dir = dir
	cmd.Env = []string{
		"GOPROXY=https://proxy.golang.org",
		"GO111MODULE=on",
		"HOME=" + homedir,
	}
	getOutput, err := cmd.CombinedOutput()
	if err != nil {
		log.Printf("output: %s", string(getOutput))
		failf("fetching module: %v", err)
		return
	}

	var name string
	if req.Dir != "" {
		t := strings.Split(req.Dir, "/")
		name = t[len(t)-1]
	} else {
		t := strings.Split(req.Mod, "/")
		name = t[len(t)-1]
	}
	lname := dir + "/bin/" + name
	os.Mkdir(filepath.Dir(lname), 0777)
	cmd = exec.CommandContext(r.Context(), gobin, "build", "-o", lname, "-x", "-trimpath", "-ldflags", "-buildid=00000000000000000000/00000000000000000000/00000000000000000000/00000000000000000000")
	cmd.Env = []string{
		"CGO_ENABLED=0",
		"GOOS=" + req.Goos,
		"GOARCH=" + req.Goarch,
		"HOME=" + homedir,
	}
	cmd.Dir = homedir + "/go/pkg/mod/" + req.Mod + "@" + req.Version
	if req.Dir != "" {
		cmd.Dir += "/" + req.Dir
	}
	output, err := cmd.CombinedOutput()
	if err != nil {
		failf("running build: %v", err)
		return
	}

	err = saveFiles(req, output, lname)
	if err != nil {
		failf("writing resulting files: %v", err)
		return
	}

	http.ServeFile(w, r, "data/"+req.path())
}

func saveFiles(req request, logOutput []byte, lname string) error {
	base := req.basepath()
	logpath := "data/" + base + ".log"
	sumpath := "data/" + base + ".sha256"
	binpath := "data/" + base

	success := false
	defer func() {
		if !success {
			os.Remove(logpath)
			os.Remove(sumpath)
			os.Remove(binpath)
		}
	}()
	os.MkdirAll(filepath.Dir("data/"+base), 0777)

	err := ioutil.WriteFile(logpath, logOutput, 0666)
	if err != nil {
		return err
	}

	of, err := os.Open(lname)
	if err != nil {
		return err
	}
	defer of.Close()

	h := sha256.New()
	_, err = io.Copy(h, of)
	if err != nil {
		return err
	}
	err = ioutil.WriteFile(sumpath, []byte(fmt.Sprintf("%x", h.Sum(nil))), 0666)
	if err != nil {
		return err
	}
	_, err = of.Seek(0, io.SeekStart)
	if err != nil {
		return err
	}

	nf, err := os.Create(binpath)
	if err != nil {
		return err
	}
	defer func() {
		if nf != nil {
			nf.Close()
		}
	}()
	_, err = io.Copy(nf, of)
	if err != nil {
		return err
	}
	err = nf.Close()
	if err != nil {
		return err
	}
	nf = nil

	success = true
	return nil
}
