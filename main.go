package main

import (
	"flag"
	"log"
	"os"

	"github.com/mjl-/sconf"
)

func main() {
	log.SetFlags(0)
	flag.Usage = func() {
		log.Println("usage: gobuild [flags] { config | testconfig | serve }")
		log.Println("       gobuild config")
		log.Println("       gobuild testconfig gobuild.conf")
		log.Println("       gobuild serve [gobuild.conf]")
		flag.PrintDefaults()
	}
	flag.Parse()
	args := flag.Args()
	if len(args) == 0 {
		flag.Usage()
		os.Exit(2)
	}
	cmd, args := args[0], args[1:]
	switch cmd {
	case "config":
		err := sconf.Describe(os.Stdout, &config)
		if err != nil {
			log.Fatalf("describing config: %v", err)
		}
	case "testconfig":
		if len(args) != 1 {
			flag.Usage()
			os.Exit(2)
		}
		err := sconf.ParseFile(args[0], &config)
		if err != nil {
			log.Fatalf("parsing config file: %v", err)
		}
		log.Printf("config OK")
	case "serve":
		serve(args)
	default:
		flag.Usage()
		os.Exit(2)
	}
}
