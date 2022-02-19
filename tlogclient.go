package main

import (
	"bytes"
	"fmt"
	"io/ioutil"
	"log"
	"os"
	"path/filepath"
	"strings"

	"github.com/mjl-/gobuild/internal/sumdb"

	"golang.org/x/mod/sumdb/note"
)

type clientOps struct {
	localDir string
	baseURL  string
}

var _ sumdb.ClientOps = (*clientOps)(nil)

func newClient(vkey string, baseURL string) (*sumdb.Client, *clientOps, error) {
	verifier, err := note.NewVerifier(vkey)
	if err != nil {
		return nil, nil, fmt.Errorf("parsing verifier key: %v", err)
	}

	if baseURL == "" {
		name := verifier.Name()
		if strings.Contains(name, ".") {
			baseURL = "https://" + name + "/tlog"
		} else {
			baseURL = "http://" + name + ":8000/tlog"
		}
	}
	baseURL = strings.TrimRight(baseURL, "/")

	dir, err := os.UserCacheDir()
	if err != nil {
		return nil, nil, err
	}
	ops := &clientOps{
		filepath.Join(dir, "gobuild", "sumclient", verifier.Name()),
		baseURL,
	}

	if ovkey, err := ops.ReadConfig("key"); err != nil {
		if !os.IsNotExist(err) {
			return nil, nil, fmt.Errorf("reading verifierkey: %v", err)
		}
		if err := ops.WriteConfig("key", nil, []byte(vkey)); err != nil {
			return nil, nil, fmt.Errorf("writing verifierkey: %v", err)
		}
	} else {
		if vkey != string(ovkey) {
			return nil, nil, fmt.Errorf("different key for name in verifierkey, new %s, old %s", vkey, string(ovkey))
		}
	}

	return sumdb.NewClient(ops), ops, nil
}

func (c *clientOps) ReadRemote(path string) ([]byte, error) {
	// log.Printf("client: ReadRemote %s", path)

	resp, err := httpGet(c.baseURL + path)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		return nil, fmt.Errorf("http get: %v", resp.Status)
	}
	return ioutil.ReadAll(resp.Body)
}

// ReadConfig reads and returns the content of the named configuration file.
// There are only a fixed set of configuration files.
//
// "key" returns a file containing the verifier key for the server.
//
// serverName + "/latest" returns a file containing the latest known
// signed tree from the server.
// To signal that the client wishes to start with an "empty" signed tree,
// ReadConfig can return a successful empty result (0 bytes of data).
func (c *clientOps) ReadConfig(file string) ([]byte, error) {
	// log.Printf("client: ReadConfig %s", file)

	p := filepath.Join(c.localDir, "config", file)
	buf, err := ioutil.ReadFile(p)
	if err != nil && os.IsNotExist(err) && strings.HasSuffix(file, "/latest") {
		return nil, nil
	}
	return buf, err
}

// WriteConfig updates the content of the named configuration file,
// changing it from the old []byte to the new []byte.
// If the old []byte does not match the stored configuration,
// WriteConfig must return ErrWriteConflict.
// Otherwise, WriteConfig should atomically replace old with new.
// The "key" configuration file is never written using WriteConfig.
func (c *clientOps) WriteConfig(file string, old, new []byte) error {
	// log.Printf("client: WriteConfig %s", file)

	p := filepath.Join(c.localDir, "config", file)
	if old != nil {
		cur, err := c.ReadConfig(file)
		if err != nil {
			return fmt.Errorf("reading config: %v", err)
		}
		if !bytes.Equal(cur, old) {
			return sumdb.ErrWriteConflict
		}
	}
	os.MkdirAll(filepath.Dir(p), 0777)
	return ioutil.WriteFile(p, new, 0666)
}

// ReadCache reads and returns the content of the named cache file.
// Any returned error will be treated as equivalent to the file not existing.
// There can be arbitrarily many cache files, such as:
//      serverName/lookup/pkg@version
//      serverName/tile/8/1/x123/456
func (c *clientOps) ReadCache(file string) ([]byte, error) {
	// log.Printf("client: Readcache %s", file)

	p := filepath.Join(c.localDir, "cache", file)
	return ioutil.ReadFile(p)
}

// WriteCache writes the named cache file.
func (c *clientOps) WriteCache(file string, data []byte) {
	// log.Printf("client: WriteCache %s", file)

	p := filepath.Join(c.localDir, "cache", file)
	os.MkdirAll(filepath.Dir(p), 0777)
	if err := ioutil.WriteFile(p, data, 0666); err != nil {
		// todo: should be able to return errors
		panic(fmt.Sprintf("write failed: %v", err))
	}
}

// Log prints the given log message (such as with log.Print)
func (c *clientOps) Log(msg string) {
	log.Println(msg)
}

// SecurityError prints the given security error log message.
// The Client returns ErrSecurity from any operation that invokes SecurityError,
// but the return value is mainly for testing. In a real program,
// SecurityError should typically print the message and call log.Fatal or os.Exit.
func (c *clientOps) SecurityError(msg string) {
	log.Fatalln(msg)
}
