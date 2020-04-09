package main

import (
	"flag"
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
	}
}
