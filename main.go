package main

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"flag"
	"fmt"
	"io"
	"log"
	"os"
	"strings"

	"github.com/mjl-/sconf"
	"golang.org/x/mod/sumdb/note"
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
		if err := sconf.Describe(os.Stdout, &config); err != nil {
			log.Fatalf("describing config: %v", err)
		}
	case "testconfig":
		if len(args) != 1 {
			usage()
		}
		if err := parseConfig(args[0], &config); err != nil {
			log.Fatalf("parsing config file: %v", err)
		}
		log.Printf("config OK")
	case "serve":
		serve(args)
	case "genkey":
		if len(args) != 1 {
			usage()
		}

		if skey, vkey, err := note.GenerateKey(rand.Reader, args[0]); err != nil {
			log.Fatalf("generating key: %v", err)
		} else {
			log.Printf("Signer key: %s", skey)
			log.Printf("Verifier key: %s", vkey)
			log.Printf(`Configure the signer key in your server config file, and use the verifier key with the "get" subcommand.`)
		}
	case "get":
		get(args)
	case "sum":
		if len(args) != 0 {
			usage()
		}
		sha := sha256.New()
		if _, err := io.Copy(sha, os.Stdin); err != nil {
			log.Fatalf("read: %v", err)
		} else if _, err := fmt.Println("0" + base64.RawURLEncoding.EncodeToString(sha.Sum(nil)[:20])); err != nil {
			log.Fatalf("write: %v", err)
		}
	}
}

func parseConfig(p string, c *Config) error {
	if err := sconf.ParseFile(p, c); err != nil {
		return err
	}
	for i, cp := range c.BadClients {
		cp.UserAgent = strings.ToLower(cp.UserAgent)
		cp.HostnameSuffix = strings.ToLower(cp.HostnameSuffix)
		c.BadClients[i] = cp
	}
	return nil
}
