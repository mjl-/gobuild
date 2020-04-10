package main

import (
	"crypto/sha256"
	"encoding/base64"
	"flag"
	"fmt"
	"io"
	"log"
	"os"

	"github.com/mjl-/sconf"
)

func usage() {
	log.Println("usage: gobuild config")
	log.Println("       gobuild testconfig gobuild.conf")
	log.Println("       gobuild serve [flags] [gobuild.conf]")
	log.Println("       gobuild genkey name")
	log.Println("       gobuild get [flags] module[@version/package]")
	log.Println("       gobuild sum < file")
	flag.PrintDefaults()
	os.Exit(2)
}

func main() {
	log.SetFlags(0)
	flag.Usage = usage
	flag.Parse()
	args := flag.Args()
	if len(args) == 0 {
		usage()
	}
	cmd, args := args[0], args[1:]
	switch cmd {
	default:
		usage()

	case "config":
		if len(args) != 0 {
			usage()
		}
		err := sconf.Describe(os.Stdout, &config)
		if err != nil {
			log.Fatalf("describing config: %v", err)
		}
	case "testconfig":
		if len(args) != 1 {
			usage()
		}
		err := sconf.ParseFile(args[0], &config)
		if err != nil {
			log.Fatalf("parsing config file: %v", err)
		}
		log.Printf("config OK")
	case "serve":
		serve(args)
	case "genkey":
		genkey(args)
	case "get":
		get(args)
	case "sum":
		if len(args) != 0 {
			usage()
		}
		sha := sha256.New()
		if _, err := io.Copy(sha, os.Stdin); err != nil {
			log.Fatalf("read: %v", err)
		}
		if _, err := fmt.Println("0" + base64.RawURLEncoding.EncodeToString(sha.Sum(nil)[:20])); err != nil {
			log.Fatalf("write: %v", err)
		}
	}
}
